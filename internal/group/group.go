// Package group keeps the host and device lists in sync across computers.
package group

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/us0p/btshare/internal/config"
	"github.com/us0p/btshare/internal/node"
)

var Timeout = 5 * time.Second

// Warning is a non-fatal problem with one peer (usually: it's offline).
type Warning struct {
	Host string
	Err  error
}

func (w Warning) Error() string { return fmt.Sprintf("%s: %v", w.Host, w.Err) }

func remote(cfg *config.Config, addr string) *node.Remote {
	return &node.Remote{Addr: addr, Secret: []byte(cfg.Secret)}
}

// Join registers this host with the agent at peerAddr, appends the peer's
// hosts and devices to the local config, then announces this host to every
// other member so all configs list each other.
//
// secret is the group's secret; "" keeps this host's current one. It is
// saved only once the peer has accepted it, so a mistyped secret changes
// nothing.
func Join(ctx context.Context, cfgPath, peerAddr, secret string) (*config.Config, []Warning, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	if secret != "" {
		if err := config.ValidSecret(secret); err != nil {
			return nil, nil, err
		}
		cfg.Secret = secret
	}
	self, _ := cfg.Host(cfg.Self)
	if self.Addr == peerAddr {
		return nil, nil, fmt.Errorf("%s is this computer's own address", peerAddr)
	}
	req := node.JoinRequest{Host: self, Devices: cfg.Devices}

	cctx, cancel := context.WithTimeout(ctx, Timeout)
	resp, err := remote(cfg, peerAddr).Join(cctx, req)
	cancel()
	if err != nil {
		if strings.Contains(err.Error(), "bad signature") {
			err = fmt.Errorf("%w (the secret doesn't match that computer's; run `btshare invite` there to see it)", err)
		}
		return nil, nil, fmt.Errorf("join %s: %w", peerAddr, err)
	}

	cfg, err = config.Update(cfgPath, func(c *config.Config) error {
		c.Secret = cfg.Secret
		for _, h := range resp.Hosts {
			if h.Name != c.Self {
				c.UpsertHost(h)
			}
		}
		for _, d := range resp.Devices {
			c.UpsertDevice(d)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	var warns []Warning
	for _, h := range resp.Hosts {
		if h.Name == cfg.Self || h.Addr == peerAddr {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, Timeout)
		_, err := remote(cfg, h.Addr).Join(cctx, req)
		cancel()
		if err != nil {
			warns = append(warns, Warning{h.Name, err})
		}
	}
	return cfg, warns, nil
}

// AddHost appends a host to the local config only (manual fallback when the
// peer's agent isn't reachable yet; prefer Join).
func AddHost(cfgPath string, h config.Host) error {
	if err := config.ValidName(h.Name); err != nil {
		return err
	}
	if err := config.ValidAddr(h.Addr); err != nil {
		return err
	}
	_, err := config.Update(cfgPath, func(c *config.Config) error {
		if h.Name == c.Self {
			return fmt.Errorf("%q is this computer", h.Name)
		}
		c.UpsertHost(h)
		return nil
	})
	return err
}

// RemoveDevice stops sharing a device: it is removed from the local config
// and from every other host's. nameOrMAC is matched against the local
// config; a MAC that isn't there locally is still removed from the peers,
// to clean up an entry only they have. Pairings are left alone.
func RemoveDevice(ctx context.Context, cfgPath, nameOrMAC string) (config.Device, []Warning, error) {
	var d config.Device
	cfg, err := config.Update(cfgPath, func(c *config.Config) error {
		if found, err := c.Device(nameOrMAC); err == nil {
			d = found
		} else if mac, merr := config.ValidMAC(nameOrMAC); merr == nil {
			d = config.Device{MAC: mac}
		} else {
			return err
		}
		c.RemoveDevice(d.MAC)
		return nil
	})
	if err != nil {
		return d, nil, err
	}
	var warns []Warning
	for _, h := range cfg.Hosts {
		if h.Name == cfg.Self {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, Timeout)
		err := remote(cfg, h.Addr).RemoveDevice(cctx, d)
		cancel()
		if err != nil {
			warns = append(warns, Warning{h.Name, err})
		}
	}
	return d, warns, nil
}

// AddDevice shares a device: it is added (or updated) locally, and the
// result is sent to every other host.
//
// here is this computer's channel of a multi-channel device, or nil for a
// single-host device. Each computer registers its own channel by running
// `device add` with the device connected; the entries are merged.
func AddDevice(ctx context.Context, cfgPath string, d config.Device, here *config.DeviceHost) (config.Device, []Warning, error) {
	if err := config.ValidName(d.Name); err != nil {
		return d, nil, err
	}
	mac, err := config.ValidMAC(d.MAC)
	if err != nil {
		return d, nil, err
	}
	d.MAC = mac
	var out config.Device
	cfg, err := config.Update(cfgPath, func(c *config.Config) error {
		for _, e := range c.Devices {
			if e.Name != d.Name && (e.HasAddr(d.MAC) || (here != nil && e.HasAddr(here.MAC))) {
				return fmt.Errorf("%s is already shared as %q", d.MAC, e.Name)
			}
		}
		existing, err := c.Device(d.Name)
		switch {
		case err != nil: // new device
			out = d
			if here != nil {
				out.Hosts = []config.DeviceHost{*here}
			}
		case here != nil:
			// Another computer's channel: keep the device's first address.
			out = existing
			out.Hosts = append([]config.DeviceHost(nil), existing.Hosts...)
			if _, err := out.SetHost(*here); err != nil {
				return err
			}
		case existing.MultiChannel():
			return fmt.Errorf("%s has switchable channels, but btshare couldn't read them here; "+
				"connect it to this computer on its own channel and run this again", d.Name)
		default: // single-host device re-paired: new address
			out = d
		}
		for i := range c.Devices {
			if c.Devices[i].Name == out.Name {
				c.Devices[i] = out
				return nil
			}
		}
		c.Devices = append(c.Devices, out)
		return nil
	})
	if err != nil {
		return d, nil, err
	}
	var warns []Warning
	for _, h := range cfg.Hosts {
		if h.Name == cfg.Self {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, Timeout)
		err := remote(cfg, h.Addr).AddDevice(cctx, out)
		cancel()
		if err != nil {
			warns = append(warns, Warning{h.Name, err})
		}
	}
	return out, warns, nil
}
