package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Structural guard: no test in this package may reach the real earbuds, the
// real daemon socket or the user's real backups. TestMain disables hardware
// (hardwareAllowed=false, see refuseHardware in main.go) and points HOME and
// every XDG directory at a throwaway tree. Tests that need a link use the
// seams (libraryRunner, eqApplyVia + fakeEqLink), which never go through it.

var (
	testRoot         string
	realBackupDir    string
	realBackupsAtRun []string
)

func dirNames(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestMain(m *testing.M) {
	// Locate the user's real backup dir BEFORE overriding the environment.
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" || !filepath.IsAbs(base) {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".local", "share")
		}
	}
	if base != "" {
		realBackupDir = filepath.Join(base, "qcy-omarchy", "backups")
		realBackupsAtRun = dirNames(realBackupDir)
	}

	root, err := os.MkdirTemp("", "qcyctl-test-")
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

// A VALID eq set (all ten bands flat, a legal band-9 gain of +1) must be
// stopped by the guard: no socket, no BLE, no backup in the real data dir.
func TestHardwareGuardBlocksValidWrite(t *testing.T) {
	if hardwareAllowed {
		t.Fatal("hardwareAllowed is true under test")
	}
	err := cmdEqSet([]string{"0,0,0,0,0,0,0,0,0,1", "--yes"})
	if err == nil || !errors.Is(err, errHardwareDisabled) {
		t.Fatalf("valid eq set was not blocked by the guard: %v", err)
	}
	if err := cmdMasterGain([]string{"1"}); !errors.Is(err, errHardwareDisabled) {
		t.Fatalf("valid master-gain was not blocked: %v", err)
	}
	if _, err := dialDaemon(0); !errors.Is(err, errHardwareDisabled) {
		t.Fatalf("dialDaemon: %v", err)
	}
	if _, err := connectQCY(1); !errors.Is(err, errHardwareDisabled) {
		t.Fatalf("connectQCY: %v", err)
	}
	if err := cmdScan(nil); !errors.Is(err, errHardwareDisabled) {
		t.Fatalf("cmdScan: %v", err)
	}
	after := dirNames(realBackupDir)
	if strings.Join(after, ",") != strings.Join(realBackupsAtRun, ",") {
		t.Fatalf("real backup dir changed during tests: %v -> %v", realBackupsAtRun, after)
	}
}

func TestEnvIsSandboxed(t *testing.T) {
	for _, k := range []string{"HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		if v := os.Getenv(k); !strings.HasPrefix(v, testRoot) {
			t.Errorf("%s=%q is outside the test sandbox %q", k, v, testRoot)
		}
	}
}
