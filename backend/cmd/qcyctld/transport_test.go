package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tinygo.org/x/bluetooth"
)

// fakeConnect counts Connect calls; each blocks until release is closed.
type fakeConnect struct {
	connects, disconnects int32
	release               chan struct{}
}

func newFakeConnect() *fakeConnect { return &fakeConnect{release: make(chan struct{})} }

func (f *fakeConnect) connect() (bluetooth.Device, error) {
	atomic.AddInt32(&f.connects, 1)
	<-f.release
	return bluetooth.Device{}, nil
}
func (f *fakeConnect) disconnect(bluetooth.Device) { atomic.AddInt32(&f.disconnects, 1) }

func waitInt32(t *testing.T, what string, p *int32, want int32) {
	t.Helper()
	waitFor(t, what, func() bool { return atomic.LoadInt32(p) == want })
}

// An abandoned dial completing with nobody waiting is disconnected, once.
func TestOrphanDialIsDisconnectedOnce(t *testing.T) {
	var g dialGate
	f := newFakeConnect()
	if _, err := g.get(f.connect, f.disconnect, 20*time.Millisecond); err == nil {
		t.Fatal("want timeout")
	}
	close(f.release)
	waitInt32(t, "orphan disconnect", &f.disconnects, 1)
	time.Sleep(30 * time.Millisecond)
	if c, d := atomic.LoadInt32(&f.connects), atomic.LoadInt32(&f.disconnects); c != 1 || d != 1 {
		t.Fatalf("connects=%d disconnects=%d, want 1/1", c, d)
	}
}

// A new caller joins the pending Connect of an expired one instead of
// emitting another; it adopts the device and nothing is disconnected.
func TestJoinPendingDialAdoptsDevice(t *testing.T) {
	var g dialGate
	f := newFakeConnect()
	if _, err := g.get(f.connect, f.disconnect, 20*time.Millisecond); err == nil {
		t.Fatal("A: want timeout")
	}
	var adopted int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := g.get(f.connect, f.disconnect, 5*time.Second); err == nil {
			atomic.AddInt32(&adopted, 1)
		}
	}()
	time.Sleep(30 * time.Millisecond) // let B register
	close(f.release)
	<-done
	time.Sleep(30 * time.Millisecond)
	c, d, a := atomic.LoadInt32(&f.connects), atomic.LoadInt32(&f.disconnects), atomic.LoadInt32(&adopted)
	if c != 1 || d != 0 || a != 1 || c-a-d != 0 {
		t.Fatalf("connects=%d disconnects=%d adopted=%d", c, d, a)
	}
}

// The Connect completing right at the waiter's deadline is adopted exactly
// once, never dropped and never both adopted and disconnected.
func TestCompletionAtDeadlineAdoptedExactlyOnce(t *testing.T) {
	for i := 0; i < 200; i++ {
		var g dialGate
		f := newFakeConnect()
		var wg sync.WaitGroup
		var adopted int32
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.get(f.connect, f.disconnect, 2*time.Millisecond); err == nil {
				atomic.AddInt32(&adopted, 1)
			}
		}()
		time.Sleep(time.Duration(1500+i*5) * time.Microsecond) // sweep around the deadline
		close(f.release)
		wg.Wait()
		// the stray timed-out case: wait for a possible orphan disconnect
		time.Sleep(5 * time.Millisecond)
		a, d := atomic.LoadInt32(&adopted), atomic.LoadInt32(&f.disconnects)
		if a+d != 1 {
			t.Fatalf("iter %d: adopted=%d disconnected=%d, want exactly one", i, a, d)
		}
	}
}

// A pending older than its validity is replaced; the old one's completion is
// ignored without disconnecting.
func TestExpiredPendingReplacedAndLateResultIgnored(t *testing.T) {
	var g dialGate
	g.validity = 10 * time.Millisecond
	old := newFakeConnect()
	if _, err := g.get(old.connect, old.disconnect, 5*time.Millisecond); err == nil {
		t.Fatal("want timeout")
	}
	time.Sleep(20 * time.Millisecond)
	fresh := newFakeConnect()
	close(fresh.release)
	if _, err := g.get(fresh.connect, fresh.disconnect, time.Second); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&fresh.connects) != 1 {
		t.Fatal("expired pending was joined instead of replaced")
	}
	close(old.release)
	time.Sleep(30 * time.Millisecond)
	if d := atomic.LoadInt32(&old.disconnects) + atomic.LoadInt32(&fresh.disconnects); d != 0 {
		t.Fatalf("late result of a replaced pending disconnected a device (%d)", d)
	}
}

// A hanging Disconnect of an orphan must not hold g.mu: a new get returns by
// its own timeout, and no second Connect is emitted while the Disconnect runs.
func TestHangingOrphanDisconnectDoesNotBlockGet(t *testing.T) {
	hang := make(chan struct{})
	entered := make(chan struct{})
	disc := func(bluetooth.Device) {
		close(entered)
		<-hang
	}
	var g2 dialGate
	f2 := newFakeConnect()
	if _, err := g2.get(f2.connect, disc, 10*time.Millisecond); err == nil {
		t.Fatal("want timeout")
	}
	close(f2.release)
	<-entered

	done := make(chan error, 1)
	go func() {
		_, err := g2.get(f2.connect, disc, 30*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err != errConnectTimeout {
			t.Fatalf("err=%v, want errConnectTimeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("get stuck behind the hanging disconnect")
	}
	if c := atomic.LoadInt32(&f2.connects); c != 1 {
		t.Fatalf("connects=%d, want 1 while disconnect pending", c)
	}
	close(hang)
	waitFor(t, "gate free", func() bool {
		g2.mu.Lock()
		defer g2.mu.Unlock()
		return g2.disc == nil
	})
}
