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
	// Logf, if set, receives a detailed trace of Release and Acquire: device
	// and adapter state, every Connect attempt with BlueZ's exact error, and
	// every property change BlueZ signals for the device meanwhile.
	Logf func(format string, args ...any)
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

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// describe returns every interesting property of the device and the adapter
// as one line, for the trace.
func (c *Client) describe(mac string) string {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := c.obj("/").Call(ifaceObjMgr+".GetManagedObjects", 0).Store(&objs); err != nil {
		return "query bluez: " + err.Error()
	}
	var b strings.Builder
	if a, ok := objs[c.adapterPath()][ifaceAdapt]; ok {
		fmt.Fprintf(&b, "adapter %s %v powered=%v discovering=%v; ", c.adapter,
			a["Address"].Value(), a["Powered"].Value(), a["Discovering"].Value())
	} else {
		fmt.Fprintf(&b, "adapter %s missing; ", c.adapter)
	}
	d, ok := objs[c.devicePath(mac)][ifaceDevice]
	if !ok {
		b.WriteString("device unknown to bluez")
		return b.String()
	}
	b.WriteString("device")
	for _, k := range []string{"AddressType", "Paired", "Bonded", "Trusted", "Blocked",
		"Connected", "ServicesResolved", "WakeAllowed", "RSSI"} {
		if v, ok := d[k]; ok {
			fmt.Fprintf(&b, " %s=%v", k, v.Value())
		}
	}
	return b.String()
}

// watch logs every property change BlueZ signals for the device until the
// returned stop function is called. It uses its own bus connection so the
// signals don't interfere with the client's calls.
func (c *Client) watch(mac string) (stop func()) {
	if c.Logf == nil {
		return func() {}
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		c.logf("watch: connect system bus: %v", err)
		return func() {}
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(c.devicePath(mac)),
		dbus.WithMatchInterface(ifaceProps),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		c.logf("watch: subscribe: %v", err)
		conn.Close()
		return func() {}
	}
	ch := make(chan *dbus.Signal, 64)
	conn.Signal(ch)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sig := range ch {
			if len(sig.Body) < 2 {
				continue
			}
			iface, _ := sig.Body[0].(string)
			changed, _ := sig.Body[1].(map[string]dbus.Variant)
			keys := make([]string, 0, len(changed))
			for k := range changed {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				v := fmt.Sprint(changed[k].Value())
				if len(v) > 60 {
					v = v[:60] + "…"
				}
				parts = append(parts, k+"="+v)
			}
			if len(sig.Body) > 2 {
				if inv, _ := sig.Body[2].([]string); len(inv) > 0 {
					parts = append(parts, "invalidated="+strings.Join(inv, ","))
				}
			}
			c.logf("  bluez %s %s: %s", mac, strings.TrimPrefix(iface, "org.bluez."), strings.Join(parts, " "))
		}
	}()
	return func() {
		conn.Close() // closes ch, ending the goroutine
		<-done
	}
}

// dbusErr formats a D-Bus error with its name, e.g.
// "org.bluez.Error.Failed: le-connection-abort-by-local".
func dbusErr(err error) string {
	var de dbus.Error
	if errors.As(err, &de) {
		return de.Name + ": " + de.Error()
	}
	return err.Error()
}

// Release disconnects the device and blocks it so it cannot auto-reconnect
// to this host. The pairing keys are kept.
func (c *Client) Release(mac string) (DeviceState, error) {
	c.logf("release %s: before: %s", mac, c.describe(mac))
	st, err := c.State(mac)
	if err != nil || !st.Known {
		// Nothing to release on a host that doesn't know the device.
		c.logf("release %s: nothing to do (known=%v err=%v)", mac, st.Known, err)
		return st, err
	}
	defer c.watch(mac)()
	p := c.devicePath(mac)
	if st.Connected {
		t := time.Now()
		if err := c.obj(p).Call(ifaceDevice+".Disconnect", 0).Err; err != nil {
			c.logf("release %s: Disconnect failed after %s: %s", mac, since(t), dbusErr(err))
			return st, fmt.Errorf("disconnect: %w", err)
		}
		c.logf("release %s: Disconnect ok in %s", mac, since(t))
	}
	if err := c.setProp(p, ifaceDevice, "Blocked", true); err != nil {
		c.logf("release %s: block failed: %s", mac, dbusErr(err))
		return st, fmt.Errorf("block: %w", err)
	}
	c.logf("release %s: after: %s", mac, c.describe(mac))
	return c.State(mac)
}

func since(t time.Time) time.Duration { return time.Since(t).Round(10 * time.Millisecond) }

// Acquire unblocks, trusts and connects the device, retrying until ctx expires
// (the device may need a moment to notice the previous host let go).
func (c *Client) Acquire(ctx context.Context, mac string) (DeviceState, error) {
	start := time.Now()
	if dl, ok := ctx.Deadline(); ok {
		c.logf("acquire %s: start, %s to connect", mac, time.Until(dl).Round(time.Second))
	}
	c.logf("acquire %s: before: %s", mac, c.describe(mac))
	if err := c.ensurePowered(); err != nil {
		c.logf("acquire %s: power on adapter: %v", mac, err)
		return DeviceState{}, err
	}
	st, err := c.State(mac)
	if err != nil {
		return st, err
	}
	if !st.Known || !st.Paired {
		c.logf("acquire %s: not paired here (known=%v paired=%v)", mac, st.Known, st.Paired)
		return st, ErrUnknownDevice
	}
	defer c.watch(mac)()
	p := c.devicePath(mac)
	if st.Blocked {
		if err := c.setProp(p, ifaceDevice, "Blocked", false); err != nil {
			c.logf("acquire %s: unblock failed: %s", mac, dbusErr(err))
			return st, fmt.Errorf("unblock: %w", err)
		}
		c.logf("acquire %s: unblocked", mac)
	}
	if err := c.setProp(p, ifaceDevice, "Trusted", true); err != nil {
		c.logf("acquire %s: trust failed: %s", mac, dbusErr(err))
		return st, fmt.Errorf("trust: %w", err)
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		if st, err = c.State(mac); err == nil && st.Connected {
			c.logf("acquire %s: connected (by auto-reconnect) after %s", mac, since(start))
			return st, nil
		}
		t := time.Now()
		call := c.obj(p).CallWithContext(ctx, ifaceDevice+".Connect", 0)
		if call.Err == nil {
			c.logf("acquire %s: Connect #%d ok in %s, %s total; after: %s",
				mac, attempt, since(t), since(start), c.describe(mac))
			return c.State(mac)
		}
		lastErr = call.Err
		if ctx.Err() != nil {
			// BlueZ hadn't answered yet; our deadline cut the call short.
			c.logf("acquire %s: Connect #%d still pending after %s when the deadline hit; after: %s",
				mac, attempt, since(t), c.describe(mac))
		} else {
			c.logf("acquire %s: Connect #%d failed after %s: %s", mac, attempt, since(t), dbusErr(call.Err))
		}
		select {
		case <-ctx.Done():
			c.logf("acquire %s: gave up after %s and %d attempt(s)", mac, since(start), attempt)
			return st, fmt.Errorf("connect timed out (is the device awake? try moving it): %w", lastErr)
		case <-time.After(750 * time.Millisecond):
		}
	}
}

// PairedDevice is a device bonded with this adapter.
type PairedDevice struct {
	MAC       string
	Name      string
	Connected bool
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
		d.Connected, _ = props["Connected"].Value().(bool)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
