package switcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/node"
)

const mac = "AA:BB:CC:DD:EE:FF"

// world simulates one physical device shared by several fake hosts and
// fails the test if it is ever connected to two hosts at once.
type world struct {
	t      *testing.T
	mu     sync.Mutex
	holder string
	events []string // "unblock pc2", "release pc1", in order
}

type fakeHost struct {
	w           *world
	name        string
	known       bool
	blocked     bool
	down        bool // unreachable
	failConn    bool
	failRelease bool // release errors without letting go
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
		if !f.failConn && (f.w.holder == "" || f.w.holder == f.name) {
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

	if _, err := s.Switch(context.Background(), mac, "pc2"); err != nil {
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

	rep, err := s.Switch(context.Background(), mac, "pc2")
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
	rep, err := s.Switch(context.Background(), mac, "pc2")
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
	if _, err := s.Next(context.Background(), mac); err == nil {
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
	rep, err := s.Switch(context.Background(), mac, "pc2")
	if err == nil || rep.Restored == nil || rep.Restored.Err == nil {
		t.Fatalf("err=%v Restored=%+v", err, rep.Restored)
	}
}

func TestFailedSwitchWithNoHolderRestoresNothing(t *testing.T) {
	pc1 := &fakeHost{name: "pc1", known: true}
	pc2 := &fakeHost{name: "pc2", known: true, failConn: true}
	s, _ := setup(t, "", pc1, pc2)
	rep, err := s.Switch(context.Background(), mac, "pc2")
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
		if _, err := s.Next(context.Background(), mac); err != nil {
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
	if _, err := s.Next(context.Background(), mac); err != nil {
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
	if _, err := s.Switch(context.Background(), mac, "pc2"); err != nil {
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
	if _, err := s.Switch(context.Background(), mac, "pc2"); err == nil {
		t.Fatal("expected error")
	}
	if w.holder != "pc1" || !pc2.blocked {
		t.Fatalf("holder=%s pc2.blocked=%v", w.holder, pc2.blocked)
	}
}
