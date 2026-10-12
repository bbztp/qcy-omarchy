package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
)

// 0x22 payloads shaped like the device's (slot 11 is synthetic; same bytes as qcycore's fixtures).
const (
	realCustom11  = "0b50fb28006400370002c800d4fe410002840332004b0002f00afa006e0002401f38ff2c01023f0096005000027d00ceff500002fa006400500002f4016aff500002e8033200500002d007c800500002a00f9cff500002401f2c015a000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
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

// Slot 255 (the app's "Personalizado") takes the 143-byte payload through the
// socket with no allowFull, the empty positions carry the 0xFF type marker, the
// echo that the device gives (marker included) confirms it, and the same bands
// to another slot still need allowFull. Band types 3 and 255 are accepted.
func TestDaemonEqApplySlot255ImpliesFullLength(t *testing.T) {
	bands := []interface{}{
		bandArgs(0, 125, 3, 100, 255), bandArgs(1, 200, -9, 70, 2), bandArgs(2, 7000, -1, 120, 3),
		bandArgs(12, 8000, 4, 70, 2),
	}
	d, sess, bdir := eqTestDaemon(t, mustHexBytes(t, realCustom11), applyWrite(false))
	res, err := eqApplyReq(d, map[string]interface{}{"slot": float64(255), "masterGain": float64(-1200), "bands": bands})
	if err != nil {
		t.Fatalf("slot 255 without allowFull: %v", err)
	}
	if sess.writeCount() != 1 || backupCount(t, bdir) == 0 {
		t.Fatalf("writes %d backups %d", sess.writeCount(), backupCount(t, bdir))
	}
	w := sess.writes[0]
	if len(w) != qcycore.EqReadLen || w[0] != 0xff || w[3+6] != 255 || w[3+2*7+6] != 3 {
		t.Fatalf("wrote %d bytes: %x", len(w), w)
	}
	if got := hex.EncodeToString(w[3+19*7:]); got != "000000000000ff" {
		t.Fatalf("empty position 19 = %s", got)
	}
	if r := res.(*qcycore.EqResult); r.Slot != 255 || !r.FullLength || len(r.Bands) != 4 {
		t.Fatalf("result %+v", r)
	}
	// any other target keeps the requirement
	d2, sess2, _ := eqTestDaemon(t, mustHexBytes(t, realCustom11), applyWrite(false))
	_, err = eqApplyReq(d2, map[string]interface{}{"slot": float64(0), "masterGain": float64(-1200), "allowProtected": true, "bands": []interface{}{
		bandArgs(0, 125, 3, 100, 2), bandArgs(1, 200, -9, 70, 2), bandArgs(2, 7000, -1, 120, 3), bandArgs(12, 8000, 4, 70, 2)}})
	if responseCode(err) != "full_length_required" || sess2.writeCount() != 0 {
		t.Fatalf("slot 0 without allowFull: %v (code %q), writes %d", err, responseCode(err), sess2.writeCount())
	}
	// type 255 (the empty marker elsewhere) is refused for any slot but 255
	_, err = eqApplyReq(d2, map[string]interface{}{"slot": float64(0), "masterGain": float64(-1200), "allowProtected": true, "allowFull": true, "bands": bands})
	if err == nil || !strings.Contains(err.Error(), "type 255") || sess2.writeCount() != 0 {
		t.Fatalf("type 255 to slot 0: %v, writes %d", err, sess2.writeCount())
	}
	// type 7 is still refused
	_, err = eqApplyReq(d2, map[string]interface{}{"slot": float64(255), "masterGain": float64(0), "bands": []interface{}{bandArgs(0, 100, 0, 100, 7)}})
	if err == nil || sess2.writeCount() != 0 {
		t.Fatalf("type 7: %v", err)
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
	if b, rerr := os.ReadFile(ee.Backup); rerr != nil || !strings.HasPrefix(string(b), "0b50fb28") {
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

// The wire response of a failed eqApply carries code and, for unconfirmed,
// sent/deviceReported/backup (what qcyctl rebuilds the typed error from).
func TestHandleConnEqFailureResponseFields(t *testing.T) {
	d, sess, _ := eqTestDaemon(t, mustHexBytes(t, realCustom11), nil)
	ask := func(args map[string]interface{}) map[string]interface{} {
		c, s := net.Pipe()
		go handleConn(s, d)
		defer c.Close()
		req, _ := json.Marshal(map[string]interface{}{"cmd": "eqApply", "args": args})
		if _, err := c.Write(append(req, '\n')); err != nil {
			t.Fatal(err)
		}
		var resp map[string]interface{}
		if err := json.NewDecoder(c).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := ask(map[string]interface{}{"slot": float64(0), "masterGain": float64(0)})
	if resp["ok"] != false || resp["code"] != "protected_slot" || resp["sent"] != nil {
		t.Fatalf("protected: %v", resp)
	}
	resp = ask(map[string]interface{}{
		"slot": float64(0), "masterGain": float64(0), "allowProtected": true,
		"bands": []interface{}{bandArgs(0, 100, 1, 100, 2)},
	})
	if resp["code"] != "unconfirmed" || sess.writeCount() != 1 {
		t.Fatalf("unconfirmed: %v", resp)
	}
	if resp["sent"] != hex.EncodeToString(sess.writes[0]) || resp["deviceReported"] == "" || resp["deviceReported"] == nil {
		t.Fatalf("sent/deviceReported: %v", resp)
	}
	if b, _ := resp["backup"].(string); b == "" {
		t.Fatalf("backup missing: %v", resp)
	}
}
