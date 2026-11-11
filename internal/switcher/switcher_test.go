package switcher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/config"
	"github.com/us0p/btshare/internal/node"
)

const mac = "AA:BB:CC:DD:EE:FF"

var dev = config.Device{Name: "mouse", MAC: mac}

// world simulates one physical device shared by several fake hosts and
// fails the test if it is ever connected to two hosts at once.
type world struct {
	t      *testing.T
	mu     sync.Mutex
	holder string
	events []string // "unblock pc2", "release pc1", in order
	// channel is the multi-channel device's active channel; 0 for a
	// single-host device. A host connects only on its own channel.
	channel int
}

type fakeHost struct {
	w           *world
	name        string
	known       bool
	blocked     bool
	down        bool // unreachable
	failConn    bool
	failRelease bool // release errors without letting go
	channel     int  // this host's channel of a multi-channel device
}

func (f *fakeHost) SetChannel(_ context.Context, _ string, ch int) error {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	if f.down {
		return errDown
	}
	f.w.events = append(f.w.events, "channel "+f.name)
	if f.w.holder != f.name {
		return errors.New("device is not connected to this computer")
	}
	f.w.channel, f.w.holder = ch, ""
	return nil
}

var errDown = errors.New("connection refused")

func (f *fakeHost) st() bluez.DeviceState {
	return bluez.DeviceState{MAC: mac, Known: f.known, Paired: f.known,
		Connected: f.w.holder == f.name, Blocked: f.blocked}
}

func (f *fakeHost) State(context.Context, string) (bluez.DeviceState, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	if f.down {
		return bluez.DeviceState{}, errDown
	}
	return f.st(), nil
}

func (f *fakeHost) Release(context.Context, string) (bluez.DeviceState, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	if f.down {
		return bluez.DeviceState{}, errDown
	}
	f.w.events = append(f.w.events, "release "+f.name)
	if f.failRelease {
		return f.st(), errors.New("disconnect: busy")
	}
	if f.known {
		if f.w.holder == f.name {
			f.w.holder = ""
		}
		f.blocked = true
	}
	return f.st(), nil
}

// Acquire behaves like BlueZ: it unblocks the device, then waits until the
// device is free (released elsewhere) and connects, or gives up with ctx.
func (f *fakeHost) Acquire(ctx context.Context, _ string) (bluez.DeviceState, error) {
	f.w.mu.Lock()
	if f.down {
		f.w.mu.Unlock()
		return bluez.DeviceState{}, errDown
	}
	if !f.known {
		defer f.w.mu.Unlock()
		return f.st(), bluez.ErrUnknownDevice
	}
	f.blocked = false
	f.w.events = append(f.w.events, "unblock "+f.name)
	f.w.mu.Unlock()
	for {
		f.w.mu.Lock()
		if !f.failConn && f.w.channel == f.channel && (f.w.holder == "" || f.w.holder == f.name) {
			f.w.holder = f.name
			defer f.w.mu.Unlock()
			return f.st(), nil
		}
		f.w.mu.Unlock()
		select {
		case <-ctx.Done():
			f.w.mu.Lock()
			defer f.w.mu.Unlock()
			return f.st(), errors.New("page timeout")
		case <-time.After(time.Millisecond):
		}
	}
}

func init() {
	// Keep failing acquires fast; the fakes answer instantly.
	AcquireTimeout = 100 * time.Millisecond
	HeadStart = 5 * time.Millisecond
}

func setup(t *testing.T, holder string, hosts ...*fakeHost) (*Switcher, *world) {
	w := &world{t: t, holder: holder}
	s := &Switcher{Nodes: map[string]node.Node{}, Self: hosts[0].name}
	for _, h := range hosts {
		h.w = w
		s.Order = append(s.Order, h.name)
		s.Nodes[h.name] = h
	}
	return s, w
}

func TestSwitchMovesDevice(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true, blocked: true}
	s, w := setup(t, "pc1", pc1, pc2)

	if _, err := s.Switch(context.Background(), dev, "pc2"); err != nil {
		t.Fatal(err)
	}
	if w.holder != "pc2" || !pc1.blocked || pc2.blocked {
		t.Fatalf("holder=%s pc1.blocked=%v pc2.blocked=%v", w.holder, pc1.blocked, pc2.blocked)
	}
}

func TestSwitchToleratesOfflineHost(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true}
	laptop := &fakeHost{name: "laptop", known: true, down: true}
	s, w := setup(t, "pc1", pc1, pc2, laptop)

	rep, err := s.Switch(context.Background(), dev, "pc2")
	if err != nil {
		t.Fatal(err)
	}
	if w.holder != "pc2" {
		t.Fatalf("holder=%s", w.holder)
	}
	var sawDown bool
	for _, r := range rep.Released {
		sawDown = sawDown || (r.Host == "laptop" && r.Err != nil)
	}
	if !sawDown {
		t.Fatal("offline host's error not reported")
	}
}

func TestSwitchReportsAcquireFailure(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true, failConn: true}
	s, w := setup(t, "pc1", pc1, pc2)
	rep, err := s.Switch(context.Background(), dev, "pc2")
	if err == nil {
		t.Fatal("expected error")
	}
	// The device goes back to pc1 instead of staying blocked everywhere.
	if w.holder != "pc1" || pc1.blocked || !pc2.blocked {
		t.Fatalf("holder=%s pc1.blocked=%v pc2.blocked=%v", w.holder, pc1.blocked, pc2.blocked)
	}
	if rep.Restored == nil || rep.Restored.Host != "pc1" || rep.Restored.Err != nil {
		t.Fatalf("Restored=%+v", rep.Restored)
	}
}

func TestNextRestoresHolderOnFailure(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true, failConn: true}
	s, w := setup(t, "pc1", pc1, pc2)
	if _, err := s.Next(context.Background(), dev); err == nil {
		t.Fatal("expected error")
	}
	if w.holder != "pc1" || pc1.blocked {
		t.Fatalf("holder=%s pc1.blocked=%v", w.holder, pc1.blocked)
	}
}

func TestRestoreFailureIsReported(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true, failConn: true}
	s, _ := setup(t, "pc1", pc1, pc2)
	pc1.failConn = true // the original holder can't take it back either
	rep, err := s.Switch(context.Background(), dev, "pc2")
	if err == nil || rep.Restored == nil || rep.Restored.Err == nil {
		t.Fatalf("err=%v Restored=%+v", err, rep.Restored)
	}
}

func TestFailedSwitchWithNoHolderRestoresNothing(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true, failConn: true}
	s, _ := setup(t, "", pc1, pc2)
	rep, err := s.Switch(context.Background(), dev, "pc2")
	if err == nil || rep.Restored != nil {
		t.Fatalf("err=%v Restored=%+v", err, rep.Restored)
	}
}

func TestNextCyclesAndSkipsUnusable(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: false} // never paired
	pc3 := &fakeHost{name: "pc3", known: true}
	s, w := setup(t, "pc1", pc1, pc2, pc3)

	for _, want := range []string{"pc3", "pc1", "pc3"} {
		if _, err := s.Next(context.Background(), dev); err != nil {
			t.Fatal(err)
		}
		if w.holder != want {
			t.Fatalf("holder=%s want %s", w.holder, want)
		}
	}
}

func TestNextWithNoHolderStartsAfterSelf(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true}
	s, w := setup(t, "", pc1, pc2)
	if _, err := s.Next(context.Background(), dev); err != nil {
		t.Fatal(err)
	}
	if w.holder != "pc2" {
		t.Fatalf("holder=%s", w.holder)
	}
}

func TestTargetListensBeforeHolderReleases(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true, blocked: true}
	s, w := setup(t, "pc1", pc1, pc2)
	if _, err := s.Switch(context.Background(), dev, "pc2"); err != nil {
		t.Fatal(err)
	}
	if len(w.events) < 2 || w.events[0] != "unblock pc2" || w.events[1] != "release pc1" {
		t.Fatalf("events %v: pc2 must be unblocked before pc1 lets go", w.events)
	}
}

func TestAbortedSwitchReblocksTarget(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true, failRelease: true}
	pc2 := &fakeHost{name: "pc2", known: true, blocked: true}
	s, w := setup(t, "pc1", pc1, pc2)
	if _, err := s.Switch(context.Background(), dev, "pc2"); err == nil {
		t.Fatal("expected error")
	}
	if w.holder != "pc1" || !pc2.blocked {
		t.Fatalf("holder=%s pc2.blocked=%v", w.holder, pc2.blocked)
	}
}

// ---- multi-channel devices ----

func channelDev(hosts ...*fakeHost) config.Device {
	d := config.Device{Name: "mx", MAC: mac}
	for _, h := range hosts {
		d.Hosts = append(d.Hosts, config.DeviceHost{Host: h.name, MAC: mac, Channel: h.channel})
	}
	return d
}

func TestChannelSwitch(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true, channel: 1}
	pc2 := &fakeHost{name: "pc2", known: true, channel: 2}
	s, w := setup(t, "pc1", pc1, pc2)
	w.channel = 1
	d := channelDev(pc1, pc2)
	if _, err := s.Switch(context.Background(), d, "pc2"); err != nil {
		t.Fatal(err)
	}
	if w.holder != "pc2" || w.channel != 2 || pc1.blocked {
		t.Fatalf("holder=%s channel=%d pc1.blocked=%v", w.holder, w.channel, pc1.blocked)
	}
	// The target listens first, and nothing is released or blocked.
	if want := "unblock pc2,channel pc1"; strings.Join(w.events, ",") != want {
		t.Fatalf("events %v, want %s", w.events, want)
	}
	if _, err := s.Next(context.Background(), d); err != nil || w.holder != "pc1" || w.channel != 1 {
		t.Fatalf("next back: err=%v holder=%s channel=%d", err, w.holder, w.channel)
	}
}

func TestChannelSwitchNeedsTargetChannel(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true, channel: 1}
	pc2 := &fakeHost{name: "pc2", known: true}
	s, w := setup(t, "pc1", pc1, pc2)
	w.channel = 1
	d := channelDev(pc1) // pc2 never registered a channel
	if _, err := s.Switch(context.Background(), d, "pc2"); err == nil || !strings.Contains(err.Error(), "device add") {
		t.Fatalf("err=%v", err)
	}
	if _, err := s.Next(context.Background(), d); err == nil {
		t.Fatal("next picked a host without a channel")
	}
	if w.holder != "pc1" || len(w.events) != 0 {
		t.Fatalf("holder=%s events=%v", w.holder, w.events)
	}
}

func TestChannelSwitchFailureSaysWhichButton(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true, channel: 1}
	pc2 := &fakeHost{name: "pc2", known: true, channel: 2, failConn: true}
	s, w := setup(t, "pc1", pc1, pc2)
	w.channel = 1
	_, err := s.Switch(context.Background(), channelDev(pc1, pc2), "pc2")
	if err == nil || !strings.Contains(err.Error(), "go back to pc1 (channel 1)") {
		t.Fatalf("err=%v", err)
	}
}

func TestChannelSwitchHolderFails(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true, channel: 1, down: true}
	pc2 := &fakeHost{name: "pc2", known: true, channel: 2}
	s, w := setup(t, "pc1", pc1, pc2)
	w.channel = 1
	// pc1 is down, so status finds no holder: pc2 can only wait, and the
	// error says to press the button.
	_, err := s.Switch(context.Background(), channelDev(pc1, pc2), "pc2")
	if err == nil || !strings.Contains(err.Error(), "select channel 2") {
		t.Fatalf("err=%v", err)
	}
	if w.holder != "pc1" {
		t.Fatalf("holder=%s", w.holder)
	}
}
