package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func base() *Config {
	return &Config{
		Self:   "desktop",
		Secret: "0123456789abcdef-secret",
		Hosts:  []Host{{Name: "desktop", Addr: "10.0.0.1:7531"}},
	}
}

func TestCreateLoadUpdate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "btshare", "config.toml")
	if err := Create(p, base()); err != nil {
		t.Fatal(err)
	}
	if err := Create(p, base()); err == nil {
		t.Fatal("Create overwrote an existing config")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}

	_, err := Update(p, func(c *Config) error {
		c.UpsertHost(Host{Name: "laptop", Addr: "10.0.0.2:7531"})
		c.UpsertDevice(Device{Name: "mouse", MAC: "aa:bb:cc:dd:ee:7f"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hosts) != 2 || c.Hosts[1].Name != "laptop" || c.Devices[0].MAC != "AA:BB:CC:DD:EE:7F" {
		t.Fatalf("got %+v", c)
	}

	// Re-adding is a no-op; a changed address updates in place.
	if c.UpsertHost(Host{Name: "laptop", Addr: "10.0.0.2:7531"}) {
		t.Fatal("duplicate host reported as change")
	}
	if !c.UpsertHost(Host{Name: "laptop", Addr: "10.0.0.9:7531"}) || len(c.Hosts) != 2 {
		t.Fatal("address update failed")
	}
}

func TestUpdateRejectsInvalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := Create(p, base()); err != nil {
		t.Fatal(err)
	}
	_, err := Update(p, func(c *Config) error {
		c.UpsertHost(Host{Name: `evil"\n[x]`, Addr: "1.2.3.4:1"})
		return nil
	})
	if err == nil {
		t.Fatal("invalid name accepted")
	}
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "evil") {
		t.Fatal("invalid update was written")
	}
}

func TestConcurrentUpdatesDontLoseEntries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := Create(p, base()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Update(p, func(c *Config) error {
				c.UpsertHost(Host{Name: "h" + string(rune('a'+i)), Addr: "10.0.0.1:1"})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hosts) != 21 {
		t.Fatalf("got %d hosts, want 21", len(c.Hosts))
	}
}

func TestRenameSelf(t *testing.T) {
	c := base()
	c.Hosts = append(c.Hosts, Host{Name: "laptop", Addr: "10.0.0.2:7531"})
	if changed, err := c.RenameSelf("desktop"); err != nil || changed {
		t.Fatalf("same name: changed=%v err=%v", changed, err)
	}
	if _, err := c.RenameSelf("laptop"); err == nil {
		t.Fatal("renamed onto another host's name")
	}
	if _, err := c.RenameSelf("bad name"); err == nil {
		t.Fatal("accepted invalid name")
	}
	if changed, err := c.RenameSelf("office"); err != nil || !changed {
		t.Fatalf("rename: changed=%v err=%v", changed, err)
	}
	if c.Self != "office" || c.Hosts[0].Name != "office" || c.Hosts[1].Name != "laptop" {
		t.Fatalf("after rename: %+v", c)
	}
	c.defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSetPort(t *testing.T) {
	c := base()
	c.Listen = ":7531"
	c.Hosts = append(c.Hosts, Host{Name: "laptop", Addr: "10.0.0.2:7531"})
	if changed, err := c.SetPort("7531"); err != nil || changed {
		t.Fatalf("same port: changed=%v err=%v", changed, err)
	}
	if _, err := c.SetPort("70000"); err == nil {
		t.Fatal("accepted invalid port")
	}
	if changed, err := c.SetPort("8000"); err != nil || !changed {
		t.Fatalf("set port: changed=%v err=%v", changed, err)
	}
	if c.Listen != ":8000" || c.Hosts[0].Addr != "10.0.0.1:8000" || c.Hosts[1].Addr != "10.0.0.2:7531" {
		t.Fatalf("after set port: %+v", c)
	}
}
