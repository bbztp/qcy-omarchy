package main

import (
	"encoding/binary"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/qcycore"
	"github.com/bbztp/qcy-omarchy/internal/qcycore/models"
)

// earSession answers the 0x46/0x47 queries with a 0x22 frame, like the earbuds
// do while slot 11 is active. left/right are the payloads to answer; a nil
// payload means no reply. stray, if set, is injected as a 0x22 frame right
// before each reply; afterRight, if set, replaces the EQ state once the
// right ear was answered (the EQ changing mid-read).
type earSession struct {
	*eqSession
	qmu        sync.Mutex
	left       []byte
	right      []byte
	stray      []byte
	afterRight []byte
	queries    []byte
}

func (s *earSession) WriteCmd(pkt []byte) error {
	if len(pkt) >= 5 && pkt[2] == 0xFE && (pkt[4] == 0x46 || pkt[4] == 0x47) {
		s.qmu.Lock()
		s.queries = append(s.queries, pkt[4])
		reply, stray, after := s.left, s.stray, []byte(nil)
		if pkt[4] == 0x47 {
			reply, after = s.right, s.afterRight
		}
		s.qmu.Unlock()
		s.eqSession.mu.Lock()
		n := s.eqSession.notify
		s.eqSession.mu.Unlock()
		if stray != nil {
			n(buildFrame(qcycore.EqOpcode, stray))
		}
		if reply != nil {
			n(buildFrame(qcycore.EqOpcode, reply))
		}
		if after != nil {
			s.eqSession.mu.Lock()
			s.eqSession.state = after
			s.eqSession.mu.Unlock()
		}
		return nil
	}
	return s.eqSession.WriteCmd(pkt)
}

func (s *earSession) queried() []byte {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	return append([]byte(nil), s.queries...)
}

type earDialer struct{ s *earSession }

func (f earDialer) Dial(onNotify func([]byte)) (session, string, error) {
	f.s.eqSession.mu.Lock()
	f.s.eqSession.notify = onNotify
	f.s.eqSession.mu.Unlock()
	return f.s, "QCY MeloBuds N70", nil
}

// shiftEar returns a copy of a slot-11 payload with every one of the last
// eight bands raised by centi (0.01 dB units).
func shiftEar(t *testing.T, p []byte, centi int) []byte {
	t.Helper()
	out := append([]byte(nil), p...)
	s, err := qcycore.DecodeEq(p)
	if err != nil || len(s.Bands) < qcycore.EarBands {
		t.Fatalf("fixture: %v", err)
	}
	for _, b := range s.Bands[len(s.Bands)-qcycore.EarBands:] {
		off := 3 + b.Index*7 + 2
		g := int16(binary.LittleEndian.Uint16(out[off:])) + int16(centi)
		binary.LittleEndian.PutUint16(out[off:], uint16(g))
	}
	return out
}

func earDaemon(t *testing.T, state []byte, mod func(*earSession)) (*daemon, *earSession, *clock) {
	t.Helper()
	es := &earSession{eqSession: &eqSession{state: append([]byte(nil), state...)}}
	es.left = append([]byte(nil), state...)
	es.right = shiftEar(t, state, 300)
	if mod != nil {
		mod(es)
	}
	d, clk, _ := testDaemon(&fakeDialer{}, time.Hour)
	d.dialer = earDialer{es}
	return d, es, clk
}

func statusEars(t *testing.T, d *daemon) *earsReport {
	t.Helper()
	e, _ := mustStatus(t, d)["ears"].(*earsReport)
	settle(t, d)
	return e
}

func TestStatusCarriesBothEarsOnSlot11(t *testing.T) {
	state := mustHexBytes(t, realCustom11)
	d, es, _ := earDaemon(t, state, nil)
	e := statusEars(t, d)
	if e == nil {
		t.Fatal("no ears on slot 11")
	}
	if len(e.Left) != qcycore.EarBands || len(e.Right) != qcycore.EarBands {
		t.Fatalf("%d/%d bands", len(e.Left), len(e.Right))
	}
	for i := range e.Left {
		if e.Left[i].Index != i || e.Right[i].Index != i || e.Left[i].Freq != e.Right[i].Freq {
			t.Fatalf("band %d: %+v %+v", i, e.Left[i], e.Right[i])
		}
		if d := e.Right[i].Gain - e.Left[i].Gain; d < 2.99 || d > 3.01 {
			t.Fatalf("band %d: right-left = %v", i, d)
		}
	}
	snap, _ := qcycore.DecodeEq(state)
	want := snap.Bands[len(snap.Bands)-qcycore.EarBands:]
	if e.Left[0].Freq != want[0].Freq || e.Left[0].Gain != want[0].Gain {
		t.Fatalf("left is not the last eight bands of 0x22: %+v vs %+v", e.Left[0], want[0])
	}
	if q := es.queried(); string(q) != "\x46\x47" {
		t.Fatalf("queries %x", q)
	}
	// Same payload: served from the cache, nothing is asked again.
	if e2 := statusEars(t, d); e2 == nil || len(es.queried()) != 2 {
		t.Fatalf("cache miss: %x", es.queried())
	}
}

func TestNoEarsOutsideSlot11(t *testing.T) {
	state := mustHexBytes(t, realCustom11)
	state[0] = 255
	d, es, _ := earDaemon(t, state, nil)
	if e := statusEars(t, d); e != nil {
		t.Fatalf("ears on slot 255: %+v", e)
	}
	if len(es.queried()) != 0 {
		t.Fatalf("0x46/0x47 asked on slot 255: %x", es.queried())
	}
}

func TestEarsRefusedWhenTheReadIsNotTrustworthy(t *testing.T) {
	base := mustHexBytes(t, realCustom11)
	wrongSlot := func(p []byte) []byte { q := append([]byte(nil), p...); q[0] = 255; return q }
	cases := map[string]func(*earSession){
		"right reply with another slot":      func(s *earSession) { s.right = wrongSlot(s.right) },
		"no right reply":                     func(s *earSession) { s.right = nil },
		"left reply is not 0x22":             func(s *earSession) { s.left = shiftEar(t, base, 100) },
		"stray 0x22 makes the read rejected": func(s *earSession) { s.stray = shiftEar(t, base, -500) },
		"EQ changed while reading":           func(s *earSession) { s.afterRight = shiftEar(t, base, 700) },
		"right with another master": func(s *earSession) {
			s.right = append([]byte(nil), s.right...)
			binary.LittleEndian.PutUint16(s.right[1:], 0)
		},
		"right with too few bands": func(s *earSession) {
			s.right = append([]byte(nil), s.right[:3+7*5]...)
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			d, _, _ := earDaemon(t, base, mod)
			if e := statusEars(t, d); e != nil {
				t.Fatalf("ears reported: %+v", e)
			}
		})
	}
}

func TestFailedEarReadIsRetriedOnlyAfterTheTTL(t *testing.T) {
	base := mustHexBytes(t, realCustom11)
	d, es, clk := earDaemon(t, base, func(s *earSession) { s.right = nil })
	if e := statusEars(t, d); e != nil {
		t.Fatal("ears on a failed read")
	}
	n := len(es.queried())
	if n == 0 {
		t.Fatal("nothing was asked")
	}
	statusEars(t, d)
	if len(es.queried()) != n {
		t.Fatalf("retried inside the TTL: %x", es.queried())
	}
	clk.advance(earsRetryAfter + time.Second)
	es.qmu.Lock()
	es.right = shiftEar(t, base, 300)
	es.qmu.Unlock()
	if e := statusEars(t, d); e == nil {
		t.Fatal("not retried after the TTL")
	}
}

// Ears are read only where the panel declares the hearing test; no model id is
// named here: every vendorId of the snapshot is held to the capability.
func TestEarsOnlyWhereTheHearingTestIsDeclared(t *testing.T) {
	base := mustHexBytes(t, realCustom11)
	d, es, _ := earDaemon(t, base, nil)
	mustStatus(t, d)
	settle(t, d)
	var with, without int
	for _, p := range models.All() {
		v := p.VendorID
		t.Run(strconv.Itoa(int(v)), func(t *testing.T) {
			h := GetHandler(detectModel(v, true, "", ""))
			d.mu.Lock()
			d.handler = h
			d.mu.Unlock()
			d.earsMu.Lock()
			d.earsKey, d.earsVal = "", nil
			d.earsMu.Unlock()
			before := len(es.queried())
			e := d.earsFor(base)
			if got, want := e != nil, h.Capabilities().HearingTest; got != want {
				t.Fatalf("ears=%v but hearingTest=%v", got, want)
			}
			if e == nil && len(es.queried()) != before {
				t.Fatal("asked 0x46/0x47 on a model without the hearing test")
			}
			if e != nil {
				with++
			} else {
				without++
			}
		})
	}
	if with+without == 0 {
		t.Fatal("no vendorId was exercised")
	}
}

func TestEarsCacheDroppedByWriteReconnectAndTTL(t *testing.T) {
	base := mustHexBytes(t, realCustom11)
	d, es, clk := earDaemon(t, base, nil)
	if statusEars(t, d) == nil {
		t.Fatal("no ears")
	}
	n := len(es.queried())
	// A right-only change (the left, hence 0x22, stays the same) after a 0x47 write.
	es.qmu.Lock()
	es.right = shiftEar(t, base, 500)
	es.qmu.Unlock()
	if statusEars(t, d); len(es.queried()) != n {
		t.Fatal("served from the cache before any write")
	}
	if _, err := d.handleRequest(request{Cmd: "hearingWrite", Args: map[string]interface{}{
		"ear": "right", "gains": gains8(0, 0, 0, 0, 0, 0, 0, 0), "confirm": true}}); err != nil {
		t.Fatal(err)
	}
	e := statusEars(t, d)
	if len(es.queried()) == n || e == nil || e.Right[0].Gain-e.Left[0].Gain < 4.9 {
		t.Fatalf("not re-read after a 0x47 write: %x %+v", es.queried(), e)
	}
	// Reconnect.
	n = len(es.queried())
	d.markDisconnected()
	mustStatus(t, d)
	settle(t, d)
	if len(es.queried()) == n {
		t.Fatal("not re-read after a reconnect")
	}
	// TTL.
	n = len(es.queried())
	clk.advance(earsFreshFor + time.Second)
	statusEars(t, d)
	if len(es.queried()) == n {
		t.Fatal("served past the TTL")
	}
}
