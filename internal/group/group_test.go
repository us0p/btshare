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

	if _, err := AddDevice(ctx, a.path, config.Device{Name: "mouse", MAC: "aa:bb:cc:dd:ee:7f"}); err != nil {
		t.Fatal(err)
	}
	if _, w, err := Join(ctx, b.path, a.addr); err != nil || len(w) > 0 {
		t.Fatalf("b join: %v %v", err, w)
	}
	if _, w, err := Join(ctx, c.path, b.addr); err != nil || len(w) > 0 {
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
	if w, err := AddDevice(ctx, c.path, config.Device{Name: "keyboard", MAC: "11:22:33:44:55:66"}); err != nil || len(w) > 0 {
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
	if _, _, err := Join(context.Background(), b.path, a.addr); err == nil {
		t.Fatal("expected name clash error")
	}
	if len(a.note.errors) == 0 {
		t.Fatal("name clash not notified")
	}
}

func TestAgentNotifiesErrors(t *testing.T) {
	a := newMember(t, "a")
	ctx := context.Background()
	if _, err := AddDevice(ctx, a.path, config.Device{Name: "mouse", MAC: "AA:BB:CC:DD:EE:7F"}); err != nil {
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
