package qcycore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonVersionFileBelongsToItsPid(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if v := ReadDaemonVersion(42); v != "" {
		t.Fatalf("no file: %q", v)
	}
	if err := WriteDaemonVersion(42, "0.1.6"); err != nil {
		t.Fatal(err)
	}
	if v := ReadDaemonVersion(42); v != "0.1.6" {
		t.Fatalf("own pid: %q", v)
	}
	if v := ReadDaemonVersion(43); v != "" {
		t.Fatalf("stale file for another pid answered %q", v)
	}
	RemoveDaemonVersion(43) // not ours: kept
	if ReadDaemonVersion(42) != "0.1.6" {
		t.Fatal("removed another daemon's record")
	}
	RemoveDaemonVersion(42)
	if ReadDaemonVersion(42) != "" {
		t.Fatal("record not removed")
	}
}

func TestWriteFileAtomicReplacesWithPermAndNoLeftovers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if string(b) != "new" || fi.Mode().Perm() != 0o644 {
		t.Fatalf("%q %v", b, fi.Mode())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("leftover temp files: %v", ents)
	}
}
