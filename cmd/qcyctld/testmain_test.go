package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Structural guard: tests never reach the real BLE adapter (the real dialer
// refuses while hardwareAllowed is false) and never touch the user's real
// socket, backups or caches (HOME and XDG_* point at a throwaway tree).
// Tests that need a link inject fakeDialer/eqDialer through newDaemon.

var testRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "qcyctld-test-")
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
	hardwareAllowed = false
	earSettle = 20 * time.Millisecond
	earReplyTimeout = 150 * time.Millisecond // a fake that never answers a 0x46/0x47 query must not slow every status

	code := m.Run()

	hardwareAllowed = true
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

func TestRealDialerRefusedUnderTest(t *testing.T) {
	_, _, err := (&bleDialer{scanSeconds: 1}).Dial(func([]byte) {})
	if !errors.Is(err, errHardwareDisabled) {
		t.Fatalf("real BLE dialer was not refused: %v", err)
	}
}

func TestEnvIsSandboxed(t *testing.T) {
	for _, k := range []string{"HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		if v := os.Getenv(k); !strings.HasPrefix(v, testRoot) {
			t.Errorf("%s=%q is outside the test sandbox %q", k, v, testRoot)
		}
	}
}
