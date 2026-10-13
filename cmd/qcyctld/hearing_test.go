package main

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/hearing"
)

// hearSession records every raw packet and optionally echoes 0x45/0x46/0x47.
type hearSession struct {
	*eqSession
	pmu  sync.Mutex
	pkts [][]byte
	ack  bool
}

func (s *hearSession) WriteCmd(pkt []byte) error {
	if len(pkt) >= 5 && pkt[2] >= 0x45 && pkt[2] <= 0x47 {
		s.pmu.Lock()
		s.pkts = append(s.pkts, append([]byte(nil), pkt...))
		s.pmu.Unlock()
		if s.ack {
			s.eqSession.mu.Lock()
			n := s.eqSession.notify
			s.eqSession.mu.Unlock()
			n(buildFrame(pkt[2], pkt[4:]))
		}
		return nil
	}
	return s.eqSession.WriteCmd(pkt)
}

func (s *hearSession) sent() [][]byte {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return append([][]byte(nil), s.pkts...)
}

type hearDialer struct{ s *hearSession }

func (f hearDialer) Dial(onNotify func([]byte)) (session, string, error) {
	f.s.eqSession.mu.Lock()
	f.s.eqSession.notify = onNotify
	f.s.eqSession.mu.Unlock()
	return f.s, "QCY MeloBuds N70", nil
}

func hearTestDaemon(t *testing.T, ack bool) (*daemon, *hearSession) {
	t.Helper()
	oa, or := hearingAckTimeout, hearingReadTimeout
	hearingAckTimeout, hearingReadTimeout = 150*time.Millisecond, time.Second
	t.Cleanup(func() { hearingAckTimeout, hearingReadTimeout = oa, or })
	hs := &hearSession{eqSession: &eqSession{state: mustHexBytes(t, realCustom11)}, ack: ack}
	d, _, _ := testDaemon(&fakeDialer{}, time.Hour)
	d.dialer = hearDialer{hs}
	mustStatus(t, d)
	settle(t, d)
	return d, hs
}

func gains8(v ...float64) []interface{} {
	out := make([]interface{}, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

func TestHearingModeSendsFlagFrameAndReadsBack(t *testing.T) {
	d, hs := hearTestDaemon(t, true)
	v, err := d.handleRequest(request{Cmd: "hearingMode", Args: map[string]interface{}{"on": true, "confirm": true}})
	if err != nil {
		t.Fatal(err)
	}
	r := v.(*hearingResult)
	if r.Sent != "ff03450101" || !r.Ack || r.Readback == nil || r.Readback.Slot != 11 {
		t.Fatalf("%+v", r)
	}
	if p := hs.sent(); len(p) != 1 || hex.EncodeToString(p[0]) != "ff03450101" {
		t.Fatalf("wire %x", p)
	}
	// Without an echo the command still succeeds and says so.
	d2, hs2 := hearTestDaemon(t, false)
	v, err = d2.handleRequest(request{Cmd: "hearingMode", Args: map[string]interface{}{"on": false, "confirm": true}})
	if err != nil || v.(*hearingResult).Ack || v.(*hearingResult).Sent != "ff03450100" || len(hs2.sent()) != 1 {
		t.Fatalf("%v %+v", err, v)
	}
	if _, err := d.handleRequest(request{Cmd: "hearingMode", Args: map[string]interface{}{"confirm": true}}); err == nil {
		t.Fatal("on is required")
	}
	n := len(hs.sent())
	for _, a := range []map[string]interface{}{{"on": true}, {"on": true, "confirm": false}, {"on": true, "confirm": "true"}} {
		if _, err := d.handleRequest(request{Cmd: "hearingMode", Args: a}); err == nil {
			t.Fatalf("%v accepted without confirm", a)
		}
	}
	if len(hs.sent()) != n {
		t.Fatal("a frame was sent without confirm")
	}
}

func TestHearingWriteRefusedWithoutConfirmAndOnBadInput(t *testing.T) {
	d, hs := hearTestDaemon(t, false)
	good := func() map[string]interface{} {
		return map[string]interface{}{"ear": "left", "gains": gains8(0, 0, 0, 0, 0, 0, 0, 0), "confirm": true}
	}
	mut := func(f func(m map[string]interface{})) map[string]interface{} { m := good(); f(m); return m }
	cases := map[string]map[string]interface{}{
		"no confirm":      mut(func(m map[string]interface{}) { delete(m, "confirm") }),
		"confirm false":   mut(func(m map[string]interface{}) { m["confirm"] = false }),
		"confirm string":  mut(func(m map[string]interface{}) { m["confirm"] = "true" }),
		"bad ear":         mut(func(m map[string]interface{}) { m["ear"] = "both" }),
		"7 gains":         mut(func(m map[string]interface{}) { m["gains"] = gains8(0, 0, 0, 0, 0, 0, 0) }),
		"too high":        mut(func(m map[string]interface{}) { m["gains"] = gains8(0, 0, 0, 0, 0, 0, 0, 7) }),
		"too low":         mut(func(m map[string]interface{}) { m["gains"] = gains8(-9, 0, 0, 0, 0, 0, 0, 0) }),
		"fraction":        mut(func(m map[string]interface{}) { m["gains"] = gains8(0.5, 0, 0, 0, 0, 0, 0, 0) }),
		"not numbers":     mut(func(m map[string]interface{}) { m["gains"] = []interface{}{"a", 0, 0, 0, 0, 0, 0, 0} }),
		"bad otherGains":  mut(func(m map[string]interface{}) { m["otherGains"] = gains8(1) }),
		"gains not array": mut(func(m map[string]interface{}) { m["gains"] = "0,0,0" }),
	}
	for name, args := range cases {
		if _, err := d.handleRequest(request{Cmd: "hearingWrite", Args: args}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := len(hs.sent()); n != 0 {
		t.Fatalf("%d frames reached the wire", n)
	}
}

func TestHearingWriteBuildsFrameWithMasterGain(t *testing.T) {
	d, hs := hearTestDaemon(t, true)
	gains := [hearing.Bands]float64{3, 4, 2, 1, 0, -2, 0, 5}
	other := [hearing.Bands]float64{6, 6, 6, 6, 6, 6, 6, 6}
	mg := hearing.MasterGain(gains)
	if o := hearing.MasterGain(other); o < mg {
		mg = o
	}
	mg, _ = hearing.ClampMaster(mg)
	want := hex.EncodeToString(hearing.WriteFrame(hearing.Right, gains, mg))
	v, err := d.handleRequest(request{Cmd: "hearingWrite", Args: map[string]interface{}{
		"ear": "right", "gains": gains8(3, 4, 2, 1, 0, -2, 0, 5), "otherGains": gains8(6, 6, 6, 6, 6, 6, 6, 6), "confirm": true}})
	if err != nil {
		t.Fatal(err)
	}
	r := v.(*hearingResult)
	if r.Sent != want || !strings.HasPrefix(r.Sent, "ff") || r.MasterGain == nil || *r.MasterGain != mg || !r.Ack || r.Readback == nil {
		t.Fatalf("%+v want %s", r, want)
	}
	if p := hs.sent(); len(p) != 1 || hex.EncodeToString(p[0]) != want || p[0][2] != 0x47 {
		t.Fatalf("wire %x", p)
	}
}

func TestHearingWriteClampsMasterGain(t *testing.T) {
	d, _ := hearTestDaemon(t, false)
	v, err := d.handleRequest(request{Cmd: "hearingWrite", Args: map[string]interface{}{
		"ear": "left", "gains": gains8(6, 6, 6, 6, 6, 6, 6, 6), "confirm": true}})
	if err != nil {
		t.Fatal(err)
	}
	r := v.(*hearingResult)
	if !r.MasterClamped || r.MasterGain == nil || *r.MasterGain != -12 {
		t.Fatalf("%+v", r)
	}
}

func TestHearingWriteBaselineZeroSends13Bands(t *testing.T) {
	d, hs := hearTestDaemon(t, true)
	gains := [hearing.Bands]float64{3, 4, 2, 1, 0, -2, 0, 5}
	mg := hearing.MasterGain(gains)
	mg, _ = hearing.ClampMaster(mg)
	want := hex.EncodeToString(hearing.WriteFrameBands(hearing.Left, hearing.ZeroBaselineBands(gains), mg))
	v, err := d.handleRequest(request{Cmd: "hearingWrite", Args: map[string]interface{}{
		"ear": "left", "gains": gains8(3, 4, 2, 1, 0, -2, 0, 5), "baseline": "zero", "confirm": true}})
	if err != nil {
		t.Fatal(err)
	}
	if r := v.(*hearingResult); r.Sent != want {
		t.Fatalf("sent %s want %s", r.Sent, want)
	}
	if p := hs.sent(); len(p) != 1 || p[0][3] != 0x5e {
		t.Fatalf("wire %x", p)
	}
	for _, bad := range []interface{}{"one", "ZERO", 1.0} {
		if _, err := d.handleRequest(request{Cmd: "hearingWrite", Args: map[string]interface{}{
			"ear": "left", "gains": gains8(0, 0, 0, 0, 0, 0, 0, 0), "baseline": bad, "confirm": true}}); err == nil {
			t.Fatalf("baseline %v accepted", bad)
		}
	}
	if n := len(hs.sent()); n != 1 {
		t.Fatalf("%d frames", n)
	}
}
