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
