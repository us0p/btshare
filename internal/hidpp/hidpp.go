// Package hidpp detects and drives multi-channel Bluetooth devices: devices
// that remember several computers, one per channel, and switch between
// them with a button (Logitech Easy-Switch). It speaks Logitech's HID++ 2.0
// over the device's /dev/hidraw node, using the CHANGE_HOST feature
// (0x1814): getHostInfo reads the channel count and the active channel,
// setCurrentHost switches channel exactly like pressing the button.
//
// Devices that don't speak HID++ simply report ErrNotSupported, and btshare
// shares them by moving the connection instead.
package hidpp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	reportShort = 0x10 // HID++ 1.0 error replies come back as short reports
	reportLong  = 0x11 // 20-byte report used for every request
	devIndex    = 0xFF // devices connected directly over Bluetooth answer on 0xFF
	swID        = 0x0B // our software id, to tell our replies from others'
	longLen     = 20

	featureRoot       = 0x00
	featureChangeHost = 0x1814
)

var (
	// ErrNotFound means no hidraw node belongs to that address: the device
	// isn't connected to this computer.
	ErrNotFound = errors.New("device is not connected to this computer")
	// ErrNotSupported means the device can't switch channels on request.
	ErrNotSupported = errors.New("device has no switchable channels (not a HID++ multi-host device)")
)

// Timeout bounds each request.
var Timeout = 2 * time.Second

// SysRoot is where hidraw nodes are listed; tests point it elsewhere.
var SysRoot = "/sys/class/hidraw"

// logitech is the only vendor speaking HID++; other devices aren't opened.
const logitech = "0000046d"

// node is one /dev/hidraw node of a device.
type node struct {
	path   string
	vendor string // from HID_ID, e.g. "0000046d"
}

// Find returns the /dev/hidraw nodes of the Bluetooth device at mac. The
// kernel exposes the device's address as HID_UNIQ.
func Find(mac string) ([]string, error) {
	nodes, err := find(mac)
	var out []string
	for _, n := range nodes {
		out = append(out, n.path)
	}
	return out, err
}

func find(mac string) ([]node, error) {
	dirs, err := filepath.Glob(filepath.Join(SysRoot, "hidraw*"))
	if err != nil {
		return nil, err
	}
	want := "hid_uniq=" + strings.ToLower(mac)
	var out []node
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "device", "uevent"))
		if err != nil {
			continue
		}
		n, match := node{path: "/dev/" + filepath.Base(d)}, false
		for _, line := range strings.Split(strings.ToLower(string(b)), "\n") {
			line = strings.TrimSpace(line)
			if line == want {
				match = true
			}
			if id, ok := strings.CutPrefix(line, "hid_id="); ok { // bus:vendor:product
				if f := strings.Split(id, ":"); len(f) == 3 {
					n.vendor = f[1]
				}
			}
		}
		if match {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// Info describes a multi-channel device.
type Info struct {
	Channels int // how many channels it has
	Current  int // active channel, 1-based
}

// Probe reads the device's channels. It fails with ErrNotFound if the
// device isn't connected here and ErrNotSupported if it has no channels
// btshare can switch.
func Probe(mac string) (Info, error) {
	var info Info
	err := withDevice(mac, func(d *device, idx byte) error {
		r, err := d.request(idx, 0, nil)
		if err != nil {
			return err
		}
		info = Info{Channels: int(r[0]), Current: int(r[1]) + 1}
		if info.Channels < 2 {
			return ErrNotSupported
		}
		return nil
	})
	return info, err
}

// SetChannel switches the device at mac to channel (1-based). The device
// drops this computer right away to look for the one on that channel.
func SetChannel(mac string, channel int) error {
	return withDevice(mac, func(d *device, idx byte) error {
		r, err := d.request(idx, 0, nil)
		if err != nil {
			return err
		}
		if n := int(r[0]); channel < 1 || channel > n {
			return fmt.Errorf("channel %d out of range (device has %d)", channel, n)
		}
		if int(r[1])+1 == channel {
			return nil // already there
		}
		// No reply comes back: the device leaves as soon as it gets this.
		return d.send(idx, 1, []byte{byte(channel - 1)}) //nolint:gosec // channel is 1..n above, n came from a byte
	})
}

// withDevice opens the device's HID++ node and runs fn with the CHANGE_HOST
// feature index. A device may expose several hidraw nodes; the one that
// answers HID++ is used.
func withDevice(mac string, fn func(*device, byte) error) error {
	nodes, err := find(mac)
	if err != nil {
		return err
	}
	lastErr := ErrNotSupported
	for _, n := range nodes {
		if n.vendor != logitech {
			continue
		}
		p := n.path
		d, err := openFn(p)
		if err != nil {
			if errors.Is(err, os.ErrPermission) {
				return fmt.Errorf("%w: no permission to use %s (run install.sh to add the udev rule, see README)", err, p)
			}
			lastErr = err
			continue
		}
		idx, err := d.featureIndex(featureChangeHost)
		if err == nil {
			err = fn(d, idx)
			d.close()
			return err
		}
		d.close()
		if !errors.Is(err, ErrNotSupported) {
			lastErr = err
		}
	}
	return lastErr
}

// rw is the raw hidraw node; tests replace it.
type rw interface {
	Read([]byte) (int, error) // non-blocking: syscall.EAGAIN when empty
	Write([]byte) (int, error)
	Close() error
}

type device struct{ f rw }

type fdFile int

func (f fdFile) Read(b []byte) (int, error)  { return syscall.Read(int(f), b) }
func (f fdFile) Write(b []byte) (int, error) { return syscall.Write(int(f), b) }
func (f fdFile) Close() error                { return syscall.Close(int(f)) }

// openFn opens a hidraw node; tests replace it.
var openFn = open

func open(path string) (*device, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &device{f: fdFile(fd)}, nil
}

func (d *device) close() { _ = d.f.Close() }

func (d *device) send(feat, fn byte, params []byte) error {
	msg := make([]byte, longLen)
	msg[0], msg[1], msg[2], msg[3] = reportLong, devIndex, feat, fn<<4|swID
	copy(msg[4:], params)
	if _, err := d.f.Write(msg); err != nil {
		// A device without the HID++ long report rejects the write.
		return fmt.Errorf("%w (%w)", ErrNotSupported, err)
	}
	return nil
}

// request sends a HID++ 2.0 request and returns the reply's parameters.
// Other traffic on the node (pointer movement, notifications) is skipped.
func (d *device) request(feat, fn byte, params []byte) ([]byte, error) {
	if err := d.send(feat, fn, params); err != nil {
		return nil, err
	}
	fid := fn<<4 | swID
	buf := make([]byte, 64)
	deadline := time.Now().Add(Timeout)
	for time.Now().Before(deadline) {
		n, err := d.f.Read(buf)
		if errors.Is(err, syscall.EAGAIN) || (err == nil && n == 0) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil {
			return nil, err
		}
		r := buf[:n]
		switch {
		case n >= 6 && r[0] == reportLong && r[1] == devIndex && r[2] == 0xFF && r[3] == feat && r[4] == fid:
			return nil, fmt.Errorf("HID++ error 0x%02x", r[5])
		case n >= 5 && r[0] == reportShort && r[1] == devIndex && r[2] == 0x8F && r[3] == feat:
			return nil, ErrNotSupported // HID++ 1.0 only
		case n >= longLen && r[0] == reportLong && r[1] == devIndex && r[2] == feat && r[3] == fid:
			return append([]byte(nil), r[4:n]...), nil
		}
	}
	return nil, ErrNotSupported // nothing answered: not HID++
}

// featureIndex looks up a feature through the root feature; ErrNotSupported
// if the device doesn't have it.
func (d *device) featureIndex(id uint16) (byte, error) {
	r, err := d.request(featureRoot, 0, binary.BigEndian.AppendUint16(nil, id))
	if err != nil {
		return 0, err
	}
	if r[0] == 0 {
		return 0, ErrNotSupported
	}
	return r[0], nil
}
