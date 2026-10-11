package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestEqOpFromArgs(t *testing.T) {
	op, err := eqOpFromArgs(map[string]interface{}{
		"masterGain": float64(-150),
		"slot":       float64(11),
		"gains":      []interface{}{1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0, -10.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if *op.MasterCenti != -150 || *op.Slot != 11 || op.Gains[9] != -10 {
		t.Fatalf("%+v", op)
	}
	op, err = eqOpFromArgs(map[string]interface{}{"masterGain": 200.0})
	if err != nil || op.Gains != nil || op.Slot != nil || *op.MasterCenti != 200 {
		t.Fatalf("%+v %v", op, err)
	}
	for name, args := range map[string]map[string]interface{}{
		"fractional master": {"masterGain": 1.5},
		"string master":     {"masterGain": "1"},
		"short gains":       {"gains": []interface{}{1.0}},
		"fractional gain":   {"gains": []interface{}{1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0, 0.5}},
		"huge":              {"masterGain": 1e12},
	} {
		if _, err := eqOpFromArgs(args); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestListenSocketDoesNotDisplaceLiveDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "qcyt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	sock := filepath.Join(dir, "qcyctld.sock")

	l, p, err := listenSocket()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if p != sock {
		t.Fatalf("path %s", p)
	}
	fi, _ := os.Stat(sock)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %o", fi.Mode().Perm())
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, _, err := listenSocket(); err == nil {
		t.Fatal("second daemon took over a live socket")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatal("live socket was removed")
	}
}

func TestListenSocketReplacesStaleSocketOnly(t *testing.T) {
	dir, _ := os.MkdirTemp("", "qcyt")
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	sock := filepath.Join(dir, "qcyctld.sock")

	// stale: a socket file with nobody listening
	l0, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l0.(*net.UnixListener).SetUnlinkOnClose(false)
	l0.Close()
	l, _, err := listenSocket()
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	l.Close()
	os.Remove(sock)

	// a regular file at the path is never deleted
	if err := os.WriteFile(sock, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenSocket(); err == nil {
		t.Fatal("regular file replaced")
	}
	if b, _ := os.ReadFile(sock); string(b) != "precious" {
		t.Fatal("regular file damaged")
	}
}
