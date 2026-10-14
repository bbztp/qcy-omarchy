package main

import (
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
