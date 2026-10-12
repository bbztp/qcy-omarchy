package qcycore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Structural guard: backups and the curve library written by these tests go to
// a throwaway tree, never the user's real data directory.

var testRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "qcycore-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testmain:", err)
		os.Exit(1)
	}
	testRoot = root
	env := map[string]string{
		"HOME":            filepath.Join(root, "home"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"XDG_RUNTIME_DIR": filepath.Join(root, "run"),
		"XDG_CACHE_HOME":  filepath.Join(root, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
	}
	saved := map[string]*string{}
	for k, v := range env {
		if old, ok := os.LookupEnv(k); ok {
			o := old
			saved[k] = &o
		} else {
			saved[k] = nil
		}
		if err := os.MkdirAll(v, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "testmain:", err)
			os.Exit(1)
		}
		os.Setenv(k, v)
	}

	code := m.Run()

	for k, old := range saved {
		if old == nil {
			os.Unsetenv(k)
		} else {
			os.Setenv(k, *old)
		}
	}
	os.RemoveAll(root)
	os.Exit(code)
}

func TestEnvIsSandboxed(t *testing.T) {
	for _, k := range []string{"HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		if v := os.Getenv(k); !strings.HasPrefix(v, testRoot) {
			t.Errorf("%s=%q is outside the test sandbox %q", k, v, testRoot)
		}
	}
	d, err := BackupDir()
	if err != nil || !strings.HasPrefix(d, testRoot) {
		t.Fatalf("BackupDir=%q err=%v is outside the sandbox", d, err)
	}
}
