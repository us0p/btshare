package keys

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const (
	adapter = "00:D7:6D:78:90:4B"
	dev     = "AA:BB:CC:DD:EE:7F"
	info    = "[General]\nName=Mouse\n\n[LongTermKey]\nKey=00112233445566778899AABBCCDDEEFF\n"
)

func TestExportInstallRoundTrip(t *testing.T) {
	src := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(src, adapter, dev), 0o700))
	must(t, os.WriteFile(filepath.Join(src, adapter, dev, "info"), []byte(info), 0o600))
	must(t, os.WriteFile(filepath.Join(src, adapter, "identity"), []byte("[General]\nIdentityResolvingKey=AB\n"), 0o600))

	b, err := Export(src, adapter, "aa:bb:cc:dd:ee:7f")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	b2, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	// Pre-existing different identity must be backed up, not lost.
	must(t, os.MkdirAll(filepath.Join(dst, adapter), 0o700))
	must(t, os.WriteFile(filepath.Join(dst, adapter, "identity"), []byte("old"), 0o600))

	if _, err := Install(dst, b2); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, adapter, dev, "info"))
	if string(got) != info {
		t.Fatalf("info = %q", got)
	}
	bak, _ := os.ReadFile(filepath.Join(dst, adapter, "identity.btshare.bak"))
	if string(bak) != "old" {
		t.Fatalf("backup = %q", bak)
	}
	fi, _ := os.Stat(filepath.Join(dst, adapter, dev, "info"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
}

func TestExportRejectsUnpaired(t *testing.T) {
	src := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(src, adapter, dev), 0o700))
	must(t, os.WriteFile(filepath.Join(src, adapter, dev, "info"), []byte("[General]\nName=x\n"), 0o600))
	if _, err := Export(src, adapter, dev); err == nil {
		t.Fatal("expected error for device without keys")
	}
}

func TestReadRejectsPathTraversal(t *testing.T) {
	_, err := Read(bytes.NewBufferString(`{"version":1,"adapter_address":"../../etc","device_mac":"` + dev + `","info":"x"}`))
	if err == nil {
		t.Fatal("expected invalid address error")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
