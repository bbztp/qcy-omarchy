package qcycore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSocketPathUsesRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	p, err := SocketPath(false)
	if err != nil || p != "/run/user/1234/qcyctld.sock" {
		t.Fatalf("%q %v", p, err)
	}
}

func TestCheckPrivateDir(t *testing.T) {
	d := t.TempDir()
	good := filepath.Join(d, "good")
	os.Mkdir(good, 0o700)
	if err := checkPrivateDir(good); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(d, "open")
	os.Mkdir(open, 0o755)
	os.Chmod(open, 0o755)
	if err := checkPrivateDir(open); err == nil {
		t.Fatal("0755 directory accepted")
	}
	link := filepath.Join(d, "link")
	os.Symlink(good, link)
	if err := checkPrivateDir(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := checkPrivateDir(filepath.Join(d, "missing")); err == nil {
		t.Fatal("missing dir accepted")
	}
}
