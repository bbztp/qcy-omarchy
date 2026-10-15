package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/protocol"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// Both ears of the hearing experience (slot 11). The 0x22 report carries the
// left ear only. While slot 11 is active the earbuds also answer the queries
// 0x46 (left) and 0x47 (right) with a 0x22 frame holding that ear's curve
// (measured on an N70, firmware 3.0.13; other models that declare the hearing
// test are inferred). With any other slot active the same queries return the
// active EQ, not an ear, so the read is only made for slot 11.

// earReplyTimeout bounds the wait for each ear's reply. A variable so tests
// need not wait it out.
var earReplyTimeout = time.Second

// earSettle is how long after a reply the queue is watched for a second,
// different 0x22 frame. The earbuds answer a query once; two different frames
// mean a notification crossed the reply and the reply cannot be told apart.
var earSettle = 100 * time.Millisecond

// earsFreshFor is how long a successful read is served. The cache key is the
// 0x22 payload, which holds the left ear only, so a right-only change would not
// change it; writes and reconnects drop the cache, and this bounds what else
// (the phone changing the right ear) can leave stale.
const earsFreshFor = 30 * time.Second

// dropEars forgets the cached ears (after a write, on a new link).
func (d *daemon) dropEars() {
	d.earsMu.Lock()
	d.earsKey, d.earsVal, d.earsUntil, d.earsAt = "", nil, time.Time{}, time.Time{}
	d.earsMu.Unlock()
}

// earsRetryAfter is how long a failed read is remembered before the next try.
const earsRetryAfter = 60 * time.Second

// earsReport is the "ears" object of the daemon status: the eight bands of each
// ear, indexed 0-7 within the ear.
type earsReport struct {
	Left  []qcycore.EqBand `json:"left"`
	Right []qcycore.EqBand `json:"right"`
}

// earsFor returns both ears for the 0x22 payload main, or nil when they do not
// apply (not slot 11, model without the hearing test) or could not be read.
// The answer is cached by payload; a failure is remembered for earsRetryAfter.
func (d *daemon) earsFor(main []byte) *earsReport {
	if len(main) < 1 || int(main[0]) != qcycore.HearingTestSlot {
		return nil
	}
	h := d.knownHandler()
	if h == nil || !h.Capabilities().HearingTest {
		return nil
	}
	key := hex.EncodeToString(main)
	d.earsMu.Lock()
	defer d.earsMu.Unlock()
	if d.earsKey == key {
		if d.earsVal != nil && d.now().Sub(d.earsAt) < earsFreshFor {
			return d.earsVal
		}
		if d.earsVal != nil {
			d.earsKey = "" // expired: read again
		}
		if d.now().Before(d.earsUntil) {
			return nil
		}
	}
	e, err := d.readEars(main)
	d.earsKey = key
	d.earsVal = nil
	if err != nil {
		dbg("ears: %v\n", err)
		d.earsVal, d.earsUntil = nil, d.now().Add(earsRetryAfter)
		return nil
	}
	d.earsVal, d.earsAt = e, d.now()
	return e
}

// earQuery sends the query for op and returns the first 0x22 frame that follows
// it. The ears come back as 0x22 frames, so queryLocked (which keeps only the
// opcode it asked for) cannot be used. Callers hold txMu.
func (d *daemon) earQuery(op byte) ([]byte, error) {
	d.drain()
	if err := d.send(protocol.OpQuery, []byte{op}); err != nil {
		return nil, err
	}
	deadline := time.After(earReplyTimeout)
	for {
		select {
		case f := <-d.frames:
			if f.cmd != qcycore.EqOpcode {
				continue
			}
			if len(f.params) == 0 || allFF(f.params) {
				continue
			}
			return d.earSettled(f.params, op)
		case <-deadline:
			return nil, fmt.Errorf("no 0x22 reply to query 0x%02x within %s", op, earReplyTimeout)
		}
	}
}

// earSettled returns first unless another, different 0x22 frame follows it
// within earSettle.
func (d *daemon) earSettled(first []byte, op byte) ([]byte, error) {
	settle := time.After(earSettle)
	for {
		select {
		case f := <-d.frames:
			if f.cmd == qcycore.EqOpcode && len(f.params) > 0 && !allFF(f.params) && !bytes.Equal(f.params, first) {
				return nil, fmt.Errorf("two different 0x22 frames answered query 0x%02x", op)
			}
		case <-settle:
			return first, nil
		}
	}
}

// readEars runs the whole sequence under one txMu hold: 0x22 (opening), query
// 0x46, query 0x47, 0x22 (closing). The result is refused unless the opening is
// the payload the caller saw, the closing equals the opening (the EQ did not
// change meanwhile) and the left reply equals the opening (measured: the 0x22
// report is the left ear).
func (d *daemon) readEars(main []byte) (*earsReport, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}
	opening, err := d.queryLocked(qcycore.EqOpcode, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("opening 0x22: %w", err)
	}
	if !bytes.Equal(opening, main) {
		return nil, errors.New("the EQ changed since the status read")
	}
	om, err := qcycore.DecodeEq(opening)
	if err != nil {
		return nil, err
	}
	if om.Slot != qcycore.HearingTestSlot {
		return nil, fmt.Errorf("active slot is %d", om.Slot)
	}
	lp, err := d.earQuery(protocol.OpHearingLeft)
	if err != nil {
		return nil, err
	}
	rp, err := d.earQuery(protocol.OpHearingRight)
	if err != nil {
		return nil, err
	}
	closing, err := d.queryLocked(qcycore.EqOpcode, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("closing 0x22: %w", err)
	}
	if !bytes.Equal(closing, opening) {
		return nil, errors.New("the EQ changed while the ears were read")
	}
	if !bytes.Equal(lp, opening) {
		return nil, errors.New("the left-ear reply is not what 0x22 reports")
	}
	left, err := earBands(lp, om.MasterGain)
	if err != nil {
		return nil, fmt.Errorf("left: %w", err)
	}
	right, err := earBands(rp, om.MasterGain)
	if err != nil {
		return nil, fmt.Errorf("right: %w", err)
	}
	e := &earsReport{Left: left, Right: right}
	if err := qcycore.ValidateEars(&qcycore.EarCurves{Left: left, Right: right, MasterGain: om.MasterGain}); err != nil {
		return nil, err
	}
	return e, nil
}

// earBands decodes one reply and returns its last qcycore.EarBands bands,
// re-indexed 0-7. The reply must be slot 11 with the given master gain and carry
// at least that many bands.
func earBands(p []byte, master float64) ([]qcycore.EqBand, error) {
	s, err := qcycore.DecodeEq(p)
	if err != nil {
		return nil, err
	}
	if s.Slot != qcycore.HearingTestSlot {
		return nil, fmt.Errorf("reply is slot %d, not %d", s.Slot, qcycore.HearingTestSlot)
	}
	if s.MasterGain != master {
		return nil, fmt.Errorf("reply master gain %.2f differs from %.2f", s.MasterGain, master)
	}
	if len(s.Bands) < qcycore.EarBands {
		return nil, fmt.Errorf("reply has %d bands (want at least %d)", len(s.Bands), qcycore.EarBands)
	}
	out := append([]qcycore.EqBand(nil), s.Bands[len(s.Bands)-qcycore.EarBands:]...)
	for i := range out {
		out[i].Index = i
	}
	return out, nil
}
