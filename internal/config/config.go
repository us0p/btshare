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

// Device resolves a device by configured name or MAC.
func (c *Config) Device(nameOrMAC string) (Device, error) {
	for _, d := range c.Devices {
		if strings.EqualFold(d.Name, nameOrMAC) || strings.EqualFold(d.MAC, nameOrMAC) {
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

// UpsertDevice appends d, or updates the MAC of an existing device with the
// same name. Reports whether anything changed.
func (c *Config) UpsertDevice(d Device) bool {
	d.MAC = strings.ToUpper(d.MAC)
	for i, e := range c.Devices {
		if e.Name == d.Name {
			if e.MAC == d.MAC {
				return false
			}
			c.Devices[i].MAC = d.MAC
			return true
		}
	}
	c.Devices = append(c.Devices, d)
	return true
}

// RemoveDevice removes the device with this MAC. Reports whether it was there.
func (c *Config) RemoveDevice(mac string) bool {
	mac = strings.ToUpper(mac)
	for i, d := range c.Devices {
		if d.MAC == mac {
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
