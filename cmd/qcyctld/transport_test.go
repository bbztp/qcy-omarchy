package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/protocol"
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
	// Wait until B has joined A's pending Connect (A gave up: waiters 0 -> 1).
	waitFor(t, "B to join the pending dial", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.cur == nil {
			return false
		}
		g.cur.mu.Lock()
		defer g.cur.mu.Unlock()
		return g.cur.waiters == 1
	})
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
		// Either the waiter adopted the device, or the orphan disconnect is
		// on its way (its goroutine may be slow to run under load).
		waitFor(t, "adoption or orphan disconnect", func() bool {
			return atomic.LoadInt32(&adopted)+atomic.LoadInt32(&f.disconnects) >= 1
		})
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
	var clkMu sync.Mutex
	now := time.Unix(1_000_000, 0)
	g.now = func() time.Time {
		clkMu.Lock()
		defer clkMu.Unlock()
		return now
	}
	g.validity = 10 * time.Millisecond
	old := newFakeConnect()
	if _, err := g.get(old.connect, old.disconnect, 5*time.Millisecond); err == nil {
		t.Fatal("want timeout")
	}
	clkMu.Lock()
	now = now.Add(20 * time.Millisecond) // the pending outlives its validity
	clkMu.Unlock()
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
	case <-time.After(15 * time.Second):
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

// The daemon's framing is protocol.Block: bytes identical to the hand-built
// envelope it used before, for every opcode it writes.
func TestBuildFrameMatchesLegacyEnvelope(t *testing.T) {
	cases := []struct {
		op     byte
		params []byte
	}{
		{protocol.OpANC, []byte{3, 1, 0}}, {protocol.OpGame, []byte{1}}, {protocol.OpSleep, []byte{2}},
		{protocol.OpFit, []byte{1, 1, 1}}, {protocol.OpAutoOff, []byte{4, 60, 0, 0, 0}},
		{protocol.OpBalance, []byte{50}}, {protocol.OpPromptVolume, []byte{8, 0}},
		{protocol.OpLDAC, []byte{1}}, {protocol.OpMultipoint, []byte{2}}, {protocol.OpWind, []byte{0}},
		{protocol.OpWear, []byte{1, 2, 3}}, {protocol.OpSpatial, []byte{1}},
		{protocol.OpHearingMode, []byte{1}}, {protocol.OpWearSensitivity, []byte{3}},
		{protocol.OpQuery, []byte{protocol.OpANC}},
	}
	for _, c := range cases {
		want := append([]byte{0xff, byte(len(c.params) + 2), c.op, byte(len(c.params))}, c.params...)
		if got := buildFrame(c.op, c.params); !bytes.Equal(got, want) {
			t.Errorf("0x%02x: got %x want %x", c.op, got, want)
		}
	}
}

func TestSaveCachedAddrPermissions(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(cacheFilePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	saveCachedAddr(bluetooth.Address{}, "x", 0, false, 0, false)
	for p, want := range map[string]os.FileMode{filepath.Dir(cacheFilePath()): 0o700, cacheFilePath(): 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Fatalf("%s mode %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
}
