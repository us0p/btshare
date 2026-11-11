package switcher

import (
	"context"
	"errors"
	"sync"
	"testing"

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
}

type fakeHost struct {
	w        *world
	name     string
	known    bool
	blocked  bool
	down     bool // unreachable
	failConn bool
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
	if f.known {
		if f.w.holder == f.name {
			f.w.holder = ""
		}
		f.blocked = true
	}
	return f.st(), nil
}

func (f *fakeHost) Acquire(context.Context, string) (bluez.DeviceState, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	if f.down {
		return bluez.DeviceState{}, errDown
	}
	if !f.known {
		return f.st(), bluez.ErrUnknownDevice
	}
	f.blocked = false
	if f.failConn {
		return f.st(), errors.New("page timeout")
	}
	if f.w.holder != "" && f.w.holder != f.name {
		f.w.t.Errorf("%s connected while %s still holds the device", f.name, f.w.holder)
	}
	f.w.holder = f.name
	return f.st(), nil
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
