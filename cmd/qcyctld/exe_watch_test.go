package main

import (
	"os"
	"testing"
	"time"
)

func TestShouldExitForGoneExecutable(t *testing.T) {
	d := newDaemon(&fakeDialer{}, defaultLifecycle())
	present := func() bool { return false }
	gone := func() bool { return true }

	if d.shouldExitForGoneExecutable(present) {
		t.Fatal("exit requested while the executable exists")
	}
	d.beginRequest()
	if d.shouldExitForGoneExecutable(gone) {
		t.Fatal("exit requested with a request in flight")
	}
	d.endRequest()
	if !d.shouldExitForGoneExecutable(gone) {
		t.Fatal("no exit although the executable is gone and the daemon is idle")
	}
}

func TestWatchExecutableStopsOnceWhenGone(t *testing.T) {
	d := newDaemon(&fakeDialer{}, defaultLifecycle())
	tick := make(chan time.Time, 3)
	stopped := make(chan struct{}, 3)
	done := make(chan struct{})
	go func() {
		d.watchExecutable(tick, func() bool { return true }, func() { stopped <- struct{}{} })
		close(done)
	}()
	tick <- time.Now()
	select {
	case <-done:
		t.Fatal("stopped after a single tick")
	case <-time.After(50 * time.Millisecond):
	}
	tick <- time.Now()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not stop")
	}
	if len(stopped) != 1 {
		t.Fatalf("stop called %d times, want 1", len(stopped))
	}
}

func TestExecutableGoneFalseForRunningTestBinary(t *testing.T) {
	if executableGone() {
		t.Fatal("the running test binary exists; executableGone must be false")
	}
}

func TestPluginDirGone(t *testing.T) {
	exe := "/data/qcy-omarchy/bin/0.1.2/qcyctld"
	read := func(content string, err error) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if p != "/data/qcy-omarchy/bin/0.1.2/plugin-dir" {
				t.Fatalf("read %q", p)
			}
			return []byte(content), err
		}
	}
	stat := func(err error) func(string) (os.FileInfo, error) {
		return func(p string) (os.FileInfo, error) {
			if p != "/plugins/qcy" {
				t.Fatalf("stat %q", p)
			}
			return nil, err
		}
	}
	if !pluginDirGone(exe, read("/plugins/qcy\n", nil), stat(os.ErrNotExist)) {
		t.Fatal("plugin dir missing: want gone")
	}
	if pluginDirGone(exe, read("/plugins/qcy\n", nil), stat(nil)) {
		t.Fatal("plugin dir present: want not gone")
	}
	if pluginDirGone(exe, read("/plugins/qcy\n", nil), stat(os.ErrPermission)) {
		t.Fatal("permission error must not count as gone")
	}
	if pluginDirGone(exe, read("", os.ErrNotExist), stat(os.ErrNotExist)) {
		t.Fatal("no plugin-dir record (CLI-only install): never gone")
	}
	if pluginDirGone(exe, read("relative/dir", nil), stat(os.ErrNotExist)) {
		t.Fatal("relative record must be ignored")
	}
}

func TestWatchStopsWhenPluginDirGoneButNotInFlight(t *testing.T) {
	d := newDaemon(&fakeDialer{}, defaultLifecycle())
	pluginGone := true
	gone := func() bool { return pluginGone }
	d.beginRequest()
	if d.shouldExitForGoneExecutable(gone) {
		t.Fatal("exit requested with a request in flight")
	}
	d.endRequest()
	if !d.shouldExitForGoneExecutable(gone) {
		t.Fatal("no exit although the plugin dir is gone and the daemon is idle")
	}
}

func TestWatchResetsWhenOwnerReturns(t *testing.T) {
	d := newDaemon(&fakeDialer{}, defaultLifecycle())
	seq := []bool{true, false, true, true}
	i := 0
	gone := func() bool { v := seq[i]; i++; return v }
	tick := make(chan time.Time, len(seq))
	for range seq {
		tick <- time.Now()
	}
	close(tick)
	stops := 0
	d.watchExecutable(tick, gone, func() { stops++ })
	if stops != 1 || i != 4 {
		t.Fatalf("stops=%d consumed=%d, want 1 stop only on the 4th tick", stops, i)
	}
}

func TestPluginDirGoneKeepsEdgeSpaces(t *testing.T) {
	read := func(string) ([]byte, error) { return []byte("/plugins/qcy \n"), nil }
	var got string
	stat := func(p string) (os.FileInfo, error) { got = p; return nil, os.ErrNotExist }
	if !pluginDirGone("/d/bin/1/qcyctld", read, stat) || got != "/plugins/qcy " {
		t.Fatalf("stat path %q, want the path with its trailing space", got)
	}
}
