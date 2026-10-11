package qcycore

import (
	"bytes"
	"encoding/hex"
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
	hi, lo := 1201, -1201
	if _, err := ApplyEq(cur, EqOp{MasterCenti: &hi}); err == nil {
		t.Fatal("master 1201 accepted")
	}
	if _, err := ApplyEq(cur, EqOp{MasterCenti: &lo}); err == nil {
		t.Fatal("master -1201 accepted")
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

// realEq143 is the 0x22 payload (143 bytes) reported by the real device:
// 73 known bytes followed by 70 zero bytes.
const realEq143 = "0000001f00e0fc6400003e0000006400007d000000640000fa000000640000f4010000640000e8030000640000d0070000640000a00f0000640000401f0000640000803e000064000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

func real143(t *testing.T) []byte {
	t.Helper()
	p, err := hex.DecodeString(realEq143)
	if err != nil || len(p) != EqReadLen {
		t.Fatalf("fixture: %v len %d", err, len(p))
	}
	return p
}

func TestRealPayload143(t *testing.T) {
	cur := real143(t)
	if err := ValidateEq(cur); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEq(cur[:EqLen]); err != nil {
		t.Fatalf("73-byte payload rejected: %v", err)
	}
	m := -50
	next, err := ApplyEq(cur, EqOp{MasterCenti: &m})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != EqLen {
		t.Fatalf("write length %d, want %d", len(next), EqLen)
	}
	want := append([]byte(nil), cur[:EqLen]...)
	want[1], want[2] = byte(int16(m)), byte(int16(m)>>8)
	if !bytes.Equal(next, want) {
		t.Fatalf("patched payload differs beyond bytes 1-2:\n got %x\nwant %x", next, want)
	}
	// echo of 143 bytes confirms; stale echo does not
	echo := append(append([]byte(nil), next...), make([]byte, EqReadLen-EqLen)...)
	match := EqMatch(next, EqOp{MasterCenti: &m})
	if !match(echo) {
		t.Fatal("143-byte echo not confirmed")
	}
	if match(cur) {
		t.Fatal("pre-change 143-byte echo confirmed")
	}
	if match(echo[:EqLen-1]) {
		t.Fatal("short echo confirmed")
	}
	s, err := DecodeEq(cur)
	if err != nil || len(s.Bands) != EqBands || s.Bands[0].Freq != 31 || len(s.Payload) != 2*EqReadLen {
		t.Fatalf("decode: %+v %v", s, err)
	}
}

func TestEqAllFFAndShortRejected(t *testing.T) {
	if err := ValidateEq(bytes.Repeat([]byte{0xff}, EqReadLen)); err == nil {
		t.Fatal("143x0xFF accepted")
	}
	if err := ValidateEq(real143(t)[:EqLen-1]); err == nil {
		t.Fatal("72-byte payload accepted")
	}
}

// Real payloads read from the device (slot 11 custom, 13 bands; slot 6 factory
// Classico, 5 bands with 0xFF empty-type markers in every unused position).
const (
	realCustom11  = "0b50fb320064003c0002c80076fd3c000284039cff460002f00ac800780002401f44fd5e01023f002c014600027d000000460002fa000000460002f4010000460002e8036400460002d0076400460002a00f6400460002401f900146000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	realClassico6 = "0650fb3c002c01320002c8000cfe46000284039cff460002f00ac800780002401f44fd5e0102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	p, err := hex.DecodeString(s)
	if err != nil || len(p) != EqReadLen {
		t.Fatalf("fixture: %v len %d", err, len(p))
	}
	return p
}

func TestRealCustomDecodes13Bands(t *testing.T) {
	cur := mustHex(t, realCustom11)
	s, err := DecodeEq(cur)
	if err != nil {
		t.Fatal(err)
	}
	if s.Slot != 11 || s.MasterGain != -12 || len(s.Bands) != 13 {
		t.Fatalf("slot=%d master=%v bands=%d", s.Slot, s.MasterGain, len(s.Bands))
	}
	for i, b := range s.Bands {
		if b.Index != i {
			t.Fatalf("band %d has index %d", i, b.Index)
		}
	}
	if s.Bands[12].Freq != 8000 || s.Bands[12].Gain != 4 || s.Bands[12].Q != 70 || s.Bands[1].Gain != -6.5 {
		t.Fatalf("bands = %+v", s.Bands)
	}
}

func TestRealClassicoDecodes5Bands(t *testing.T) {
	s, err := DecodeEq(mustHex(t, realClassico6))
	if err != nil {
		t.Fatal(err)
	}
	if s.Slot != 6 || s.MasterGain != -12 || len(s.Bands) != 5 || s.Bands[4].Freq != 8000 || s.Bands[4].Gain != -7 {
		t.Fatalf("%+v", s)
	}
}

func TestRealCustomMasterPatchSendsFull143(t *testing.T) {
	cur := mustHex(t, realCustom11)
	if TailBlank(cur) {
		t.Fatal("custom tail considered blank")
	}
	m := -600
	next, err := ApplyEq(cur, EqOp{MasterCenti: &m})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != EqReadLen || !FullLength(next) {
		t.Fatalf("len %d", len(next))
	}
	for i := range cur {
		if (i == 1 || i == 2) == (next[i] == cur[i]) && i > 2 {
			t.Fatalf("byte %d unexpectedly changed", i)
		}
		if i != 1 && i != 2 && next[i] != cur[i] {
			t.Fatalf("byte %d changed: %02x -> %02x", i, cur[i], next[i])
		}
	}
	if int16(uint16(next[1])|uint16(next[2])<<8) != -600 {
		t.Fatalf("master bytes %x", next[1:3])
	}
	// the echo must match all 143 bytes
	match := EqMatch(next, EqOp{MasterCenti: &m})
	if !match(next) {
		t.Fatal("exact echo not confirmed")
	}
	for _, off := range []int{0, 1, 5, 80, 142} {
		bad := append([]byte(nil), next...)
		bad[off] ^= 1
		if match(bad) {
			t.Fatalf("echo diverging at byte %d confirmed", off)
		}
	}
	if match(next[:EqLen]) {
		t.Fatal("73-byte echo confirmed a 143-byte write")
	}
	// positions 10+ stay as read when `eq set` patches positions 0-9
	g := [EqBands]int{100, 100, 100, 100, 100, 100, 100, 100, 100, 100}
	n2, err := ApplyEq(cur, EqOp{Gains: &g})
	if err != nil || len(n2) != EqReadLen || !bytes.Equal(n2[73:], cur[73:]) {
		t.Fatalf("eq set altered the tail or length: %v", err)
	}
}

func TestRealClassicoMasterPatchSends73(t *testing.T) {
	cur := mustHex(t, realClassico6)
	if !TailBlank(cur) {
		t.Fatal("Classico tail (0xFF type markers) not considered blank")
	}
	m := 0
	next, err := ApplyEq(cur, EqOp{MasterCenti: &m})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != EqLen || FullLength(next) {
		t.Fatalf("len %d", len(next))
	}
	want := append([]byte(nil), cur[:EqLen]...)
	want[1], want[2] = 0, 0
	if !bytes.Equal(next, want) {
		t.Fatalf("got %x want %x", next, want)
	}
	// a 143-byte echo whose first 73 bytes match confirms
	echo := append([]byte(nil), cur...)
	echo[1], echo[2] = 0, 0
	if !EqMatch(next, EqOp{MasterCenti: &m})(echo) {
		t.Fatal("143-byte echo not confirmed")
	}
}

func TestMasterRange12(t *testing.T) {
	cur := mustHex(t, realClassico6)
	for _, c := range []int{-1200, 1200} {
		c := c
		if _, err := ApplyEq(cur, EqOp{MasterCenti: &c}); err != nil {
			t.Errorf("master %d rejected: %v", c, err)
		}
	}
	for _, c := range []int{-1201, 1201, 40000} {
		c := c
		if _, err := ApplyEq(cur, EqOp{MasterCenti: &c}); err == nil {
			t.Errorf("master %d accepted", c)
		}
	}
	for _, db := range []float64{12, -12, 9.5, 0.5} {
		if _, err := MasterCentiFromDB(db); err != nil {
			t.Errorf("MasterCentiFromDB(%v): %v", db, err)
		}
	}
	for _, db := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 12.01, -12.01, 1e30} {
		if _, err := MasterCentiFromDB(db); err == nil {
			t.Errorf("MasterCentiFromDB(%v) accepted", db)
		}
	}
}
