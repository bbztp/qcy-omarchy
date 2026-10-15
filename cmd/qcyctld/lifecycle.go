package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The earbuds accept ONE BLE central at a time. The daemon therefore holds the
// link only while it is being used: it connects on the first request, keeps the
// link for an idle window so a burst of panel requests shares it, and then
// disconnects cleanly so the phone app can take the device.

const (
	defaultIdle = 6 * time.Second

	busyCode     = "busy"
	notFoundCode = "not_found"
	busyMessage  = "earbuds are in use by another app: close it on this or any other device (phone, tablet, computer) and retry"
)

// errNotFound: nothing answered the scan and no cached address worked. Distinct
// from busy: the earbuds may be off or out of range, or held by an app on
// another device (a connected client stops advertising, so the scan misses it).
var errNotFound = errors.New("earbuds not found: powered off, out of range, or held by an app open on this or another device (they accept one client at a time)")

type lifecycleConfig struct {
	idle        time.Duration // 0 = never drop the link
	busyAttempt int           // connect attempts per request before giving up as busy
	busyBackoff time.Duration // pause between attempts
	busyHold    time.Duration // after giving up, fail fast for this long
}

func defaultLifecycle() lifecycleConfig {
	return lifecycleConfig{
		idle:        defaultIdle,
		busyAttempt: 3,
		busyBackoff: 1500 * time.Millisecond,
		busyHold:    3 * time.Second,
	}
}

// resolveIdle picks the idle window: flag (when >= 0) over env over default.
func resolveIdle(flagSeconds float64, env string) (time.Duration, error) {
	secs := flagSeconds
	if secs < 0 {
		env = strings.TrimSpace(env)
		if env == "" {
			return defaultIdle, nil
		}
		v, err := strconv.ParseFloat(env, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("QCYCTL_IDLE_SECONDS=%q: want a number >= 0", env)
		}
		secs = v
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// busyError is the structured "someone else holds the earbuds" failure.
type busyError struct{ cause error }

func (e *busyError) Error() string { return busyMessage }
func (e *busyError) Unwrap() error { return e.cause }

func isBusy(err error) bool {
	var b *busyError
	return errors.As(err, &b)
}

// errorCode is the machine-readable class sent in the response, "" if none.
func errorCode(err error) string {
	if errors.Is(err, errStopping) {
		return stoppingCode
	}
	if isBusy(err) {
		return busyCode
	}
	if errors.Is(err, errNotFound) {
		return notFoundCode
	}
	return ""
}

// snapshot is the last valid reading with the time it was taken.
type snapshot struct {
	at   time.Time
	data map[string]interface{}
}

// idleStopper is the part of *time.Timer the idle window uses.
type idleStopper interface{ Stop() bool }

func newDaemon(dl dialer, cfg lifecycleConfig) *daemon {
	if cfg.busyAttempt < 1 {
		cfg.busyAttempt = 1
	}
	return &daemon{
		dialer: dl,
		frames: make(chan frame, 64),
		cfg:    cfg,
		now:    time.Now,
		sleep:  time.Sleep,
		afterFunc: func(dur time.Duration, f func()) idleStopper {
			return time.AfterFunc(dur, f)
		},
	}
}

func (d *daemon) curSession() session {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sess
}

func (d *daemon) isConnected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

func (d *daemon) needsProbe() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.probed
}

// waitProbe blocks until the background capability sweep is done, or max.
func (d *daemon) waitProbe(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		p := d.probing
		d.mu.Unlock()
		if !p {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *daemon) knownHandler() ModelHandler {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.handler
}

// onNotify parses a raw notification into the shared frame queue. The send is
// non-blocking: the BLE callback must never stall, and every transaction drains
// the queue before it starts anyway.
func (d *daemon) onNotify(buf []byte) {
	dbg("notify raw=%x\n", buf)
	frames, err := parseFrames(buf)
	if err != nil {
		dbg("notify unparsed: %v\n", err)
	}
	for _, f := range frames {
		select {
		case d.frames <- f:
		default:
		}
	}
}

// busyHeldLocked reports whether the last failed round declared the earbuds
// busy and its window is still open. Only expiry clears it. Caller holds mu.
func (d *daemon) busyHeldLocked() bool {
	if d.busyUntil.IsZero() {
		return false
	}
	if d.now().Before(d.busyUntil) {
		return true
	}
	d.busyUntil = time.Time{}
	return false
}

func errHeldBusy() error {
	return &busyError{cause: errors.New("recent connect attempts found the earbuds busy")}
}

// ensureConnected connects on demand.
func (d *daemon) ensureConnected() error {
	if d.isClosing() {
		return errShuttingDown
	}
	if d.isConnected() {
		return nil
	}
	return d.connect()
}

var errShuttingDown = errors.New("shutting down")

func (d *daemon) isClosing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closing
}

// shutdown refuses new links, then drops the current one. Requests queued on
// connMu behind closeSession see closing and return without dialling.
func (d *daemon) shutdown() {
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()
	d.closeSession()
}

// connect opens the link if it is not open, as one single-flight round under
// connMu: up to busyAttempt dials, busyBackoff apart (the sleeps happen under
// connMu but with mu free). Requests that queue behind the round do not repeat
// it: on waking they see the link up, an open busy window (fail fast), or a
// round that finished after they arrived and failed (its error is returned
// without another scan: a not_found round costs the whole scan window).
func (d *daemon) connect() error {
	d.mu.Lock()
	seq := d.roundSeq
	d.mu.Unlock()

	d.connMu.Lock()
	defer d.connMu.Unlock()

	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return errShuttingDown
	}
	if d.connected {
		d.mu.Unlock()
		return nil
	}
	if d.busyHeldLocked() {
		d.mu.Unlock()
		return errHeldBusy()
	}
	if d.roundSeq != seq && d.lastErr != nil {
		err := d.lastErr
		d.mu.Unlock()
		return err
	}
	d.mu.Unlock()

	d.drain()
	var err error
	for attempt := 1; attempt <= d.cfg.busyAttempt; attempt++ {
		var sess session
		var name string
		sess, name, err = d.dialer.Dial(d.onNotify)
		if err == nil {
			d.dropEars() // the phone may have changed the ears while we were away
			d.mu.Lock()
			d.sess = sess
			d.connected = true
			d.roundSeq++
			d.lastErr = nil
			if name != "" {
				d.deviceName = name
			}
			if vs, ok := d.dialer.(vendorSource); ok {
				d.vendorID, d.haveVendor = vs.VendorID()
			}
			if cs, ok := d.dialer.(colorSource); ok {
				d.colorIndex, d.haveColor = cs.ColorIndex()
			}
			d.mu.Unlock()
			d.detectAndAttachModel()
			return nil
		}
		if !isBusy(err) && looksBusy(err) {
			err = &busyError{cause: err}
		}
		if !isBusy(err) {
			break
		}
		if attempt < d.cfg.busyAttempt {
			d.sleep(d.cfg.busyBackoff)
		}
	}
	d.mu.Lock()
	d.roundSeq++
	d.lastErr = err
	if isBusy(err) {
		d.busyUntil = d.now().Add(d.cfg.busyHold)
	}
	d.mu.Unlock()
	return err
}

// beginRequest registers a user of the link and cancels the idle timer;
// endRequest releases it and, once nobody is left, starts the idle window.
func (d *daemon) beginRequest() {
	d.mu.Lock()
	d.inflight++
	d.idleGen++
	if d.idleTimer != nil {
		d.idleTimer.Stop()
		d.idleTimer = nil
	}
	d.mu.Unlock()
}

func (d *daemon) endRequest() {
	d.mu.Lock()
	d.inflight--
	if d.inflight == 0 && d.cfg.idle > 0 && d.connected {
		d.idleGen++
		gen := d.idleGen
		d.idleTimer = d.afterFunc(d.cfg.idle, func() { d.idleExpire(gen) })
	}
	d.mu.Unlock()
}

// idleExpire drops the link when the window ran out untouched. It waits for
// connMu and txMu, so it never cuts a transaction or a connect attempt short,
// and re-checks the generation afterwards: any request that arrived meanwhile
// has bumped it.
func (d *daemon) idleExpire(gen uint64) {
	d.connMu.Lock()
	defer d.connMu.Unlock()
	d.txMu.Lock()
	defer d.txMu.Unlock()
	d.mu.Lock()
	if gen != d.idleGen || d.inflight > 0 || !d.connected {
		d.mu.Unlock()
		return
	}
	d.idleTimer = nil
	s, h := d.detachLocked()
	d.mu.Unlock()
	dbg("idle window over: releasing the earbuds\n")
	d.releaseSession(s, h)
}

// detachLocked marks the link down and hands back what must be closed.
// Caller holds mu. The model handler is kept: the model does not change
// between connections and capabilities are answered from it.
func (d *daemon) detachLocked() (session, ModelHandler) {
	s := d.sess
	d.sess = nil
	d.connected = false
	return s, d.handler
}

func (d *daemon) releaseSession(s session, h ModelHandler) {
	if s == nil {
		return
	}
	if h != nil {
		h.OnDisconnect(d)
	}
	s.Close()
}

// markDisconnected is called when a write fails: the link is gone (or wedged),
// so close it and let the next request reconnect.
func (d *daemon) markDisconnected() {
	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		return
	}
	s, h := d.detachLocked()
	d.mu.Unlock()
	dbg("DISCONNECTED: write failed\n")
	d.releaseSession(s, h)
}

// closeSession is the explicit "disconnect" command and the shutdown path.
// Like idleExpire it takes connMu then txMu, so it waits for a transaction in
// flight (eqApply, wear: read-modify-write) instead of cutting it in half.
func (d *daemon) closeSession() {
	d.connMu.Lock()
	defer d.connMu.Unlock()
	d.txMu.Lock()
	defer d.txMu.Unlock()
	d.mu.Lock()
	d.busyUntil = time.Time{} // an explicit disconnect forgets the busy window
	if !d.connected {
		d.mu.Unlock()
		return
	}
	d.modelID = ""
	d.handler = nil
	s, h := d.detachLocked()
	d.mu.Unlock()
	d.releaseSession(s, h)
}

// ---- status cache ----

// statusValid: only a reading that carries real device data is worth serving
// later; an empty sweep must not overwrite a good cache.
func statusValid(rep map[string]interface{}) bool {
	_, hasBatt := rep["battery"]
	_, hasANC := rep["anc"]
	return hasBatt || hasANC
}

// cacheGen returns the write generation; a reading taken before a write must
// not be stored after it (see storeStatus).
func (d *daemon) cacheGen() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeGen
}

// invalidateCache drops the cached readings after a write was attempted: the
// device state is no longer what they say, so stale must never serve them.
func (d *daemon) invalidateCache() {
	d.mu.Lock()
	d.writeGen++
	d.lastStatus = nil
	d.lastBattery = nil
	d.mu.Unlock()
	d.dropEars()
}

func (d *daemon) storeStatus(rep map[string]interface{}, gen uint64) {
	if !statusValid(rep) {
		return
	}
	snap := &snapshot{at: d.now(), data: rep}
	d.mu.Lock()
	if gen != d.writeGen {
		d.mu.Unlock()
		return
	}
	d.lastStatus = snap
	if b, ok := rep["battery"].(map[string]interface{}); ok {
		d.lastBattery = &snapshot{at: snap.at, data: b}
	}
	d.mu.Unlock()
}

func (d *daemon) storeBattery(b map[string]interface{}, gen uint64) {
	d.mu.Lock()
	if gen != d.writeGen {
		d.mu.Unlock()
		return
	}
	d.lastBattery = &snapshot{at: d.now(), data: b}
	d.mu.Unlock()
}

// staleFor returns the cached reading for a command that has one, marked
// stale with its age. The cached map is copied, never mutated.
func (d *daemon) staleFor(cmd string) (map[string]interface{}, bool) {
	d.mu.Lock()
	var snap *snapshot
	switch cmd {
	case "status":
		snap = d.lastStatus
	case "battery":
		snap = d.lastBattery
	}
	d.mu.Unlock()
	if snap == nil {
		return nil, false
	}
	out := make(map[string]interface{}, len(snap.data)+2)
	for k, v := range snap.data {
		out[k] = v
	}
	age := d.now().Sub(snap.at)
	if age < 0 {
		age = 0
	}
	out["stale"] = true
	out["ageSeconds"] = int(age.Seconds())
	return out, true
}
