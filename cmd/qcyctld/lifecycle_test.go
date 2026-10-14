package main

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/protocol"
)

// fakeDialer / fakeSession stand in for BlueZ: every query frame is answered
// at once with a fixed payload, so a full status sweep needs no hardware.
type fakeDialer struct {
	mu       sync.Mutex
	dials    int
	errs     []error // consumed one per Dial; empty = succeed
	alwaysEr error
	sessions []*fakeSession
	hook     func(*fakeSession) // set on new sessions
	delay    time.Duration      // each Dial takes this long
	times    []time.Time        // start of every Dial
	block    chan struct{}      // when non-nil, every Dial waits for it to be closed
}

func (f *fakeDialer) Dial(onNotify func([]byte)) (session, string, error) {
	f.mu.Lock()
	f.times = append(f.times, time.Now())
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	if f.alwaysEr != nil {
		return nil, "", f.alwaysEr
	}
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		if e != nil {
			return nil, "", e
		}
	}
	s := &fakeSession{notify: onNotify, gate: nil}
	if f.hook != nil {
		f.hook(s)
	}
	f.sessions = append(f.sessions, s)
	return s, "QCY MeloBuds N70", nil
}

func (f *fakeDialer) dialCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials
}

func (f *fakeDialer) closed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sessions {
		if atomic.LoadInt32(&s.closed) == 1 {
			n++
		}
	}
	return n
}

type fakeSession struct {
	notify func([]byte)
	closed int32
	gate   chan struct{} // when non-nil, WriteCmd blocks until it is closed

	active, maxActive int32

	silent  bool // queries are never answered (link wedged by another client)
	muteANC bool // only the ANC query goes unanswered
	battErr bool // the battery characteristic read fails
}

func (s *fakeSession) WriteCmd(pkt []byte) error {
	n := atomic.AddInt32(&s.active, 1)
	defer atomic.AddInt32(&s.active, -1)
	for {
		m := atomic.LoadInt32(&s.maxActive)
		if n <= m || atomic.CompareAndSwapInt32(&s.maxActive, m, n) {
			break
		}
	}
	if s.gate != nil {
		<-s.gate
	}
	time.Sleep(time.Millisecond) // widen the window a concurrent writer would need
	// pkt = FF <len> <op> <plen> <params>; a query is op 0xFE with the asked opcode.
	if len(pkt) >= 5 && pkt[2] == 0xFE {
		op := pkt[4]
		if s.silent || (s.muteANC && op == protocol.OpANC) {
			return nil
		}
		s.notify(buildFrame(op, []byte{1, 2, 3}))
	}
	return nil
}

func (s *fakeSession) Has(k charKind) bool { return k == charBattery || k == charVersion }
func (s *fakeSession) Read(k charKind, buf []byte) (int, error) {
	switch k {
	case charBattery:
		if s.battErr {
			return 0, errors.New("Not Connected")
		}
		return copy(buf, []byte{80, 70, 60}), nil
	case charVersion:
		return copy(buf, []byte{1, 2, 3}), nil
	}
	return 0, errors.New("absent")
}
func (s *fakeSession) WriteKeys([]byte) error { return nil }
func (s *fakeSession) Close()                 { atomic.StoreInt32(&s.closed, 1) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testDaemon(fd *fakeDialer, idle time.Duration) (*daemon, *clock, *[]time.Duration) {
	cfg := defaultLifecycle()
	cfg.idle = idle
	d := newDaemon(fd, cfg)
	clk := &clock{t: time.Unix(1_000_000, 0)}
	d.now = clk.now
	var sleeps []time.Duration
	d.sleep = func(x time.Duration) { sleeps = append(sleeps, x) }
	return d, clk, &sleeps
}

// fakeTimers replaces the idle timer: nothing fires until the test says so,
// so the idle window never races the machine's load.
type fakeTimers struct {
	mu sync.Mutex
	ts []*fakeTimer
}

type fakeTimer struct {
	owner   *fakeTimers
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func useFakeTimers(d *daemon) *fakeTimers {
	ft := &fakeTimers{}
	d.afterFunc = func(dur time.Duration, f func()) idleStopper {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		t := &fakeTimer{owner: ft, d: dur, f: f}
		ft.ts = append(ft.ts, t)
		return t
	}
	return ft
}

func (ft *fakeTimers) count() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.ts)
}

func (ft *fakeTimers) at(i int) *fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.ts[i]
}

// live is the number of timers that were armed and not stopped.
func (ft *fakeTimers) live() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	n := 0
	for _, t := range ft.ts {
		if !t.stopped {
			n++
		}
	}
	return n
}

// fire runs the timer's callback synchronously, as if its window had expired
// (even when it was stopped: a real timer can fire just before Stop).
func (t *fakeTimer) fire() { t.f() }

// waitBlockedInConnect waits until n goroutines are parked on connMu inside
// daemon.connect, so the test knows the requests really queued behind a round
// without guessing with a sleep.
func waitBlockedInConnect(t *testing.T, n int) {
	t.Helper()
	waitFor(t, "requests queued behind the connect round", func() bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		c := 0
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "(*daemon).connect(") && strings.Contains(g, "sync.runtime_SemacquireMutex") {
				c++
			}
		}
		return c >= n
	})
}

// settle waits until no request or background hold (the first-connect
// capability probe) is in flight, i.e. the idle window has been armed.
func settle(t *testing.T, d *daemon) {
	t.Helper()
	waitFor(t, "requests and probes to finish", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.inflight == 0
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustStatus(t *testing.T, d *daemon) map[string]interface{} {
	t.Helper()
	v, err := d.handleRequest(request{Cmd: "status"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return v.(map[string]interface{})
}

func TestConnectsOnFirstRequestOnly(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, time.Hour)
	if fd.dialCount() != 0 || d.isConnected() {
		t.Fatal("connected before any request")
	}
	rep := mustStatus(t, d)
	if fd.dialCount() != 1 || !d.isConnected() {
		t.Fatalf("dials=%d connected=%v", fd.dialCount(), d.isConnected())
	}
	if _, ok := rep["battery"]; !ok {
		t.Fatalf("no battery in %v", rep)
	}
	if _, stale := rep["stale"]; stale {
		t.Fatal("fresh reading marked stale")
	}
}

func TestReusesLinkWithinWindow(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, time.Hour)
	for i := 0; i < 3; i++ {
		mustStatus(t, d)
		if _, err := d.handleRequest(request{Cmd: "noise"}); err != nil {
			t.Fatal(err)
		}
	}
	if fd.dialCount() != 1 {
		t.Fatalf("dials=%d, want 1", fd.dialCount())
	}
}

func TestDisconnectsAfterIdleAndReconnects(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, 40*time.Millisecond)
	ft := useFakeTimers(d)
	mustStatus(t, d)
	settle(t, d)
	if ft.count() != 1 || ft.at(0).d != 40*time.Millisecond {
		t.Fatalf("idle timers armed=%d, want 1 of 40ms", ft.count())
	}
	if !d.isConnected() {
		t.Fatal("link dropped before the idle window ran out")
	}
	ft.at(0).fire()
	if d.isConnected() {
		t.Fatal("link still up after the idle window")
	}
	if fd.closed() != 1 {
		t.Fatalf("closed=%d, want 1 (adapter must be released)", fd.closed())
	}
	mustStatus(t, d)
	if fd.dialCount() != 2 {
		t.Fatalf("dials=%d, want 2", fd.dialCount())
	}
}

func TestEachRequestRestartsIdleTimer(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, 400*time.Millisecond)
	ft := useFakeTimers(d)
	for i := 0; i < 4; i++ {
		if _, err := d.handleRequest(request{Cmd: "noise"}); err != nil {
			t.Fatal(err)
		}
		settle(t, d)
		if ft.count() != i+1 || ft.live() != 1 {
			t.Fatalf("after request %d: armed=%d live=%d, want %d armed and 1 live", i, ft.count(), ft.live(), i+1)
		}
	}
	// A superseded timer firing late (it raced its Stop) must not drop the link.
	for i := 0; i < 3; i++ {
		ft.at(i).fire()
		if !d.isConnected() || fd.dialCount() != 1 {
			t.Fatalf("stale timer %d dropped the link: connected=%v dials=%d", i, d.isConnected(), fd.dialCount())
		}
	}
	ft.at(3).fire()
	if d.isConnected() || fd.closed() != 1 {
		t.Fatalf("current timer did not release: connected=%v closed=%d", d.isConnected(), fd.closed())
	}
}

func TestIdleZeroKeepsLinkForever(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, 0)
	ft := useFakeTimers(d)
	mustStatus(t, d)
	settle(t, d)
	if ft.count() != 0 {
		t.Fatalf("idle=0 armed %d timers", ft.count())
	}
	if !d.isConnected() || fd.closed() != 0 {
		t.Fatal("idle=0 must keep the link")
	}
}

func TestNeverDisconnectsMidTransaction(t *testing.T) {
	fd := &fakeDialer{}
	gate := make(chan struct{})
	fd.hook = func(s *fakeSession) { s.gate = gate }
	d, _, _ := testDaemon(fd, 20*time.Millisecond)
	ft := useFakeTimers(d)
	done := make(chan error, 1)
	go func() {
		_, err := d.handleRequest(request{Cmd: "noise"})
		done <- err
	}()
	// The transaction is parked inside the write.
	waitFor(t, "transaction parked in the write", func() bool {
		fd.mu.Lock()
		defer fd.mu.Unlock()
		return len(fd.sessions) == 1 && atomic.LoadInt32(&fd.sessions[0].active) == 1
	})
	if ft.live() != 0 {
		t.Fatalf("idle window armed during a transaction (%d live)", ft.live())
	}
	if fd.closed() != 0 || !d.isConnected() {
		t.Fatal("link closed under a transaction")
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	settle(t, d)
	if ft.live() != 1 {
		t.Fatalf("idle window not armed after the transaction (%d live)", ft.live())
	}
	ft.at(ft.count() - 1).fire()
	if d.isConnected() || fd.closed() != 1 {
		t.Fatalf("not released after the transaction: connected=%v closed=%d", d.isConnected(), fd.closed())
	}
}

func TestBusyBacksOffBoundedWithoutLocks(t *testing.T) {
	fd := &fakeDialer{alwaysEr: errors.New("connect: Operation already in progress")}
	d, _, sleeps := testDaemon(fd, time.Hour)
	var lockedDuringSleep bool
	d.sleep = func(x time.Duration) {
		*sleeps = append(*sleeps, x)
		if !d.mu.TryLock() {
			lockedDuringSleep = true
		} else {
			d.mu.Unlock()
		}
		// connMu is held on purpose (single-flight round); only mu must be free.
	}
	_, err := d.handleRequest(request{Cmd: "noise"})
	if !isBusy(err) || errorCode(err) != "busy" {
		t.Fatalf("err = %v, want busy", err)
	}
	if err.Error() != busyMessage {
		t.Fatalf("message %q", err.Error())
	}
	if fd.dialCount() != 3 {
		t.Fatalf("dials=%d, want 3", fd.dialCount())
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != 1500*time.Millisecond {
		t.Fatalf("sleeps=%v, want 2 x 1.5s", *sleeps)
	}
	if lockedDuringSleep {
		t.Fatal("mu held during backoff")
	}
}

func TestBusyRecoversWhenOtherClientLeaves(t *testing.T) {
	fd := &fakeDialer{errs: []error{errors.New("In Progress"), nil}}
	d, _, _ := testDaemon(fd, time.Hour)
	mustStatus(t, d)
	if fd.dialCount() != 2 || !d.isConnected() {
		t.Fatalf("dials=%d connected=%v", fd.dialCount(), d.isConnected())
	}
}

func TestBusyHoldFailsFastThenRetries(t *testing.T) {
	fd := &fakeDialer{alwaysEr: errors.New("connect: Operation already in progress")}
	d, clk, _ := testDaemon(fd, time.Hour)
	if _, err := d.handleRequest(request{Cmd: "noise"}); !isBusy(err) {
		t.Fatal(err)
	}
	before := fd.dialCount()
	if _, err := d.handleRequest(request{Cmd: "noise"}); !isBusy(err) {
		t.Fatal(err)
	}
	if fd.dialCount() != before {
		t.Fatal("dialled during the busy hold")
	}
	clk.advance(d.cfg.busyHold - time.Millisecond)
	_, _ = d.handleRequest(request{Cmd: "noise"})
	if fd.dialCount() != before {
		t.Fatal("second round started before busyHold elapsed")
	}
	clk.advance(2 * time.Millisecond)
	_, _ = d.handleRequest(request{Cmd: "noise"})
	if fd.dialCount() != before+d.cfg.busyAttempt {
		t.Fatalf("dials=%d, want a full second round (%d)", fd.dialCount(), before+d.cfg.busyAttempt)
	}
}

func TestNonBusyErrorIsNotRetried(t *testing.T) {
	fd := &fakeDialer{alwaysEr: errors.New("scan: adapter powered off")}
	d, _, sleeps := testDaemon(fd, time.Hour)
	_, err := d.handleRequest(request{Cmd: "noise"})
	if err == nil || isBusy(err) || errorCode(err) != "" {
		t.Fatalf("err=%v", err)
	}
	if fd.dialCount() != 1 || len(*sleeps) != 0 {
		t.Fatalf("dials=%d sleeps=%v", fd.dialCount(), *sleeps)
	}
}

func TestStaleCacheServedWhileBusy(t *testing.T) {
	fd := &fakeDialer{}
	d, clk, _ := testDaemon(fd, time.Hour)
	fresh := mustStatus(t, d)
	if _, ok := fresh["stale"]; ok {
		t.Fatal("fresh reading marked stale")
	}
	// Another app takes the earbuds: link drops, reconnects are refused.
	if _, err := d.handleRequest(request{Cmd: "disconnect"}); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	fd.alwaysEr = errors.New("In Progress")
	fd.mu.Unlock()
	clk.advance(42 * time.Second)

	v, err := d.handleRequest(request{Cmd: "status"})
	if err != nil {
		t.Fatalf("status while busy: %v", err)
	}
	m := v.(map[string]interface{})
	if m["stale"] != true || m["ageSeconds"] != 42 {
		t.Fatalf("stale=%v age=%v", m["stale"], m["ageSeconds"])
	}
	if _, ok := m["battery"]; !ok {
		t.Fatal("cached battery missing")
	}
	if _, ok := fresh["stale"]; ok {
		t.Fatal("cache entry was mutated")
	}
	// A command with no cache gets the structured error instead.
	if _, err := d.handleRequest(request{Cmd: "noise"}); !isBusy(err) {
		t.Fatalf("noise err=%v", err)
	}
	// And so does a status with an empty cache.
	d2, _, _ := testDaemon(&fakeDialer{alwaysEr: errors.New("In Progress")}, time.Hour)
	if _, err := d2.handleRequest(request{Cmd: "status"}); !isBusy(err) {
		t.Fatalf("empty-cache status err=%v", err)
	}
}

func TestConcurrentRequestsSerialised(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := "noise"
			if i%2 == 0 {
				cmd = "status"
			}
			if _, err := d.handleRequest(request{Cmd: cmd}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if fd.dialCount() != 1 {
		t.Fatalf("dials=%d, want 1", fd.dialCount())
	}
	for _, s := range fd.sessions {
		if m := atomic.LoadInt32(&s.maxActive); m != 1 {
			t.Fatalf("max concurrent writers = %d, want 1", m)
		}
	}
}

func TestCapabilitiesAnsweredWithoutConnecting(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, 20*time.Millisecond)
	ft := useFakeTimers(d)
	mustStatus(t, d)
	settle(t, d)
	ft.at(0).fire()
	if d.isConnected() {
		t.Fatal("link still up after the idle window")
	}
	n := fd.dialCount()
	v, err := d.handleRequest(request{Cmd: "capabilities"})
	if err != nil {
		t.Fatal(err)
	}
	if v.(Capabilities).ModelID != "melobuds-n70" || fd.dialCount() != n {
		t.Fatalf("model=%v dials %d->%d", v.(Capabilities).ModelID, n, fd.dialCount())
	}
}

func TestResolveIdle(t *testing.T) {
	cases := []struct {
		flag float64
		env  string
		want time.Duration
		bad  bool
	}{
		{-1, "", 6 * time.Second, false},
		{-1, "10", 10 * time.Second, false},
		{-1, "0", 0, false},
		{3, "10", 3 * time.Second, false},
		{0, "10", 0, false},
		{-1, "abc", 0, true},
		{-1, "-4", 0, true},
	}
	for _, c := range cases {
		got, err := resolveIdle(c.flag, c.env)
		if (err != nil) != c.bad || (!c.bad && got != c.want) {
			t.Errorf("resolveIdle(%v,%q)=%v,%v", c.flag, c.env, got, err)
		}
	}
}

func TestLooksBusy(t *testing.T) {
	for _, s := range []string{"Operation already in progress", "org.bluez.Error.InProgress", "org.bluez.Error.AlreadyConnected", "Resource busy", "connection refused"} {
		if !looksBusy(errors.New(s)) {
			t.Errorf("%q not busy", s)
		}
	}
	for _, s := range []string{"Not Connected", "discover service a001: timeout", "connect timed out", "operation aborted", "QCY service a001 not found on LE device"} {
		if looksBusy(errors.New(s)) {
			t.Errorf("%q must not be busy (it must reach the scan)", s)
		}
	}
	if looksBusy(errNotFound) {
		t.Error("errNotFound classified busy")
	}
}

func TestNotFoundIsNotBusy(t *testing.T) {
	fd := &fakeDialer{alwaysEr: errNotFound}
	d, _, sleeps := testDaemon(fd, time.Hour)
	_, err := d.handleRequest(request{Cmd: "noise"})
	if err == nil || isBusy(err) || errorCode(err) != "not_found" {
		t.Fatalf("err=%v code=%q", err, errorCode(err))
	}
	if err.Error() == busyMessage || fd.dialCount() != 1 || len(*sleeps) != 0 {
		t.Fatalf("msg=%q dials=%d sleeps=%v", err.Error(), fd.dialCount(), *sleeps)
	}
}

func TestConcurrentBusyRequestsShareOneWait(t *testing.T) {
	fd := &fakeDialer{alwaysEr: errors.New("connect: Operation already in progress"), delay: 20 * time.Millisecond}
	d, _, _ := testDaemon(fd, time.Hour)
	const backoff = 40 * time.Millisecond
	d.cfg.busyBackoff = backoff
	d.sleep = time.Sleep
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.handleRequest(request{Cmd: "noise"}); !isBusy(err) {
				t.Errorf("err=%v, want busy", err)
			}
		}()
	}
	wg.Wait()
	fd.mu.Lock()
	times := append([]time.Time(nil), fd.times...)
	fd.mu.Unlock()
	if len(times) != d.cfg.busyAttempt {
		t.Fatalf("dials=%d, want exactly %d (one shared round)", len(times), d.cfg.busyAttempt)
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < backoff {
			t.Fatalf("dial %d started %v after the previous, want >= %v", i, gap, backoff)
		}
	}
}

func TestConcurrentNotFoundRequestsShareOneScan(t *testing.T) {
	release := make(chan struct{})
	fd := &fakeDialer{alwaysEr: errNotFound, block: release}
	d, _, _ := testDaemon(fd, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.handleRequest(request{Cmd: "noise"}); errorCode(err) != notFoundCode {
				t.Errorf("err=%v, want not_found", err)
			}
		}()
	}
	// One request is inside the scan; the other seven are queued on connMu.
	waitBlockedInConnect(t, 7)
	close(release)
	wg.Wait()
	if got := fd.dialCount(); got != 1 {
		t.Fatalf("scans=%d, want 1", got)
	}
}

func TestWriteInvalidatesStaleCache(t *testing.T) {
	for _, cmd := range []string{"set", "wear", "eqMasterGain", "keysSet"} {
		fd := &fakeDialer{}
		d, _, _ := testDaemon(fd, time.Hour)
		mustStatus(t, d)
		if _, ok := d.staleFor("status"); !ok {
			t.Fatal("cache not filled")
		}
		args := map[string]interface{}{"action": "anc", "params": []interface{}{float64(1), float64(0), float64(0)}}
		switch cmd {
		case "wear":
			args = map[string]interface{}{"state": float64(1)}
		case "eqMasterGain":
			args = map[string]interface{}{"masterGain": float64(0)}
		case "keysSet":
			args = map[string]interface{}{"raw": []interface{}{float64(1), float64(2)}}
		}
		_, _ = d.handleRequest(request{Cmd: cmd, Args: args}) // outcome irrelevant: a write was attempted
		if _, ok := d.staleFor("status"); ok {
			t.Errorf("%s: status still served stale after a write", cmd)
		}
		if _, ok := d.staleFor("battery"); ok {
			t.Errorf("%s: battery still served stale after a write", cmd)
		}
	}
}

func TestStatusReadBeforeWriteIsNotCached(t *testing.T) {
	d, _, _ := testDaemon(&fakeDialer{}, time.Hour)
	gen := d.cacheGen()
	d.invalidateCache() // a write lands while the sweep was in flight
	d.storeStatus(map[string]interface{}{"battery": map[string]interface{}{"left": 1}}, gen)
	if _, ok := d.staleFor("status"); ok {
		t.Fatal("pre-write reading cached after the write")
	}
}

func TestFailedConnectDoesNotInvalidateCache(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, time.Hour)
	mustStatus(t, d)
	_, _ = d.handleRequest(request{Cmd: "disconnect"})
	fd.mu.Lock()
	fd.alwaysEr = errors.New("In Progress")
	fd.mu.Unlock()
	if _, err := d.handleRequest(request{Cmd: "set", Args: map[string]interface{}{"action": "anc", "params": []interface{}{float64(1)}}}); !isBusy(err) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := d.staleFor("status"); !ok {
		t.Fatal("a write that never reached the device dropped the cache")
	}
}

func TestDisconnectWaitsForTransaction(t *testing.T) {
	fd := &fakeDialer{}
	gate := make(chan struct{})
	fd.hook = func(s *fakeSession) { s.gate = gate }
	d, _, _ := testDaemon(fd, time.Hour)
	done := make(chan error, 1)
	go func() {
		_, err := d.handleRequest(request{Cmd: "noise"})
		done <- err
	}()
	waitFor(t, "transaction in flight", func() bool {
		fd.mu.Lock()
		defer fd.mu.Unlock()
		return len(fd.sessions) == 1 && atomic.LoadInt32(&fd.sessions[0].active) == 1
	})
	disc := make(chan struct{})
	go func() {
		_, _ = d.handleRequest(request{Cmd: "disconnect"})
		close(disc)
	}()
	select {
	case <-disc:
		t.Fatal("disconnect cut a transaction in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if fd.closed() != 0 {
		t.Fatal("session closed under a transaction")
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-disc:
	case <-time.After(15 * time.Second):
		t.Fatal("disconnect never completed")
	}
	if fd.closed() != 1 {
		t.Fatalf("closed=%d", fd.closed())
	}
}

// After shutdown starts, a request queued behind connMu must not dial.
func TestShutdownRefusesNewLink(t *testing.T) {
	fd := &fakeDialer{}
	d, _, _ := testDaemon(fd, time.Hour)
	d.shutdown()
	err := d.ensureConnected()
	if err == nil || err.Error() != "shutting down" {
		t.Fatalf("err=%v, want shutting down", err)
	}
	if err := d.connect(); err == nil || err.Error() != "shutting down" {
		t.Fatalf("connect err=%v", err)
	}
	if n := fd.dialCount(); n != 0 {
		t.Fatalf("dials=%d, want 0", n)
	}
}

// A session whose every read and query fails (another client took the
// earbuds) must not report success with an empty status: the request fails
// not_found and the dead session is dropped so the next one reconnects.
func TestStatusDeadSessionIsNotFoundAndDropsSession(t *testing.T) {
	fd := &fakeDialer{}
	fd.hook = func(s *fakeSession) { s.silent, s.battErr = true, true }
	d, _, _ := testDaemon(fd, time.Hour)
	start := time.Now()
	v, err := d.handleRequest(request{Cmd: "status"})
	if err == nil {
		t.Fatalf("status succeeded with %v", v)
	}
	if errorCode(err) != notFoundCode {
		t.Fatalf("code=%q err=%v", errorCode(err), err)
	}
	if d.isConnected() {
		t.Fatal("dead session still marked connected")
	}
	if fd.closed() != 1 {
		t.Fatalf("closed sessions=%d", fd.closed())
	}
	// Aborted after the battery and ANC queries, not after the whole sweep.
	if el := time.Since(start); el > 12*time.Second {
		t.Fatalf("status took %v", el)
	}
	// The next request dials again instead of reusing the dead session.
	fd.hook = nil
	if rep := mustStatus(t, d); rep["battery"] == nil || fd.dialCount() != 2 {
		t.Fatalf("no recovery: dials=%d rep=%v", fd.dialCount(), rep)
	}
}

// One essential field is enough: a partial status stays a success and keeps
// the session.
func TestStatusPartialBatteryOnlyStaysSuccess(t *testing.T) {
	fd := &fakeDialer{}
	fd.hook = func(s *fakeSession) { s.muteANC = true }
	d, _, _ := testDaemon(fd, time.Hour)
	rep := mustStatus(t, d)
	if rep["battery"] == nil {
		t.Fatalf("no battery in %v", rep)
	}
	if _, ok := rep["anc"]; ok {
		t.Fatal("anc present without an answer")
	}
	if !d.isConnected() {
		t.Fatal("partial status dropped the session")
	}
}
