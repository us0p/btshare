// Package keys copies a device's BlueZ pairing between hosts.
//
// A peripheral stores one bond per host: (host BD_ADDR, link key / LTK, host
// IRK). For a second host to connect without re-pairing (which would wipe the
// first host's bond on single-host devices), it must present the same adapter
// address and keys. So we export BlueZ's stored keys from the host the device
// was paired with and install them on the others, under the same adapter
// address.
package keys

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const DefaultRoot = "/var/lib/bluetooth"

var macRE = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)

// Bundle holds everything needed to recreate the bond on another host.
// It contains secret key material: treat it like a password.
type Bundle struct {
	Version        int    `json:"version"`
	AdapterAddress string `json:"adapter_address"`
	DeviceMAC      string `json:"device_mac"`
	Info           string `json:"info"`               // <root>/<adapter>/<device>/info
	Identity       string `json:"identity,omitempty"` // <root>/<adapter>/identity (local IRK, LE)
}

func checkMAC(s string) (string, error) {
	s = strings.ToUpper(s)
	if !macRE.MatchString(s) {
		return "", fmt.Errorf("invalid address %q", s)
	}
	return s, nil
}

// Export reads the bond for deviceMAC under adapterAddr.
func Export(root, adapterAddr, deviceMAC string) (*Bundle, error) {
	a, err := checkMAC(adapterAddr)
	if err != nil {
		return nil, err
	}
	d, err := checkMAC(deviceMAC)
	if err != nil {
		return nil, err
	}
	info, err := os.ReadFile(filepath.Join(root, a, d, "info"))
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w (run with sudo)", err)
		}
		return nil, fmt.Errorf("read bond: %w", err)
	}
	if !strings.Contains(string(info), "[LinkKey]") && !strings.Contains(string(info), "LongTermKey]") {
		return nil, fmt.Errorf("%s has no stored keys on adapter %s; is it paired here?", d, a)
	}
	b := &Bundle{Version: 1, AdapterAddress: a, DeviceMAC: d, Info: string(info)}
	if id, err := os.ReadFile(filepath.Join(root, a, "identity")); err == nil {
		b.Identity = string(id)
	}
	return b, nil
}

func (b *Bundle) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(b)
}

func Read(r io.Reader) (*Bundle, error) {
	var b Bundle
	if err := json.NewDecoder(r).Decode(&b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	if b.Version != 1 {
		return nil, fmt.Errorf("unsupported bundle version %d", b.Version)
	}
	var err error
	if b.AdapterAddress, err = checkMAC(b.AdapterAddress); err != nil {
		return nil, err
	}
	if b.DeviceMAC, err = checkMAC(b.DeviceMAC); err != nil {
		return nil, err
	}
	if b.Info == "" {
		return nil, errors.New("bundle has no info")
	}
	return &b, nil
}

// Install writes the bond under <root>/<bundle adapter>/. Existing files are
// backed up with a .btshare.bak suffix. Returns the paths written.
// bluetoothd must be restarted afterwards to pick them up.
func Install(root string, b *Bundle) ([]string, error) {
	// The addresses become path components of files written as root:
	// never trust them unchecked, even if Read already did.
	for _, mac := range []string{b.AdapterAddress, b.DeviceMAC} {
		if _, err := checkMAC(mac); err != nil {
			return nil, err
		}
	}
	adir := filepath.Join(root, b.AdapterAddress)
	ddir := filepath.Join(adir, b.DeviceMAC)
	if err := os.MkdirAll(ddir, 0o700); err != nil {
		return nil, err
	}
	var written []string
	put := func(path, content string) error {
		if old, err := os.ReadFile(path); err == nil && string(old) != content {
			//nolint:gosec // G703: path is root + two checked MACs + a fixed name
			if err := os.WriteFile(path+".btshare.bak", old, 0o600); err != nil {
				return err
			}
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
		written = append(written, path)
		return nil
	}
	if err := put(filepath.Join(ddir, "info"), b.Info); err != nil {
		return written, err
	}
	if b.Identity != "" {
		if err := put(filepath.Join(adir, "identity"), b.Identity); err != nil {
			return written, err
		}
	}
	return written, nil
}
