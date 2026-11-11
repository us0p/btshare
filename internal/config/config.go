// Package config loads and updates the btshare configuration. Every host has
// its own file; hosts and devices are kept in sync between them by the
// `join` and `device add` commands, which append entries on all peers.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
)

const DefaultPort = "7531"

type Host struct {
	Name string `toml:"name" json:"name"`
	Addr string `toml:"addr" json:"addr"` // host:port of that host's agent
}

type Device struct {
	Name string `toml:"name" json:"name"`
	MAC  string `toml:"mac" json:"mac"`
	// Hosts is set for multi-channel devices (e.g. Logitech Easy-Switch):
	// each computer is paired on its own channel, where the device may use
	// a different address. Empty for single-host devices, which every
	// computer reaches at MAC with copied pairing keys.
	Hosts []DeviceHost `toml:"hosts,omitempty" json:"hosts,omitempty"`
}

// DeviceHost is where one computer finds a multi-channel device.
type DeviceHost struct {
	Host    string `toml:"host" json:"host"`
	MAC     string `toml:"mac" json:"mac"`         // the device's address on that channel
	Channel int    `toml:"channel" json:"channel"` // 1-based, as printed on the device
}

// MultiChannel reports whether the device switches by changing its channel.
func (d Device) MultiChannel() bool { return len(d.Hosts) > 0 }

// On returns host's entry for a multi-channel device.
func (d Device) On(host string) (DeviceHost, bool) {
	for _, h := range d.Hosts {
		if h.Host == host {
			return h, true
		}
	}
	return DeviceHost{}, false
}

// MACOn returns the address host uses for the device.
func (d Device) MACOn(host string) string {
	if h, ok := d.On(host); ok {
		return h.MAC
	}
	return d.MAC
}

// HasAddr reports whether mac is one of the device's addresses.
func (d Device) HasAddr(mac string) bool {
	if strings.EqualFold(d.MAC, mac) {
		return true
	}
	for _, h := range d.Hosts {
		if strings.EqualFold(h.MAC, mac) {
			return true
		}
	}
	return false
}

// SetHost adds or replaces host's entry. It refuses a channel another
// computer already uses: the device can't tell those two apart.
func (d *Device) SetHost(e DeviceHost) (bool, error) {
	e.MAC = strings.ToUpper(e.MAC)
	if e.Channel < 1 || e.Channel > 255 {
		return false, fmt.Errorf("invalid channel %d", e.Channel)
	}
	for _, h := range d.Hosts {
		if h.Host != e.Host && h.Channel == e.Channel {
			return false, fmt.Errorf("%s already uses channel %d of %s; pair it with this computer on another channel", h.Host, h.Channel, d.Name)
		}
	}
	for i, h := range d.Hosts {
		if h.Host == e.Host {
			if h == e {
				return false, nil
			}
			d.Hosts[i] = e
			return true, nil
		}
	}
	d.Hosts = append(d.Hosts, e)
	return true, nil
}

type Config struct {
	Self    string   `toml:"self"`    // name of this host in Hosts
	Listen  string   `toml:"listen"`  // agent listen address, default ":7531"
	Secret  string   `toml:"secret"`  // shared HMAC secret, same on all hosts
	Adapter string   `toml:"adapter"` // local adapter, default "hci0"
	Hosts   []Host   `toml:"hosts"`   // ordered: `next` cycles in this order
	Devices []Device `toml:"devices"`
}

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	macRE    = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)
	secretRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,256}$`)
)

// DefaultPath returns $BTSHARE_CONFIG or ~/.config/btshare/config.toml.
// Under sudo it uses the invoking user's config, not root's, so commands
// like `sudo btshare keys export mouse` work without extra flags.
func DefaultPath() string {
	if p := os.Getenv("BTSHARE_CONFIG"); p != "" {
		return p
	}
	if os.Geteuid() == 0 {
		if name := os.Getenv("SUDO_USER"); name != "" && name != "root" {
			if u, err := user.Lookup(name); err == nil {
				return filepath.Join(u.HomeDir, ".config", "btshare", "config.toml")
			}
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "btshare", "config.toml")
}

func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s (run install.sh or `btshare init`)", path)
		}
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	c.defaults()
	return &c, c.Validate()
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":" + DefaultPort
	}
	if c.Adapter == "" {
		c.Adapter = "hci0"
	}
	for i := range c.Devices {
		c.Devices[i].MAC = strings.ToUpper(c.Devices[i].MAC)
		for j := range c.Devices[i].Hosts {
			c.Devices[i].Hosts[j].MAC = strings.ToUpper(c.Devices[i].Hosts[j].MAC)
		}
	}
}

func ValidName(s string) error {
	if !nameRE.MatchString(s) {
		return fmt.Errorf("invalid name %q (letters, digits, . _ - only)", s)
	}
	return nil
}

func ValidMAC(s string) (string, error) {
	s = strings.ToUpper(s)
	if !macRE.MatchString(s) {
		return "", fmt.Errorf("invalid MAC %q (want AA:BB:CC:DD:EE:FF)", s)
	}
	return s, nil
}

// ValidSecret checks a group secret's format.
func ValidSecret(s string) error {
	if !secretRE.MatchString(s) {
		return errors.New("secret must be 16+ characters (letters, digits, -_.~+/=)")
	}
	return nil
}

func ValidAddr(s string) error {
	h, p, err := net.SplitHostPort(s)
	if err != nil || h == "" || p == "" || strings.ContainsAny(s, "\"\\\n") {
		return fmt.Errorf("invalid address %q (want host:port)", s)
	}
	return nil
}

func (c *Config) Validate() error {
	if !secretRE.MatchString(c.Secret) {
		return errors.New("config: secret must be 16+ characters (letters, digits, -_.~+/=)")
	}
	if len(c.Hosts) == 0 {
		return errors.New("config: no [[hosts]] defined")
	}
	if _, err := c.Host(c.Self); err != nil {
		return fmt.Errorf("config: self: %w", err)
	}
	if err := ValidName(c.Adapter); err != nil {
		return fmt.Errorf("config: adapter: %w", err)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("config: listen: %w", err)
	}
	seen := map[string]bool{}
	for _, h := range c.Hosts {
		if err := ValidName(h.Name); err != nil {
			return fmt.Errorf("config: host: %w", err)
		}
		if err := ValidAddr(h.Addr); err != nil {
			return fmt.Errorf("config: host %s: %w", h.Name, err)
		}
		if seen[h.Name] {
			return fmt.Errorf("config: duplicate host %q", h.Name)
		}
		seen[h.Name] = true
	}
	seen = map[string]bool{}
	for _, d := range c.Devices {
		if err := ValidName(d.Name); err != nil {
			return fmt.Errorf("config: device: %w", err)
		}
		if _, err := ValidMAC(d.MAC); err != nil {
			return fmt.Errorf("config: device %s: %w", d.Name, err)
		}
		if seen[d.Name] {
			return fmt.Errorf("config: duplicate device %q", d.Name)
		}
		seen[d.Name] = true
		hosts, channels := map[string]bool{}, map[int]bool{}
		for _, h := range d.Hosts {
			if err := ValidName(h.Host); err != nil {
				return fmt.Errorf("config: device %s: host: %w", d.Name, err)
			}
			if _, err := ValidMAC(h.MAC); err != nil {
				return fmt.Errorf("config: device %s on %s: %w", d.Name, h.Host, err)
			}
			if h.Channel < 1 || h.Channel > 255 {
				return fmt.Errorf("config: device %s on %s: invalid channel %d", d.Name, h.Host, h.Channel)
			}
			if hosts[h.Host] || channels[h.Channel] {
				return fmt.Errorf("config: device %s: host %s or channel %d listed twice", d.Name, h.Host, h.Channel)
			}
			hosts[h.Host], channels[h.Channel] = true, true
		}
	}
	return nil
}

func (c *Config) Host(name string) (Host, error) {
	for _, h := range c.Hosts {
		if h.Name == name {
			return h, nil
		}
	}
	return Host{}, fmt.Errorf("unknown host %q", name)
}

// Device resolves a device by configured name or any of its addresses.
func (c *Config) Device(nameOrMAC string) (Device, error) {
	for _, d := range c.Devices {
		if strings.EqualFold(d.Name, nameOrMAC) || d.HasAddr(nameOrMAC) {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("unknown device %q (add it with `btshare device add`)", nameOrMAC)
}

// HasMAC reports whether the MAC belongs to a configured device; the agent
// refuses to touch anything else.
func (c *Config) HasMAC(mac string) bool {
	_, err := c.Device(mac)
	return err == nil
}

// UpsertHost appends h, or updates the address of an existing host with the
// same name (DHCP may have moved it). Reports whether anything changed.
func (c *Config) UpsertHost(h Host) bool {
	for i, e := range c.Hosts {
		if e.Name == h.Name {
			if e.Addr == h.Addr {
				return false
			}
			c.Hosts[i].Addr = h.Addr
			return true
		}
	}
	c.Hosts = append(c.Hosts, h)
	return true
}

// UpsertDevice appends d, or merges it into the existing device with the
// same name: its MAC is updated and its per-computer channels are added to
// (or replace) the existing ones, so computers can register their own
// channel without knowing the others'. Reports whether anything changed.
func (c *Config) UpsertDevice(d Device) bool {
	d.MAC = strings.ToUpper(d.MAC)
	for i := range d.Hosts {
		d.Hosts[i].MAC = strings.ToUpper(d.Hosts[i].MAC)
	}
	for i, e := range c.Devices {
		if e.Name != d.Name {
			continue
		}
		changed := e.MAC != d.MAC
		c.Devices[i].MAC = d.MAC
		for _, h := range d.Hosts {
			// Another computer's stale claim on the same channel gives way
			// to the newer one: the device was re-paired there.
			kept := c.Devices[i].Hosts[:0]
			for _, o := range c.Devices[i].Hosts {
				if o.Host == h.Host || o.Channel != h.Channel {
					kept = append(kept, o)
				} else {
					changed = true
				}
			}
			c.Devices[i].Hosts = kept
			if ch, _ := c.Devices[i].SetHost(h); ch {
				changed = true
			}
		}
		return changed
	}
	c.Devices = append(c.Devices, d)
	return true
}

// RemoveDevice removes the device with this address (any of its
// addresses). Reports whether it was there.
func (c *Config) RemoveDevice(mac string) bool {
	for i, d := range c.Devices {
		if d.HasAddr(mac) {
			c.Devices = append(c.Devices[:i], c.Devices[i+1:]...)
			return true
		}
	}
	return false
}

// RenameSelf renames this host, both in Self and in its [[hosts]] entry.
// Reports whether anything changed.
func (c *Config) RenameSelf(name string) (bool, error) {
	if err := ValidName(name); err != nil {
		return false, err
	}
	if name == c.Self {
		return false, nil
	}
	if _, err := c.Host(name); err == nil {
		return false, fmt.Errorf("host name %q is already used by another computer in this config", name)
	}
	for i := range c.Hosts {
		if c.Hosts[i].Name == c.Self {
			c.Hosts[i].Name = name
		}
	}
	for i := range c.Devices {
		for j := range c.Devices[i].Hosts {
			if c.Devices[i].Hosts[j].Host == c.Self {
				c.Devices[i].Hosts[j].Host = name
			}
		}
	}
	c.Self = name
	return true, nil
}

// SetPort changes the agent port, both in Listen and in this host's address.
// Reports whether anything changed.
func (c *Config) SetPort(port string) (bool, error) {
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return false, fmt.Errorf("invalid port %q", port)
	}
	changed := false
	if lh, lp, err := net.SplitHostPort(c.Listen); err == nil && lp != port {
		c.Listen = net.JoinHostPort(lh, port)
		changed = true
	}
	for i := range c.Hosts {
		if c.Hosts[i].Name != c.Self {
			continue
		}
		if h, p, err := net.SplitHostPort(c.Hosts[i].Addr); err == nil && p != port {
			c.Hosts[i].Addr = net.JoinHostPort(h, port)
			changed = true
		}
	}
	return changed, nil
}

// Create writes a new config; it refuses to overwrite an existing one.
func Create(path string, c *Config) error {
	c.defaults()
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config %s already exists", path)
	}
	return save(path, c)
}

// Update loads the config under an exclusive lock, applies fn, validates and
// atomically writes it back. Safe against concurrent CLI and agent updates.
func Update(path string, fn func(*Config) error) (*Config, error) {
	unlock, err := lock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	c, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := fn(c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, save(path, c)
}

func lock(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Render produces the file contents. Values are validated beforehand to a
// charset where Go's %q quoting is valid TOML.
func Render(c *Config) []byte {
	var b bytes.Buffer
	b.WriteString("# btshare configuration. Managed by `btshare` (init / join / host add /\n")
	b.WriteString("# device add); new hosts and devices are appended at the end.\n")
	b.WriteString("# Keep `secret` private: anyone holding it can move your devices.\n\n")
	fmt.Fprintf(&b, "self = %q\nlisten = %q\nadapter = %q\nsecret = %q\n", c.Self, c.Listen, c.Adapter, c.Secret)
	b.WriteString("\n# `btshare next` cycles through hosts in this order.\n")
	for _, h := range c.Hosts {
		fmt.Fprintf(&b, "\n[[hosts]]\nname = %q\naddr = %q\n", h.Name, h.Addr)
	}
	for _, d := range c.Devices {
		fmt.Fprintf(&b, "\n[[devices]]\nname = %q\nmac = %q\n", d.Name, d.MAC)
		if d.MultiChannel() {
			b.WriteString("# Multi-channel device: each computer is paired on its own channel.\n")
		}
		for _, h := range d.Hosts {
			fmt.Fprintf(&b, "\n[[devices.hosts]]\nhost = %q\nmac = %q\nchannel = %d\n", h.Host, h.MAC, h.Channel)
		}
	}
	return b.Bytes()
}

func save(path string, c *Config) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(Render(c)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
