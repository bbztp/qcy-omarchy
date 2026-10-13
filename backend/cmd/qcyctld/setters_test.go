package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptSession is a link whose replies the test scripts per opcode. A query
// answers with state[op] (or [1 2 3]); a write calls onWrite.
type scriptSession struct {
	mu      sync.Mutex
	notify  func([]byte)
	state   map[byte][]byte
	packets [][]byte
	onWrite func(s *scriptSession, op byte, params []byte)
}

func (s *scriptSession) WriteCmd(pkt []byte) error {
	s.mu.Lock()
	s.packets = append(s.packets, append([]byte(nil), pkt...))
	notify, onWrite := s.notify, s.onWrite
	var reply []byte
	var rop byte
	if len(pkt) >= 5 && pkt[2] == 0xFE {
		rop = pkt[4]
		reply = s.state[rop]
		if reply == nil {
			reply = []byte{1, 2, 3}
		}
	}
	s.mu.Unlock()
	if reply != nil {
		notify(buildFrame(rop, reply))
	} else if len(pkt) >= 4 && onWrite != nil {
		onWrite(s, pkt[2], pkt[4:])
	}
	return nil
}

func (s *scriptSession) writesOf(op byte) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	for _, p := range s.packets {
		if len(p) >= 3 && p[2] == op {
			out = append(out, p)
		}
	}
	return out
}

func (s *scriptSession) set(op byte, v []byte) {
	s.mu.Lock()
	s.state[op] = v
	s.mu.Unlock()
}

func (s *scriptSession) Has(k charKind) bool { return k == charBattery || k == charVersion }
func (s *scriptSession) Read(k charKind, buf []byte) (int, error) {
	switch k {
	case charBattery:
		return copy(buf, []byte{80, 70, 60}), nil
	case charVersion:
		return copy(buf, []byte{1, 2, 3}), nil
	}
	return 0, errAbsent
}
func (s *scriptSession) WriteKeys([]byte) error { return nil }
func (s *scriptSession) Close()                 {}

var errAbsent = bytes.ErrTooLarge

type scriptDialer struct{ s *scriptSession }

func (f scriptDialer) Dial(onNotify func([]byte)) (session, string, error) {
	f.s.mu.Lock()
	f.s.notify = onNotify
	f.s.mu.Unlock()
	return f.s, "QCY MeloBuds N70", nil
}

func scriptDaemon(t *testing.T, onWrite func(*scriptSession, byte, []byte)) (*daemon, *scriptSession) {
	t.Helper()
	s := &scriptSession{state: map[byte][]byte{}, onWrite: onWrite}
	cfg := defaultLifecycle()
	cfg.idle = time.Hour
	d := newDaemon(scriptDialer{s}, cfg)
	if _, err := d.handleRequest(request{Cmd: "capabilities"}); err != nil {
		t.Fatal(err)
	}
	mustStatus(t, d)
	settle(t, d)
	return d, s
}

func setReq(action string, params ...byte) request {
	list := make([]interface{}, len(params))
	for i, b := range params {
		list[i] = float64(b)
	}
	return request{Cmd: "set", Args: map[string]interface{}{"action": action, "params": list}}
}

func TestPromptVolWritesTwoByteFrameAndConfirms(t *testing.T) {
	d, s := scriptDaemon(t, func(s *scriptSession, op byte, p []byte) {
		if op == 0x1D && len(p) >= 1 {
			s.set(0x1D, []byte{p[0], 10})
		}
	})
	got, err := d.handleRequest(setReq("promptVol", 8))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := got.([]int); len(b) < 1 || b[0] != 8 {
		t.Fatalf("confirmed payload %v", got)
	}
	w := s.writesOf(0x1D)
	var frame []byte
	for _, p := range w {
		if len(p) > 3 && p[2] == 0x1D {
			frame = p
		}
	}
	if !bytes.Equal(frame, []byte{0xFF, 0x04, 0x1D, 0x02, 0x08, 0x00}) {
		t.Fatalf("frame %x", frame)
	}
}

func TestPromptVolRefusesValuesOutsideTheListBeforeWriting(t *testing.T) {
	d, s := scriptDaemon(t, nil)
	before := len(s.writesOf(0x1D))
	for _, v := range []byte{0, 5, 7, 11, 15, 255} {
		if _, err := d.handleRequest(setReq("promptVol", v)); err == nil {
			t.Errorf("%d accepted", v)
		}
	}
	if _, err := d.handleRequest(setReq("promptVol", 6, 1)); err == nil {
		t.Error("second byte 1 accepted")
	}
	if n := len(s.writesOf(0x1D)); n != before {
		t.Fatalf("%d frames written for refused values", n-before)
	}
}

func TestPromptVolMismatchSaysWrittenButReported(t *testing.T) {
	d, _ := scriptDaemon(t, func(s *scriptSession, op byte, p []byte) {
		// The device ignores the write and keeps reporting 6.
		s.set(0x1D, []byte{6, 10})
	})
	_, err := d.handleRequest(setReq("promptVol", 10))
	if err == nil || !strings.Contains(err.Error(), "escrito mas o aparelho reportou 060a") {
		t.Fatalf("err = %v", err)
	}
}

func TestWearSensitivity(t *testing.T) {
	d, s := scriptDaemon(t, func(s *scriptSession, op byte, p []byte) {
		if op == 0x48 && len(p) >= 1 {
			s.set(0x48, []byte{p[0]})
		}
	})
	for _, v := range []byte{1, 2, 3} {
		if _, err := d.handleRequest(setReq("wearSensitivity", v)); err != nil {
			t.Fatalf("%d: %v", v, err)
		}
	}
	w := s.writesOf(0x48)
	var frames [][]byte
	for _, p := range w {
		if len(p) > 3 {
			frames = append(frames, p)
		}
	}
	if len(frames) != 3 || !bytes.Equal(frames[2], []byte{0xFF, 0x03, 0x48, 0x01, 0x03}) {
		t.Fatalf("frames %x", frames)
	}
	n := len(s.writesOf(0x48))
	for _, v := range []byte{0, 4} {
		if _, err := d.handleRequest(setReq("wearSensitivity", v)); err == nil {
			t.Errorf("%d accepted", v)
		}
	}
	if len(s.writesOf(0x48)) != n {
		t.Fatal("refused value was written")
	}
	// Status reports it once the probe answered.
	rep := mustStatus(t, d)
	if v, _ := rep["wearSensitivity"].(int); v != 3 {
		t.Fatalf("status wearSensitivity=%v", rep["wearSensitivity"])
	}
}

func TestFitTestReturnsResultAndStops(t *testing.T) {
	d, s := scriptDaemon(t, func(s *scriptSession, op byte, p []byte) {
		if op == 0x11 && bytes.Equal(p, []byte{1, 1, 1}) {
			s.mu.Lock()
			n := s.notify
			s.mu.Unlock()
			n(buildFrame(0x11, []byte{0, 3, 3})) // not final
			n(buildFrame(0x11, []byte{2, 3, 1})) // final
		}
	})
	v, err := d.handleRequest(request{Cmd: "fitTest", Args: map[string]interface{}{"seconds": float64(10)}})
	if err != nil {
		t.Fatal(err)
	}
	if r := v.(fitResult); r.Status != 2 || r.Left != 3 || r.Right != 1 {
		t.Fatalf("result %+v", r)
	}
	var frames [][]byte
	for _, p := range s.writesOf(0x11) {
		if len(p) > 3 {
			frames = append(frames, p)
		}
	}
	if len(frames) != 2 ||
		!bytes.Equal(frames[0], []byte{0xFF, 0x05, 0x11, 0x03, 1, 1, 1}) ||
		!bytes.Equal(frames[1], []byte{0xFF, 0x05, 0x11, 0x03, 0, 0, 0}) {
		t.Fatalf("frames %x", frames)
	}
}

func lastFitFrame(s *scriptSession) []byte {
	var last []byte
	for _, p := range s.writesOf(0x11) {
		if len(p) > 3 {
			last = p
		}
	}
	return last
}

func TestFitTestTimeoutSendsStop(t *testing.T) {
	d, s := scriptDaemon(t, nil)
	_, err := d.fitTest(200 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no final result") {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(lastFitFrame(s), []byte{0xFF, 0x05, 0x11, 0x03, 0, 0, 0}) {
		t.Fatalf("last frame %x", lastFitFrame(s))
	}
}

func TestFitStopCancelsARunningTest(t *testing.T) {
	d, s := scriptDaemon(t, nil)
	errCh := make(chan error, 1)
	go func() {
		_, err := d.fitTest(30 * time.Second)
		errCh <- err
	}()
	waitFor(t, "start frame", func() bool { return lastFitFrame(s) != nil })
	if _, err := d.handleRequest(request{Cmd: "fitStop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fit test did not stop")
	}
	if !bytes.Equal(lastFitFrame(s), []byte{0xFF, 0x05, 0x11, 0x03, 0, 0, 0}) {
		t.Fatalf("last frame %x", lastFitFrame(s))
	}
}

func TestN70DeclaresTheNewActionsProfileDoesNot(t *testing.T) {
	has := func(c Capabilities, id string) bool { return declaresAction(c, id) }
	n70 := GetHandler("melobuds-n70").Capabilities()
	prof := GetHandler("qcy-19786").Capabilities()
	for _, id := range []string{"wearSensitivity", "promptVolume"} {
		if !has(n70, id) {
			t.Errorf("n70 lacks %s", id)
		}
		if has(prof, id) {
			t.Errorf("profile declares %s", id)
		}
	}
}
