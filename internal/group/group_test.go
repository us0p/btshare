package group

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/us0p/btshare/internal/agent"
	"github.com/us0p/btshare/internal/bluez"
	"github.com/us0p/btshare/internal/config"
	"github.com/us0p/btshare/internal/node"
)

const secret = "shared-secret-0123456789"

type recorder struct {
	mu     sync.Mutex
	errors []string
	infos  []string
}

func (r *recorder) Error(s, b string) {
	r.mu.Lock()
	r.errors = append(r.errors, s+": "+b)
	r.mu.Unlock()
}
func (r *recorder) Info(s, b string) { r.mu.Lock(); r.infos = append(r.infos, s+": "+b); r.mu.Unlock() }

type failingNode struct{}

func (failingNode) State(context.Context, string) (bluez.DeviceState, error) {
	return bluez.DeviceState{Known: true}, nil
}
func (failingNode) Release(context.Context, string) (bluez.DeviceState, error) {
	return bluez.DeviceState{}, nil
}
func (failingNode) Acquire(context.Context, string) (bluez.DeviceState, error) {
	return bluez.DeviceState{}, errors.New("page timeout")
}
func (failingNode) SetChannel(context.Context, string, int) error { return nil }

type member struct {
	name string
	path string
	addr string
	note *recorder
}

// newMember starts an agent with a fresh single-host config, like install.sh does.
func newMember(t *testing.T, name string) *member {
	t.Helper()
	m := &member{name: name, path: filepath.Join(t.TempDir(), "config.toml"), note: &recorder{}}
	srv := httptest.NewUnstartedServer(nil)
	m.addr = srv.Listener.Addr().String()
	srv.Config.Handler = (&agent.Server{Node: failingNode{}, Secret: []byte(secret),
		ConfigPath: m.path, Notify: m.note}).Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	err := config.Create(m.path, &config.Config{Self: name, Secret: secret,
		Hosts: []config.Host{{Name: name, Addr: m.addr}}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func hostNames(t *testing.T, m *member) string {
	c, err := config.Load(m.path)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, h := range c.Hosts {
		n = append(n, h.Name)
	}
	return strings.Join(n, ",")
}

func TestJoinAndDevicePropagation(t *testing.T) {
	ctx := context.Background()
	a, b, c := newMember(t, "a"), newMember(t, "b"), newMember(t, "c")

	if _, _, err := AddDevice(ctx, a.path, config.Device{Name: "mouse", MAC: "aa:bb:cc:dd:ee:7f"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, w, err := Join(ctx, b.path, a.addr, ""); err != nil || len(w) > 0 {
		t.Fatalf("b join: %v %v", err, w)
	}
	if _, w, err := Join(ctx, c.path, b.addr, ""); err != nil || len(w) > 0 {
		t.Fatalf("c join: %v %v", err, w)
	}

	// Each config lists itself first, then others in the order they were learned.
	for m, want := range map[*member]string{a: "a,b,c", b: "b,a,c", c: "c,b,a"} {
		if got := hostNames(t, m); got != want {
			t.Errorf("%s hosts = %s, want %s", m.name, got, want)
		}
	}
	// c learned about the mouse from b, which learned it from a.
	if cfg, _ := config.Load(c.path); !cfg.HasMAC("AA:BB:CC:DD:EE:7F") {
		t.Error("device not propagated to c")
	}
	// a device added on c reaches everyone.
	if _, w, err := AddDevice(ctx, c.path, config.Device{Name: "keyboard", MAC: "11:22:33:44:55:66"}, nil); err != nil || len(w) > 0 {
		t.Fatalf("add device: %v %v", err, w)
	}
	for _, m := range []*member{a, b} {
		if cfg, _ := config.Load(m.path); !cfg.HasMAC("11:22:33:44:55:66") {
			t.Errorf("keyboard missing on %s", m.name)
		}
	}
	if len(a.note.infos) == 0 {
		t.Error("no 'computer added' notification on a")
	}
}

func TestJoinRejectsNameClash(t *testing.T) {
	a := newMember(t, "pc")
	b := newMember(t, "pc")
	if _, _, err := Join(context.Background(), b.path, a.addr, ""); err == nil {
		t.Fatal("expected name clash error")
	}
	if len(a.note.errors) == 0 {
		t.Fatal("name clash not notified")
	}
}

func TestAgentNotifiesErrors(t *testing.T) {
	a := newMember(t, "a")
	ctx := context.Background()
	if _, _, err := AddDevice(ctx, a.path, config.Device{Name: "mouse", MAC: "AA:BB:CC:DD:EE:7F"}, nil); err != nil {
		t.Fatal(err)
	}

	bad := &node.Remote{Addr: a.addr, Secret: []byte("wrong-secret-0123456789")}
	if _, err := bad.State(ctx, "AA:BB:CC:DD:EE:7F"); err == nil {
		t.Fatal("wrong secret accepted")
	}
	good := &node.Remote{Addr: a.addr, Secret: []byte(secret)}
	if _, err := good.Acquire(ctx, "AA:BB:CC:DD:EE:7F"); err == nil {
		t.Fatal("expected acquire failure")
	}
	if _, err := good.Release(ctx, "AA:AA:AA:AA:AA:AA"); err == nil {
		t.Fatal("unconfigured device accepted")
	}

	got := strings.Join(a.note.errors, "\n")
	for _, want := range []string{"Rejected request from 127.0.0.1", "Could not connect mouse: page timeout", "Unknown device"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing notification %q in:\n%s", want, got)
		}
	}
}

func TestDeviceRemovePropagation(t *testing.T) {
	ctx := context.Background()
	a, b := newMember(t, "a"), newMember(t, "b")
	if _, w, err := Join(ctx, b.path, a.addr, ""); err != nil || len(w) > 0 {
		t.Fatalf("join: %v %v", err, w)
	}
	for _, d := range []config.Device{{Name: "mouse", MAC: "AA:BB:CC:DD:EE:80"}, {Name: "kbd", MAC: "11:22:33:44:55:66"}} {
		if _, w, err := AddDevice(ctx, a.path, d, nil); err != nil || len(w) > 0 {
			t.Fatalf("add: %v %v", err, w)
		}
	}

	// By name, from b: gone everywhere, the other device stays.
	d, w, err := RemoveDevice(ctx, b.path, "mouse")
	if err != nil || len(w) > 0 || d.MAC != "AA:BB:CC:DD:EE:80" {
		t.Fatalf("remove: %+v %v %v", d, w, err)
	}
	for _, m := range []*member{a, b} {
		cfg, _ := config.Load(m.path)
		if cfg.HasMAC("AA:BB:CC:DD:EE:80") || !cfg.HasMAC("11:22:33:44:55:66") {
			t.Errorf("%s devices after remove: %+v", m.name, cfg.Devices)
		}
	}

	// Unknown name: error. A MAC only a peer has: still cleaned up there.
	if _, _, err := RemoveDevice(ctx, b.path, "nope"); err == nil {
		t.Error("removing an unknown name should fail")
	}
	if _, err := config.Update(b.path, func(c *config.Config) error { c.RemoveDevice("11:22:33:44:55:66"); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, w, err := RemoveDevice(ctx, b.path, "11:22:33:44:55:66"); err != nil || len(w) > 0 {
		t.Fatalf("remove by MAC: %v %v", err, w)
	}
	if cfg, _ := config.Load(a.path); cfg.HasMAC("11:22:33:44:55:66") {
		t.Error("peer-only device not removed from a")
	}
}

func TestMultiChannelDeviceRegistration(t *testing.T) {
	ctx := context.Background()
	a, b := newMember(t, "a"), newMember(t, "b")
	if _, w, err := Join(ctx, b.path, a.addr, ""); err != nil || len(w) > 0 {
		t.Fatalf("join: %v %v", err, w)
	}
	// a: channel 1 at :83. b: channel 2, where the mouse uses :7F.
	ha := &config.DeviceHost{Host: "a", MAC: "AA:BB:CC:DD:EE:83", Channel: 1}
	if _, w, err := AddDevice(ctx, a.path, config.Device{Name: "mx", MAC: ha.MAC}, ha); err != nil || len(w) > 0 {
		t.Fatalf("add on a: %v %v", err, w)
	}
	// b without reading the channel: refused, it would lose the channel setup.
	if _, _, err := AddDevice(ctx, b.path, config.Device{Name: "mx", MAC: "AA:BB:CC:DD:EE:7F"}, nil); err == nil {
		t.Fatal("adding a multi-channel device without its channel succeeded")
	}
	// b on a's channel: refused.
	same := &config.DeviceHost{Host: "b", MAC: "AA:BB:CC:DD:EE:7F", Channel: 1}
	if _, _, err := AddDevice(ctx, b.path, config.Device{Name: "mx", MAC: same.MAC}, same); err == nil {
		t.Fatal("two computers on one channel accepted")
	}
	hb := &config.DeviceHost{Host: "b", MAC: "AA:BB:CC:DD:EE:7F", Channel: 2}
	if _, w, err := AddDevice(ctx, b.path, config.Device{Name: "mx", MAC: hb.MAC}, hb); err != nil || len(w) > 0 {
		t.Fatalf("add on b: %v %v", err, w)
	}
	for _, m := range []*member{a, b} {
		cfg, err := config.Load(m.path)
		if err != nil {
			t.Fatal(err)
		}
		d, err := cfg.Device("mx")
		if err != nil {
			t.Fatal(err)
		}
		if d.MAC != ha.MAC || d.MACOn("a") != ha.MAC || d.MACOn("b") != hb.MAC || len(d.Hosts) != 2 {
			t.Errorf("%s: %+v", m.name, d)
		}
		if !cfg.HasMAC(hb.MAC) {
			t.Errorf("%s: agent would refuse b's address", m.name)
		}
	}
	// Removing by b's address removes it everywhere.
	if _, w, err := RemoveDevice(ctx, b.path, hb.MAC); err != nil || len(w) > 0 {
		t.Fatalf("remove: %v %v", err, w)
	}
	if cfg, _ := config.Load(a.path); len(cfg.Devices) != 0 {
		t.Errorf("a still has %+v", cfg.Devices)
	}
}

func TestJoinWithSecret(t *testing.T) {
	ctx := context.Background()
	a, b := newMember(t, "a"), newMember(t, "b")
	// b was installed on its own, with a different secret.
	if _, err := config.Update(b.path, func(c *config.Config) error { c.Secret = "another-secret-0123456789"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Join(ctx, b.path, a.addr, ""); err == nil || !strings.Contains(err.Error(), "btshare invite") {
		t.Fatalf("join with b's own secret: %v", err)
	}
	if _, _, err := Join(ctx, b.path, a.addr, "mistyped-secret-0123456789"); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if c, _ := config.Load(b.path); c.Secret != "another-secret-0123456789" {
		t.Fatal("a rejected secret was saved")
	}
	if _, w, err := Join(ctx, b.path, a.addr, secret); err != nil || len(w) > 0 {
		t.Fatalf("join: %v %v", err, w)
	}
	if c, _ := config.Load(b.path); c.Secret != secret || len(c.Hosts) != 2 {
		t.Fatalf("after join: %d hosts, secret saved=%v", len(c.Hosts), c.Secret == secret)
	}
}
