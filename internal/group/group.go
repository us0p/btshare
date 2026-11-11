// Package group keeps the host and device lists in sync across computers.
package group

import (
	"context"
	"fmt"
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
func Join(ctx context.Context, cfgPath, peerAddr string) (*config.Config, []Warning, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("join %s: %w", peerAddr, err)
	}

	cfg, err = config.Update(cfgPath, func(c *config.Config) error {
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

// AddDevice appends the device locally and on every other host.
func AddDevice(ctx context.Context, cfgPath string, d config.Device) ([]Warning, error) {
	if err := config.ValidName(d.Name); err != nil {
		return nil, err
	}
	mac, err := config.ValidMAC(d.MAC)
	if err != nil {
		return nil, err
	}
	d.MAC = mac
	cfg, err := config.Update(cfgPath, func(c *config.Config) error {
		for _, e := range c.Devices {
			if e.MAC == d.MAC && e.Name != d.Name {
				return fmt.Errorf("%s is already shared as %q", d.MAC, e.Name)
			}
		}
		c.UpsertDevice(d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var warns []Warning
	for _, h := range cfg.Hosts {
		if h.Name == cfg.Self {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, Timeout)
		err := remote(cfg, h.Addr).AddDevice(cctx, d)
		cancel()
		if err != nil {
			warns = append(warns, Warning{h.Name, err})
		}
	}
	return warns, nil
}
