package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
)

// Real 0x22 payloads read from the device (same bytes as qcycore's fixtures).
const (
	realCustom11  = "0b50fb320064003c0002c80076fd3c000284039cff460002f00ac800780002401f44fd5e01023f002c014600027d000000460002fa000000460002f4010000460002e8036400460002d0076400460002a00f6400460002401f900146000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	realClassico6 = "0650fb3c002c01320002c8000cfe46000284039cff460002f00ac800780002401f44fd5e0102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"
)

func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	p, err := hex.DecodeString(s)
	if err != nil || len(p) != qcycore.EqReadLen {
		t.Fatalf("fixture: %v len %d", err, len(p))
	}
	return p
}

// eqDialer hands out one eqSession standing in for the earbuds' 0x22 state.
type eqDialer struct {
	mu   sync.Mutex
	sess *eqSession
}

func (f *eqDialer) Dial(onNotify func([]byte)) (session, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sess.notify = onNotify
	return f.sess, "QCY MeloBuds N70", nil
}

// eqSession answers a 0x22 query with state, and a 0x22 write according to
// onWrite (nil = ignore the write, as a device that keeps its active slot).
type eqSession struct {
	mu      sync.Mutex
	notify  func([]byte)
	state   []byte
	writes  [][]byte
	onWrite func(s *eqSession, written []byte)
	closed  bool
}

func (s *eqSession) WriteCmd(pkt []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(pkt) < 5 {
		return nil
	}
	switch op := pkt[2]; {
	case op == 0xFE:
		q := pkt[4]
		if q == qcycore.EqOpcode {
			s.notify(buildFrame(q, s.state))
		} else {
			s.notify(buildFrame(q, []byte{1, 2, 3}))
		}
	case op == qcycore.EqOpcode:
		w := append([]byte(nil), pkt[4:]...)
		s.writes = append(s.writes, w)
		if s.onWrite != nil {
			s.onWrite(s, w)
		}
	}
	return nil
}

func (s *eqSession) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *eqSession) Has(k charKind) bool { return k == charBattery || k == charVersion }
func (s *eqSession) Read(k charKind, buf []byte) (int, error) {
	switch k {
	case charBattery:
		return copy(buf, []byte{80, 70, 60}), nil
	case charVersion:
		return copy(buf, []byte{1, 2, 3}), nil
	}
	return 0, errors.New("absent")
}
func (s *eqSession) WriteKeys([]byte) error { return nil }
func (s *eqSession) Close()                 {}

// applyWrite makes the device adopt the written bytes over its state; bytes
// beyond what was written are zeroed (tail: false) or kept (tail: true).
func applyWrite(keepTail bool) func(*eqSession, []byte) {
	return func(s *eqSession, w []byte) {
		next := make([]byte, len(s.state))
		if keepTail {
			copy(next, s.state)
		}
		copy(next, w)
		s.state = next
		s.notify(buildFrame(qcycore.EqOpcode, s.state))
	}
}

// eqTestDaemon returns a connected daemon over a fake device holding state,
// with backups redirected to a temp dir and a short confirmation window.
func eqTestDaemon(t *testing.T, state []byte, onWrite func(*eqSession, []byte)) (*daemon, *eqSession, string) {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	old := eqConfirmTimeout
	eqConfirmTimeout = 700 * time.Millisecond // one 400 ms re-query fits
	t.Cleanup(func() { eqConfirmTimeout = old })
	sess := &eqSession{state: append([]byte(nil), state...), onWrite: onWrite}
	d, _, _ := testDaemon(&fakeDialer{}, time.Hour)
	d.dialer = &eqDialer{sess: sess}
	mustStatus(t, d)
	settle(t, d)
	return d, sess, filepath.Join(data, "qcy-omarchy", "backups")
}

func bandArgs(idx, freq int, gain float64, q, typ int) map[string]interface{} {
	return map[string]interface{}{"index": float64(idx), "freq": float64(freq), "gain": gain, "q": float64(q), "type": float64(typ)}
}

func backupCount(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

func eqApplyReq(d *daemon, args map[string]interface{}) (interface{}, error) {
	return d.handleRequest(request{Cmd: "eqApply", Args: args})
}

// With slot 11 (13 bands) active, writing three bands to slot 0 puts exactly
// those three on the wire: 73 bytes, zero tail, none of slot 11's bands.
func TestDaemonEqApplyBandsDoNotLeakActiveSlot(t *testing.T) {
	d, sess, _ := eqTestDaemon(t, mustHexBytes(t, realCustom11), applyWrite(false))
	res, err := eqApplyReq(d, map[string]interface{}{
		"slot":           float64(0),
		"masterGain":     float64(-300),
		"allowProtected": true,
		"bands":          []interface{}{bandArgs(0, 100, 1.5, 100, 2), bandArgs(4, 1000, -2, 70, 0), bandArgs(9, 8000, 3.25, 120, 2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.writeCount() != 1 {
		t.Fatalf("writes = %d", sess.writeCount())
	}
	w := sess.writes[0]
	if len(w) != qcycore.EqLen || w[0] != 0 {
		t.Fatalf("wrote %d bytes, slot %d", len(w), w[0])
	}
	snap, _ := qcycore.DecodeEq(w)
	if len(snap.Bands) != 3 || snap.MasterGain != -3 || snap.Bands[1].Freq != 1000 || snap.Bands[2].Gain != 3.25 {
		t.Fatalf("wrote %+v", snap)
	}
	if r := res.(*qcycore.EqResult); r.Slot != 0 || r.FullLength {
		t.Fatalf("result %+v", r)
	}
}

func TestDaemonEqApplyRefusesProtectedSlotsBeforeWriting(t *testing.T) {
	plain := mustHexBytes(t, realClassico6)
	plain[0] = 8 // unprotected active slot, blank tail
	cases := []struct {
		name  string
		state []byte
		args  map[string]interface{}
	}{
		{"write into 11", plain, map[string]interface{}{"slot": float64(11)}},
		{"write into 6", plain, map[string]interface{}{"slot": float64(6)}},
		{"leave protected 6", mustHexBytes(t, realClassico6), map[string]interface{}{"slot": float64(0)}},
		{"leave protected 11", mustHexBytes(t, realCustom11), map[string]interface{}{"slot": float64(0), "allowFull": true}},
		{"bands over protected 6", mustHexBytes(t, realClassico6), map[string]interface{}{"bands": []interface{}{bandArgs(0, 100, 0, 100, 2)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, sess, bdir := eqTestDaemon(t, c.state, applyWrite(false))
			_, err := eqApplyReq(d, c.args)
			if !errors.Is(err, qcycore.ErrProtectedSlot) || responseCode(err) != "protected_slot" {
				t.Fatalf("err = %v (code %q)", err, responseCode(err))
			}
			if sess.writeCount() != 0 || backupCount(t, bdir) != 0 {
				t.Fatalf("refused request still wrote %d frames / %d backups", sess.writeCount(), backupCount(t, bdir))
			}
		})
	}
	// the override is honoured
	d, sess, _ := eqTestDaemon(t, plain, applyWrite(false))
	if _, err := eqApplyReq(d, map[string]interface{}{"slot": float64(11), "allowProtected": true}); err != nil {
		t.Fatal(err)
	}
	if sess.writeCount() != 1 {
		t.Fatal("override did not write")
	}
}

func TestDaemonEqApplyFullLengthNeedsFlag(t *testing.T) {
	d, sess, bdir := eqTestDaemon(t, mustHexBytes(t, realCustom11), applyWrite(true))
	_, err := eqApplyReq(d, map[string]interface{}{"masterGain": float64(-500)})
	if responseCode(err) != "full_length_required" || !strings.Contains(err.Error(), "--full-length") {
		t.Fatalf("err = %v (code %q)", err, responseCode(err))
	}
	if sess.writeCount() != 0 || backupCount(t, bdir) != 0 {
		t.Fatal("refused request wrote")
	}
	res, err := eqApplyReq(d, map[string]interface{}{"masterGain": float64(-500), "allowFull": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.writes[0]) != qcycore.EqReadLen || !res.(*qcycore.EqResult).FullLength {
		t.Fatalf("wrote %d bytes", len(sess.writes[0]))
	}
	// the eqMasterGain convenience forwards the flag, and is refused without it
	d2, _, _ := eqTestDaemon(t, mustHexBytes(t, realCustom11), applyWrite(true))
	if _, err := d2.handleRequest(request{Cmd: "eqMasterGain", Args: map[string]interface{}{"masterGain": float64(-100)}}); responseCode(err) != "full_length_required" {
		t.Fatalf("eqMasterGain without flag: %v", err)
	}
	if _, err := d2.handleRequest(request{Cmd: "eqMasterGain", Args: map[string]interface{}{"masterGain": float64(-100), "allowFull": true}}); err != nil {
		t.Fatalf("eqMasterGain with flag: %v", err)
	}
}

// The master-gain slider of the protected Classico slot (blank tail) keeps
// working: it is a 73-byte write.
func TestDaemonEqApplyMasterOnProtectedActive73(t *testing.T) {
	d, sess, _ := eqTestDaemon(t, mustHexBytes(t, realClassico6), applyWrite(true))
	if _, err := eqApplyReq(d, map[string]interface{}{"masterGain": float64(-200)}); err != nil {
		t.Fatal(err)
	}
	if len(sess.writes[0]) != qcycore.EqLen {
		t.Fatalf("wrote %d bytes", len(sess.writes[0]))
	}
}

// Cross-slot write the device never reports back: written, unconfirmed.
func TestDaemonEqApplyUnconfirmedWhenDeviceKeepsOtherSlot(t *testing.T) {
	d, sess, bdir := eqTestDaemon(t, mustHexBytes(t, realCustom11), nil)
	_, err := eqApplyReq(d, map[string]interface{}{
		"slot": float64(0), "masterGain": float64(0), "allowProtected": true,
		"bands": []interface{}{bandArgs(0, 100, 1, 100, 2)},
	})
	var ee *qcycore.EqError
	if !errors.Is(err, qcycore.ErrUnconfirmed) || !errors.As(err, &ee) || responseCode(err) != "unconfirmed" {
		t.Fatalf("err = %v (code %q)", err, responseCode(err))
	}
	if sess.writeCount() != 1 {
		t.Fatalf("writes = %d", sess.writeCount())
	}
	if !bytes.Equal(ee.Sent, sess.writes[0]) || ee.Got[0] != 11 {
		t.Fatalf("error carries sent=%x got=%x", ee.Sent, ee.Got)
	}
	if !strings.Contains(err.Error(), hex.EncodeToString(sess.writes[0])) || !strings.Contains(err.Error(), ee.Backup) {
		t.Fatalf("message lacks what was written / the backup path: %v", err)
	}
	if b, rerr := os.ReadFile(ee.Backup); rerr != nil || !strings.HasPrefix(string(b), "0b50fb32") {
		t.Fatalf("backup %s: %v", ee.Backup, rerr)
	}
	if backupCount(t, bdir) == 0 {
		t.Fatal("no backup written")
	}
}

// A 73-byte write the device answers with a populated tail is not confirmed,
// and is a plain mismatch (the slot is the one that was sent).
func TestDaemonEqApplyDirtyTailEchoIsMismatch(t *testing.T) {
	plain := mustHexBytes(t, realClassico6)
	plain[0] = 8
	dirty := func(s *eqSession, w []byte) {
		next := make([]byte, qcycore.EqReadLen)
		copy(next, w)
		next[3+10*7], next[3+10*7+1] = 0xe8, 0x03 // a band at position 10
		s.state = next
		s.notify(buildFrame(qcycore.EqOpcode, next))
	}
	d, sess, _ := eqTestDaemon(t, plain, dirty)
	_, err := eqApplyReq(d, map[string]interface{}{"masterGain": float64(-100)})
	if err == nil || errors.Is(err, qcycore.ErrUnconfirmed) || responseCode(err) != "" || !strings.Contains(err.Error(), "readback mismatch") {
		t.Fatalf("err = %v (code %q)", err, responseCode(err))
	}
	if sess.writeCount() != 1 || len(sess.writes[0]) != qcycore.EqLen {
		t.Fatalf("writes = %d", sess.writeCount())
	}
}

// Raw payload bytes never travel over the socket.
func TestDaemonEqApplyRejectsRawPayload(t *testing.T) {
	d, sess, _ := eqTestDaemon(t, mustHexBytes(t, realClassico6), applyWrite(false))
	for _, args := range []map[string]interface{}{
		{"payload": hex.EncodeToString(mustHexBytes(t, realCustom11))},
		{"raw": []interface{}{1.0, 2.0}},
		{"params": []interface{}{1.0}},
		{"nope": 1.0},
	} {
		if _, err := eqApplyReq(d, args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// the generic raw path stays closed too
	if _, err := d.handleRequest(request{Cmd: "set", Args: map[string]interface{}{"action": "eq", "params": []interface{}{1.0}}}); err == nil {
		t.Error("raw eq set accepted")
	}
	if sess.writeCount() != 0 {
		t.Fatalf("raw request wrote %d frames", sess.writeCount())
	}
}
