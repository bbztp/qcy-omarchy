package qcycore

import (
	"bytes"
	"math"
	"os"
	"testing"
	"time"
)

// fixture builds a 73-byte payload with deliberately non-standard frequencies,
// Q and types, like the vendor hearing-test slot.
func fixture(slot byte, masterCenti int16) []byte {
	p := make([]byte, EqLen)
	p[0] = slot
	p[1], p[2] = byte(masterCenti), byte(masterCenti>>8)
	freqs := []uint16{40, 90, 180, 320, 640, 1250, 2500, 5000, 9000, 14000}
	for i := 0; i < EqBands; i++ {
		o := 3 + i*7
		p[o], p[o+1] = byte(freqs[i]), byte(freqs[i]>>8)
		g := int16((i - 5) * 50)
		p[o+2], p[o+3] = byte(g), byte(g>>8)
		q := uint16(70 + i)
		p[o+4], p[o+5] = byte(q), byte(q>>8)
		p[o+6] = byte(i % 3)
	}
	return p
}

func TestDecodeEq(t *testing.T) {
	p := fixture(11, -150)
	s, err := DecodeEq(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Slot != 11 || s.MasterGain != -1.5 {
		t.Fatalf("slot/master = %d/%v", s.Slot, s.MasterGain)
	}
	if len(s.Bands) != 10 || s.Bands[0].Freq != 40 || s.Bands[9].Freq != 14000 {
		t.Fatalf("bands = %+v", s.Bands)
	}
	if s.Bands[0].Gain != -2.5 || s.Bands[5].Gain != 0 || s.Bands[9].Gain != 2 {
		t.Fatalf("gains = %+v", s.Bands)
	}
	if s.Bands[3].Q != 73 || s.Bands[4].Type != 1 {
		t.Fatalf("q/type = %+v", s.Bands)
	}
	if _, err := DecodeEq([]byte{1, 2}); err == nil {
		t.Fatal("short payload accepted")
	}
}

func TestApplyMasterGainTouchesOnlyBytes1And2(t *testing.T) {
	cur := fixture(11, 0)
	c := 250
	next, err := ApplyEq(cur, EqOp{MasterCenti: &c})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != EqLen || next[0] != 11 {
		t.Fatalf("slot/len changed: %x", next)
	}
	if next[1] != 0xfa || next[2] != 0x00 {
		t.Fatalf("master bytes = %02x %02x", next[1], next[2])
	}
	if !bytes.Equal(next[3:], cur[3:]) {
		t.Fatal("bands changed by a master-gain patch")
	}
	// the input must not be modified
	if !bytes.Equal(cur, fixture(11, 0)) {
		t.Fatal("ApplyEq mutated its input")
	}
	// negative values are two's complement
	n := -800
	next, _ = ApplyEq(cur, EqOp{MasterCenti: &n})
	if int16(uint16(next[1])|uint16(next[2])<<8) != -800 {
		t.Fatalf("negative master = %x", next[1:3])
	}
}

func TestApplyGainsPreservesFreqQType(t *testing.T) {
	cur := fixture(8, 100)
	g := [EqBands]int{800, -800, 1, -1, 0, 50, 60, 70, 80, 90}
	next, err := ApplyEq(cur, EqOp{Gains: &g})
	if err != nil {
		t.Fatal(err)
	}
	if next[0] != 8 || next[1] != cur[1] || next[2] != cur[2] {
		t.Fatalf("header changed: %x", next[:3])
	}
	s, _ := DecodeEq(next)
	o, _ := DecodeEq(cur)
	for i := range s.Bands {
		if s.Bands[i].Freq != o.Bands[i].Freq || s.Bands[i].Q != o.Bands[i].Q || s.Bands[i].Type != o.Bands[i].Type {
			t.Fatalf("band %d freq/q/type changed", i)
		}
		if int(math.Round(s.Bands[i].Gain*100)) != g[i] {
			t.Fatalf("band %d gain = %v want %d", i, s.Bands[i].Gain, g[i])
		}
	}
}

func TestApplySlot(t *testing.T) {
	cur := fixture(11, 0)
	n := 3
	next, err := ApplyEq(cur, EqOp{Slot: &n})
	if err != nil || next[0] != 3 || !bytes.Equal(next[1:], cur[1:]) {
		t.Fatalf("slot patch: %x %v", next, err)
	}
	bad := 256
	if _, err := ApplyEq(cur, EqOp{Slot: &bad}); err == nil {
		t.Fatal("slot 256 accepted")
	}
}

func TestApplyRejectsBadInput(t *testing.T) {
	cur := fixture(1, 0)
	if _, err := ApplyEq(cur[:72], EqOp{}); err == nil {
		t.Fatal("72-byte payload accepted")
	}
	if _, err := ApplyEq(append(cur, 0), EqOp{}); err == nil {
		t.Fatal("74-byte payload accepted")
	}
	hi, lo := 801, -801
	if _, err := ApplyEq(cur, EqOp{MasterCenti: &hi}); err == nil {
		t.Fatal("master 801 accepted")
	}
	if _, err := ApplyEq(cur, EqOp{MasterCenti: &lo}); err == nil {
		t.Fatal("master -801 accepted")
	}
	g := [EqBands]int{}
	g[4] = 900
	if _, err := ApplyEq(cur, EqOp{Gains: &g}); err == nil {
		t.Fatal("band gain 900 accepted")
	}
}

func TestCentiFromDB(t *testing.T) {
	ok := map[float64]int{0: 0, 8: 800, -8: -800, 1.5: 150, -3.456: -346, 0.004: 0}
	for in, want := range ok {
		got, err := CentiFromDB(in)
		if err != nil || got != want {
			t.Errorf("CentiFromDB(%v) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 8.01, -8.01, 1e30, -1e30} {
		if _, err := CentiFromDB(in); err == nil {
			t.Errorf("CentiFromDB(%v) accepted", in)
		}
	}
}

func TestParseGains(t *testing.T) {
	g, err := ParseGains("-3, 0,2.5,1,1,1,1,1,1,8")
	if err != nil || g[0] != -300 || g[2] != 250 || g[9] != 800 {
		t.Fatalf("%v %v", g, err)
	}
	for _, bad := range []string{"1,2,3", "1,1,1,1,1,1,1,1,1,9", "1,1,1,1,1,1,1,1,1,NaN", "1,1,1,1,1,1,1,1,1,Inf", "a,1,1,1,1,1,1,1,1,1"} {
		if _, err := ParseGains(bad); err == nil {
			t.Errorf("ParseGains(%q) accepted", bad)
		}
	}
}

func TestEqMatch(t *testing.T) {
	cur := fixture(11, 0)
	m := 100
	sent, _ := ApplyEq(cur, EqOp{MasterCenti: &m})
	masterOnly := EqMatch(sent, EqOp{MasterCenti: &m})
	if masterOnly(cur) {
		t.Fatal("pre-change echo confirmed a master-gain write")
	}
	if !masterOnly(sent) {
		t.Fatal("correct echo not confirmed")
	}
	// wrong slot with right master gain must not confirm
	other := append([]byte(nil), sent...)
	other[0] = 2
	if masterOnly(other) {
		t.Fatal("echo with the wrong slot confirmed")
	}
	// with gains, every band must match
	g := [EqBands]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	sent2, _ := ApplyEq(cur, EqOp{Gains: &g})
	full := EqMatch(sent2, EqOp{Gains: &g})
	if !full(sent2) || full(cur) {
		t.Fatal("full match wrong")
	}
	mism := append([]byte(nil), sent2...)
	mism[3+9*7+2]++
	if full(mism) {
		t.Fatal("last band mismatch confirmed")
	}
}

func TestValidateEqRejectsAllFF(t *testing.T) {
	p := bytes.Repeat([]byte{0xff}, EqLen)
	if err := ValidateEq(p); err == nil {
		t.Fatal("ValidateEq accepted a 73x0xFF payload")
	}
	if _, err := ApplyEq(p, EqOp{MasterCenti: new(int)}); err == nil {
		t.Fatal("ApplyEq accepted a 73x0xFF payload")
	}
}

func TestSaveEqBackupAllFFCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	p := bytes.Repeat([]byte{0xff}, EqLen)
	if _, err := SaveEqBackupTo(dir, p, time.Now()); err == nil {
		t.Fatal("SaveEqBackupTo accepted a 73x0xFF payload")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("backup files were created for an all-0xFF payload: %v", ents)
	}
}
