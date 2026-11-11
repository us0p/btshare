// Package bluez is a thin wrapper around the BlueZ D-Bus API (org.bluez)
// exposing exactly the operations btshare needs: inspect, release and
// acquire a paired device.
package bluez

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	service     = "org.bluez"
	ifaceDevice = "org.bluez.Device1"
	ifaceAdapt  = "org.bluez.Adapter1"
	ifaceProps  = "org.freedesktop.DBus.Properties"
	ifaceObjMgr = "org.freedesktop.DBus.ObjectManager"
)

// ErrUnknownDevice means the device is not paired with this host's adapter.
var ErrUnknownDevice = errors.New("device is not paired with this host (see `btshare keys`)")

// DeviceState is the observable state of one device on one host.
type DeviceState struct {
	MAC       string `json:"mac"`
	Known     bool   `json:"known"`
	Name      string `json:"name,omitempty"`
	Paired    bool   `json:"paired"`
	Connected bool   `json:"connected"`
	Blocked   bool   `json:"blocked"`
}

// Client talks to BlueZ on the system bus for one adapter (e.g. "hci0").
type Client struct {
	conn    *dbus.Conn
	adapter string
}

// New connects to the system bus. adapter defaults to "hci0".
func New(adapter string) (*Client, error) {
	if adapter == "" {
		adapter = "hci0"
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect system bus: %w", err)
	}
	return &Client{conn: conn, adapter: adapter}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) adapterPath() dbus.ObjectPath {
	return dbus.ObjectPath("/org/bluez/" + c.adapter)
}

func (c *Client) devicePath(mac string) dbus.ObjectPath {
	return dbus.ObjectPath(fmt.Sprintf("%s/dev_%s", c.adapterPath(),
		strings.ReplaceAll(strings.ToUpper(mac), ":", "_")))
}

func (c *Client) obj(p dbus.ObjectPath) dbus.BusObject { return c.conn.Object(service, p) }

func (c *Client) getProp(p dbus.ObjectPath, iface, name string) (dbus.Variant, error) {
	var v dbus.Variant
	err := c.obj(p).Call(ifaceProps+".Get", 0, iface, name).Store(&v)
	return v, err
}

func (c *Client) setProp(p dbus.ObjectPath, iface, name string, val any) error {
	return c.obj(p).Call(ifaceProps+".Set", 0, iface, name, dbus.MakeVariant(val)).Err
}

// AdapterAddress returns the BD_ADDR of the adapter.
func (c *Client) AdapterAddress() (string, error) {
	v, err := c.getProp(c.adapterPath(), ifaceAdapt, "Address")
	if err != nil {
		return "", fmt.Errorf("adapter %s: %w", c.adapter, err)
	}
	s, _ := v.Value().(string)
	return s, nil
}

func (c *Client) ensurePowered() error {
	v, err := c.getProp(c.adapterPath(), ifaceAdapt, "Powered")
	if err != nil {
		return fmt.Errorf("adapter %s: %w", c.adapter, err)
	}
	if on, _ := v.Value().(bool); on {
		return nil
	}
	return c.setProp(c.adapterPath(), ifaceAdapt, "Powered", true)
}

// State reports the device state. Unknown devices are returned with Known=false.
func (c *Client) State(mac string) (DeviceState, error) {
	st := DeviceState{MAC: strings.ToUpper(mac)}
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := c.obj("/").Call(ifaceObjMgr+".GetManagedObjects", 0).Store(&objs); err != nil {
		return st, fmt.Errorf("query bluez: %w", err)
	}
	props, ok := objs[c.devicePath(mac)][ifaceDevice]
	if !ok {
		return st, nil
	}
	st.Known = true
	st.Name, _ = props["Alias"].Value().(string)
	st.Paired, _ = props["Paired"].Value().(bool)
	st.Connected, _ = props["Connected"].Value().(bool)
	st.Blocked, _ = props["Blocked"].Value().(bool)
	return st, nil
}

// Release disconnects the device and blocks it so it cannot auto-reconnect
// to this host. The pairing keys are kept.
func (c *Client) Release(mac string) (DeviceState, error) {
	st, err := c.State(mac)
	if err != nil || !st.Known {
		// Nothing to release on a host that doesn't know the device.
		return st, err
	}
	p := c.devicePath(mac)
	if st.Connected {
		if err := c.obj(p).Call(ifaceDevice+".Disconnect", 0).Err; err != nil {
			return st, fmt.Errorf("disconnect: %w", err)
		}
	}
	if err := c.setProp(p, ifaceDevice, "Blocked", true); err != nil {
		return st, fmt.Errorf("block: %w", err)
	}
	return c.State(mac)
}

// Acquire unblocks, trusts and connects the device, retrying until ctx expires
// (the device may need a moment to notice the previous host let go).
func (c *Client) Acquire(ctx context.Context, mac string) (DeviceState, error) {
	if err := c.ensurePowered(); err != nil {
		return DeviceState{}, err
	}
	st, err := c.State(mac)
	if err != nil {
		return st, err
	}
	if !st.Known || !st.Paired {
		return st, ErrUnknownDevice
	}
	p := c.devicePath(mac)
	if st.Blocked {
		if err := c.setProp(p, ifaceDevice, "Blocked", false); err != nil {
			return st, fmt.Errorf("unblock: %w", err)
		}
	}
	if err := c.setProp(p, ifaceDevice, "Trusted", true); err != nil {
		return st, fmt.Errorf("trust: %w", err)
	}

	var lastErr error
	for {
		if st, err = c.State(mac); err == nil && st.Connected {
			return st, nil
		}
		call := c.obj(p).CallWithContext(ctx, ifaceDevice+".Connect", 0)
		if call.Err == nil {
			return c.State(mac)
		}
		lastErr = call.Err
		select {
		case <-ctx.Done():
			return st, fmt.Errorf("connect timed out (is the device awake? try moving it): %w", lastErr)
		case <-time.After(750 * time.Millisecond):
		}
	}
}

// PairedDevice is a device bonded with this adapter.
type PairedDevice struct {
	MAC  string
	Name string
}

// Paired lists the devices paired with this adapter.
func (c *Client) Paired() ([]PairedDevice, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := c.obj("/").Call(ifaceObjMgr+".GetManagedObjects", 0).Store(&objs); err != nil {
		return nil, fmt.Errorf("query bluez: %w", err)
	}
	prefix := string(c.adapterPath()) + "/dev_"
	var out []PairedDevice
	for p, ifaces := range objs {
		props, ok := ifaces[ifaceDevice]
		if !ok || !strings.HasPrefix(string(p), prefix) {
			continue
		}
		if paired, _ := props["Paired"].Value().(bool); !paired {
			continue
		}
		d := PairedDevice{}
		d.MAC, _ = props["Address"].Value().(string)
		d.Name, _ = props["Alias"].Value().(string)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
