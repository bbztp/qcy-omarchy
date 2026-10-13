package main

import (
	"errors"
	"strings"
	"testing"
)

// fakeToggleAPI records what the toggle commands send; the embedded nil
// interface panics on anything else.
type fakeToggleAPI struct {
	daemonAPI
	setCalls    []string
	expectCalls []string
	wearStates  []byte
	wearDiag    []byte
	ldacErr     error
	windBytes   []byte
	wearReply   []byte
	windReply   []byte
	ldacReply   []byte
}

func (f *fakeToggleAPI) setAction(a string, p []byte) error {
	f.setCalls = append(f.setCalls, a)
	return nil
}
func (f *fakeToggleAPI) setActionExpect(a string, p, e []byte) ([]byte, error) {
	f.expectCalls = append(f.expectCalls, a+":"+string(rune('0'+p[0])))
	if a == "ldac" {
		if f.ldacErr != nil {
			return nil, f.ldacErr
		}
		return f.ldacReply, nil
	}
	return e, nil
}
func (f *fakeToggleAPI) wearSet(s byte) ([]byte, error) {
	f.wearStates = append(f.wearStates, s)
	return f.wearReply, nil
}
func (f *fakeToggleAPI) wearOffByte(b byte) ([]byte, error) {
	f.wearDiag = append(f.wearDiag, b)
	return f.wearReply, nil
}
func (f *fakeToggleAPI) windOffByte(b byte) ([]byte, error) {
	f.windBytes = append(f.windBytes, b)
	return f.windReply, nil
}

func useFakeToggle(t *testing.T, f *fakeToggleAPI) *int {
	t.Helper()
	opened := 0
	old := cmdRunner
	cmdRunner = func(_ int, fn func(daemonAPI) error) error {
		opened++
		return fn(f)
	}
	t.Cleanup(func() { cmdRunner = old })
	return &opened
}

func TestAutoOffRefusesZeroAndNeverSendsIt(t *testing.T) {
	f := &fakeToggleAPI{}
	opened := useFakeToggle(t, f)
	for _, v := range []string{"0", "00", "-1", "65535", "abc", "0x0"} {
		err := cmdAutoOff([]string{v})
		if err == nil {
			t.Errorf("auto-off %q accepted", v)
		}
	}
	if err := cmdAutoOff([]string{"0"}); err == nil || !strings.Contains(err.Error(), "0 is refused") {
		t.Errorf("message: %v", err)
	}
	if *opened != 0 || len(f.expectCalls) != 0 {
		t.Fatalf("link opened %d times, calls %v", *opened, f.expectCalls)
	}
	for _, v := range []string{"never", "5", "15", "90"} {
		if err := cmdAutoOff([]string{v}); err != nil {
			t.Errorf("auto-off %q: %v", v, err)
		}
	}
}

func TestLdacRequiresYes(t *testing.T) {
	f := &fakeToggleAPI{ldacReply: []byte{1}}
	opened := useFakeToggle(t, f)
	if err := cmdToggle("ldac", []string{"on"}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}
	if *opened != 0 {
		t.Fatal("link opened without --yes")
	}
	if err := cmdToggle("ldac", []string{"on", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if len(f.expectCalls) != 1 || f.expectCalls[0] != "ldac:1" {
		t.Fatalf("calls %q", f.expectCalls)
	}
}

func TestLdacRestartIsNotAFailure(t *testing.T) {
	f := &fakeToggleAPI{ldacReply: nil} // accepted, then no readback
	useFakeToggle(t, f)
	if err := cmdToggle("ldac", []string{"off", "--yes"}); err != nil {
		t.Fatalf("restart reported as failure: %v", err)
	}
	if len(f.expectCalls) != 1 {
		t.Fatalf("write repeated: %v", f.expectCalls)
	}
}

func TestLdacDirectModeRefuses(t *testing.T) {
	if _, err := opcodeFor("ldac"); err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("err = %v", err)
	}
}

func TestOffByteNeedsYesAndValidValue(t *testing.T) {
	f := &fakeToggleAPI{wearReply: []byte{2, 5, 6}, windReply: []byte{2}}
	opened := useFakeToggle(t, f)
	cases := [][]string{
		{"off", "--off-byte", "0"},          // no --yes
		{"off", "--off-byte", "1", "--yes"}, // bad value
		{"on", "--off-byte", "0", "--yes"},  // not off
	}
	for _, c := range cases {
		if err := cmdWear(c); err == nil {
			t.Errorf("wear %v accepted", c)
		}
		if err := cmdToggle("wind", c); err == nil {
			t.Errorf("wind %v accepted", c)
		}
	}
	if *opened != 0 {
		t.Fatalf("link opened %d times for refused input", *opened)
	}
	// ldac and others take no --off-byte.
	if err := cmdToggle("game", []string{"off", "--off-byte", "0", "--yes"}); err == nil {
		t.Error("game --off-byte accepted")
	}
}

func TestOffByteDiagnosticWritesRequestedByte(t *testing.T) {
	f := &fakeToggleAPI{wearReply: []byte{2, 5, 6}, windReply: []byte{2}}
	useFakeToggle(t, f)
	if err := cmdWear([]string{"off", "--off-byte", "0", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdWear([]string{"off", "--off-byte=2", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if string(f.wearDiag) != "\x00\x02" || len(f.wearStates) != 0 {
		t.Fatalf("wear diag %v, plain %v", f.wearDiag, f.wearStates)
	}
	if err := cmdToggle("wind", []string{"off", "--off-byte", "0", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if string(f.windBytes) != "\x00" || len(f.setCalls) != 0 {
		t.Fatalf("wind bytes %v, plain sets %v", f.windBytes, f.setCalls)
	}
}

// The CLI asks for "off" (2); the daemon turns it into 0x00 on a verified
// model and keeps 0x02 elsewhere.
func TestPlainWearOffAsksDaemonForOff(t *testing.T) {
	f := &fakeToggleAPI{wearReply: []byte{2, 5, 6}}
	useFakeToggle(t, f)
	if err := cmdWear([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if string(f.wearStates) != "\x02" {
		t.Fatalf("states %v", f.wearStates)
	}
}

func TestWearByteMatches(t *testing.T) {
	if !wearByteMatches(0, 2) || !wearByteMatches(0, 0) || !wearByteMatches(2, 0) || wearByteMatches(1, 2) || wearByteMatches(0, 1) {
		t.Fatal("wearByteMatches")
	}
	_ = errors.New
}

func TestLdacIndeterminateIsNotAFailureAndNotRepeated(t *testing.T) {
	f := &fakeToggleAPI{ldacErr: &indeterminateError{msg: "indeterminado: aplicado possivelmente, reinício pendente — releia o estado"}}
	useFakeToggle(t, f)
	if err := cmdToggle("ldac", []string{"on", "--yes"}); err != nil {
		t.Fatalf("indeterminate reported as failure: %v", err)
	}
	if len(f.expectCalls) != 1 {
		t.Fatalf("write repeated: %v", f.expectCalls)
	}
}

func TestOffByteZeroDirectModeRefuses(t *testing.T) {
	w := &daemonClientWrapper{}
	if _, err := w.wearOffByte(0); err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("wear: %v", err)
	}
	if _, err := w.windOffByte(0); err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("wind: %v", err)
	}
}
