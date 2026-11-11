package hidpp

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// fakeMouse answers HID++ like an MX Vertical with 3 channels, interleaved
// with pointer reports.
type fakeMouse struct {
	channels, current byte
	hidpp             bool
	queue             [][]byte
	sentChannel       int
}

func (m *fakeMouse) Write(b []byte) (int, error) {
	if !m.hidpp {
		return 0, syscall.EINVAL
	}
	m.queue = append(m.queue, []byte{0x02, 1, 2, 3}) // pointer movement
	feat, fid := b[2], b[3]
	reply := func(p ...byte) {
		r := make([]byte, 20)
		copy(r, []byte{reportLong, devIndex, feat, fid})
		copy(r[4:], p)
		m.queue = append(m.queue, r)
	}
	switch {
	case feat == 0 && b[4] == 0x18 && b[5] == 0x14:
		reply(12)
	case feat == 0:
		reply(0)
	case feat == 12 && fid>>4 == 0:
		reply(m.channels, m.current)
	case feat == 12 && fid>>4 == 1:
		m.sentChannel = int(b[4]) + 1
	}
	return len(b), nil
}

func (m *fakeMouse) Read(b []byte) (int, error) {
	if len(m.queue) == 0 {
		return 0, syscall.EAGAIN
	}
	n := copy(b, m.queue[0])
	m.queue = m.queue[1:]
	return n, nil
}

func (m *fakeMouse) Close() error { return nil }

func fakeSys(t *testing.T, m *fakeMouse) {
	t.Helper()
	root := t.TempDir()
	for name, uniq := range map[string]string{"hidraw0": "", "hidraw3": "aa:bb:cc:dd:ee:83", "hidraw5": "12:34:56:78:9a:bc"} {
		id := "0005:0000046D:0000B020"
		if name == "hidraw5" {
			id = "0005:00001235:0000AA22" // another vendor: never opened
		}
		dir := filepath.Join(root, name, "device")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "uevent"), []byte("HID_ID="+id+"\nHID_NAME=x\nHID_UNIQ="+uniq+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldRoot, oldOpen := SysRoot, openFn
	SysRoot = root
	openFn = func(p string) (*device, error) {
		if p != "/dev/hidraw3" {
			t.Fatalf("opened %s", p)
		}
		return &device{f: m}, nil
	}
	t.Cleanup(func() { SysRoot, openFn = oldRoot, oldOpen })
}

func TestProbeAndSetChannel(t *testing.T) {
	m := &fakeMouse{channels: 3, current: 0, hidpp: true}
	fakeSys(t, m)
	info, err := Probe("AA:BB:CC:DD:EE:83")
	if err != nil || info != (Info{Channels: 3, Current: 1}) {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	if err := SetChannel("AA:BB:CC:DD:EE:83", 2); err != nil || m.sentChannel != 2 {
		t.Fatalf("SetChannel: %v, sent %d", err, m.sentChannel)
	}
	if err := SetChannel("AA:BB:CC:DD:EE:83", 4); err == nil {
		t.Fatal("channel 4 of 3 accepted")
	}
	if _, err := Probe("11:22:33:44:55:66"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	if _, err := Probe("12:34:56:78:9A:BC"); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("other vendor: %v", err)
	}
}

func TestNonHIDPPDevice(t *testing.T) {
	fakeSys(t, &fakeMouse{})
	if _, err := Probe("AA:BB:CC:DD:EE:83"); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("got %v", err)
	}
}

func TestSingleChannelDevice(t *testing.T) {
	fakeSys(t, &fakeMouse{channels: 1, hidpp: true})
	if _, err := Probe("AA:BB:CC:DD:EE:83"); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("got %v", err)
	}
}
