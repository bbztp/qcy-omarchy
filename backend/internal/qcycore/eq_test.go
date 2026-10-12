package qcycore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"strings"
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
	cur := fixture(8, 0)
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
	// The 143-byte write is not proven on the device: without the explicit
	// flag the master-gain patch of slot 11 is refused, and nothing is built.
	next, err := ApplyEq(cur, EqOp{MasterCenti: &m})
	if err == nil || next != nil {
		t.Fatalf("143-byte write allowed without AllowFullLength: %x", next)
	}
	if !errors.Is(err, ErrFullLengthRequired) || !strings.Contains(err.Error(), "--full-length") || !strings.Contains(err.Error(), "slot 11") {
		t.Fatalf("refusal = %v", err)
	}
	next, err = ApplyEq(cur, EqOp{MasterCenti: &m, AllowFullLength: true})
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
	if _, err := ApplyEq(cur, EqOp{Gains: &g}); !errors.Is(err, ErrFullLengthRequired) {
		t.Fatalf("eq set on a populated tail without AllowFullLength: %v", err)
	}
	n2, err := ApplyEq(cur, EqOp{Gains: &g, AllowFullLength: true})
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

// bands3 is a small user curve: three bands at positions 0, 4 and 9.
func bands3() []BandSpec {
	return []BandSpec{
		{Index: 0, Freq: 100, Gain: 1.5, Q: 100, Type: 2},
		{Index: 4, Freq: 1000, Gain: -2, Q: 70, Type: 0},
		{Index: 9, Freq: 8000, Gain: 3.25, Q: 120, Type: 2},
	}
}

func intp(n int) *int { return &n }

// Active slot 11 holds a 13-band custom curve; writing three bands to slot 0
// must build the payload from the bands alone: nothing of slot 11's bands,
// master gain or tail may travel with it.
func TestBandsDoNotLeakActiveSlot(t *testing.T) {
	cur := mustHex(t, realCustom11)
	op := EqOp{Slot: intp(0), Bands: bands3(), MasterCenti: intp(-300), AllowProtected: true}
	next, err := ApplyEq(cur, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != EqLen {
		t.Fatalf("len %d, want %d", len(next), EqLen)
	}
	s, err := DecodeEq(next)
	if err != nil {
		t.Fatal(err)
	}
	if s.Slot != 0 || s.MasterGain != -3 || len(s.Bands) != 3 {
		t.Fatalf("slot=%d master=%v bands=%+v", s.Slot, s.MasterGain, s.Bands)
	}
	want := bands3()
	for i, b := range s.Bands {
		w := want[i]
		if b.Index != w.Index || b.Freq != w.Freq || b.Gain != w.Gain || b.Q != w.Q || b.Type != w.Type {
			t.Fatalf("band %d = %+v, want %+v", i, b, w)
		}
	}
	// every unnamed position is zero, byte for byte (no 0xFF markers either)
	for pos := 0; pos < EqBands; pos++ {
		named := pos == 0 || pos == 4 || pos == 9
		o := 3 + pos*7
		if !named && !bytes.Equal(next[o:o+7], make([]byte, 7)) {
			t.Fatalf("position %d not empty: %x", pos, next[o:o+7])
		}
	}
	// none of slot 11's frequencies (31 Hz.. 16 kHz table) survived
	for _, b := range mustDecode(t, cur).Bands {
		for _, nb := range s.Bands {
			if nb.Freq == b.Freq && nb.Index != 0 && nb.Index != 4 && nb.Index != 9 {
				t.Fatalf("band of the active slot leaked: %+v", b)
			}
		}
	}
	// the master gain of slot 11 (-12 dB) is not inherited by another slot
	if _, err := ApplyEq(cur, EqOp{Slot: intp(0), Bands: bands3(), AllowProtected: true}); err == nil {
		t.Fatal("bands for another slot accepted without an explicit master gain")
	}
	// and without the override the cross-slot write is refused outright
	if _, err := ApplyEq(cur, EqOp{Slot: intp(0), Bands: bands3(), MasterCenti: intp(0)}); !errors.Is(err, ErrProtectedSlot) {
		t.Fatalf("cross-slot write from protected slot 11: %v", err)
	}
}

func mustDecode(t *testing.T, p []byte) EqSnapshot {
	t.Helper()
	s, err := DecodeEq(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBandsSizeAndValidation(t *testing.T) {
	cur := fixture(8, 100) // unprotected active slot, master +1 dB
	m := 0

	// positions 0-9: 73 bytes, zero tail, active slot's master kept when unnamed
	next, err := ApplyEq(cur, EqOp{Bands: bands3()})
	if err != nil || len(next) != EqLen || next[0] != 8 || masterCenti(next) != 100 {
		t.Fatalf("same-slot build: %x %v", next, err)
	}
	// all ten positions
	var ten []BandSpec
	for i := 0; i < 10; i++ {
		ten = append(ten, BandSpec{Index: i, Freq: 100 * (i + 1), Gain: 0, Q: 100, Type: 2})
	}
	next, err = ApplyEq(cur, EqOp{Bands: ten, MasterCenti: &m})
	if err != nil || len(next) != EqLen {
		t.Fatalf("ten bands: len %d %v", len(next), err)
	}

	// a position >= 10 needs the 143-byte format and the explicit flag
	wide := append(bands3(), BandSpec{Index: 12, Freq: 12000, Gain: 1, Q: 100, Type: 2})
	if _, err := ApplyEq(cur, EqOp{Bands: wide, MasterCenti: &m}); !errors.Is(err, ErrFullLengthRequired) {
		t.Fatalf("143-byte build without flag: %v", err)
	}
	next, err = ApplyEq(cur, EqOp{Bands: wide, MasterCenti: &m, AllowFullLength: true})
	if err != nil || len(next) != EqReadLen {
		t.Fatalf("143-byte build: len %d %v", len(next), err)
	}
	if s := mustDecode(t, next); len(s.Bands) != 4 || s.Bands[3].Index != 12 || s.Bands[3].Freq != 12000 {
		t.Fatalf("decoded %+v", s.Bands)
	}
	if !bytes.Equal(next[3+10*7:3+12*7], make([]byte, 14)) || !bytes.Equal(next[3+13*7:], make([]byte, EqReadLen-3-13*7)) {
		t.Fatalf("unnamed tail positions not zero: %x", next[73:])
	}

	nan := math.NaN()
	bad := map[string][]BandSpec{
		"empty":        {},
		"type 1":       {{Index: 0, Freq: 100, Gain: 0, Q: 100, Type: 1}},
		"type 0xff":    {{Index: 0, Freq: 100, Gain: 0, Q: 100, Type: 0xff}},
		"freq 0":       {{Index: 0, Freq: 0, Gain: 0, Q: 100, Type: 2}},
		"freq high":    {{Index: 0, Freq: 20001, Gain: 0, Q: 100, Type: 2}},
		"q 0":          {{Index: 0, Freq: 100, Gain: 0, Q: 0, Type: 2}},
		"q u16":        {{Index: 0, Freq: 100, Gain: 0, Q: 70000, Type: 2}},
		"gain 8.01":    {{Index: 0, Freq: 100, Gain: 8.01, Q: 100, Type: 2}},
		"gain NaN":     {{Index: 0, Freq: 100, Gain: nan, Q: 100, Type: 2}},
		"index -1":     {{Index: -1, Freq: 100, Gain: 0, Q: 100, Type: 2}},
		"index 20":     {{Index: 20, Freq: 100, Gain: 0, Q: 100, Type: 2}},
		"duplicate":    {{Index: 1, Freq: 100, Gain: 0, Q: 100, Type: 2}, {Index: 1, Freq: 200, Gain: 0, Q: 100, Type: 2}},
		"more than 20": make([]BandSpec, 21),
	}
	for name, b := range bad {
		if _, err := ApplyEq(cur, EqOp{Bands: b, MasterCenti: &m, AllowFullLength: true, AllowProtected: true}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	g := [EqBands]int{}
	if _, err := ApplyEq(cur, EqOp{Bands: bands3(), Gains: &g}); err == nil {
		t.Error("bands together with gains accepted")
	}
	big := 1201
	if _, err := ApplyEq(cur, EqOp{Bands: bands3(), MasterCenti: &big}); err == nil {
		t.Error("master 1201 accepted with bands")
	}
	// a payload that cannot be a live reading is still refused before building
	if _, err := ApplyEq(bytes.Repeat([]byte{0xff}, EqReadLen), EqOp{Bands: bands3(), MasterCenti: &m}); err == nil {
		t.Error("bands built over an all-0xFF read")
	}
}

func TestProtectedSlots(t *testing.T) {
	if len(ProtectedSlots) != 2 || !IsProtectedSlot(6) || !IsProtectedSlot(11) || IsProtectedSlot(0) {
		t.Fatalf("ProtectedSlots = %v", ProtectedSlots)
	}
	classico := mustHex(t, realClassico6) // active 6, blank tail: 73-byte writes
	custom := mustHex(t, realCustom11)    // active 11, populated tail
	plain := fixture(8, 0)                // active 8, unprotected
	m := 0
	cases := []struct {
		name    string
		cur     []byte
		op      EqOp
		refused bool
	}{
		{"unprotected -> unprotected", plain, EqOp{Slot: intp(3)}, false},
		{"unprotected -> 6", plain, EqOp{Slot: intp(6)}, true},
		{"unprotected -> 11", plain, EqOp{Slot: intp(11)}, true},
		{"unprotected -> 11 allowed", plain, EqOp{Slot: intp(11), AllowProtected: true}, false},
		{"6 master, slot nil", classico, EqOp{MasterCenti: &m}, false},
		{"6 -> 0 cross-slot", classico, EqOp{Slot: intp(0)}, true},
		{"6 -> 0 cross-slot allowed", classico, EqOp{Slot: intp(0), AllowProtected: true}, false},
		{"6 explicit own slot", classico, EqOp{Slot: intp(6)}, true},
		{"6 bands in place", classico, EqOp{Bands: bands3()}, true},
		{"6 bands in place allowed", classico, EqOp{Bands: bands3(), AllowProtected: true}, false},
		{"11 -> 0 cross-slot", custom, EqOp{Slot: intp(0), AllowFullLength: true}, true},
		{"11 -> 0 cross-slot bands", custom, EqOp{Slot: intp(0), Bands: bands3(), MasterCenti: &m}, true},
	}
	for _, c := range cases {
		next, err := ApplyEq(c.cur, c.op)
		switch {
		case c.refused && (!errors.Is(err, ErrProtectedSlot) || next != nil):
			t.Errorf("%s: want a protected-slot refusal, got %x / %v", c.name, next, err)
		case c.refused && !strings.Contains(err.Error(), "--allow-protected"):
			t.Errorf("%s: refusal does not name the override: %v", c.name, err)
		case !c.refused && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		}
	}
	// the refusal names the slot
	_, err := ApplyEq(plain, EqOp{Slot: intp(11)})
	if err == nil || !strings.Contains(err.Error(), "slot 11") {
		t.Fatalf("message = %v", err)
	}
	_, err = ApplyEq(classico, EqOp{Slot: intp(0)})
	if err == nil || !strings.Contains(err.Error(), "slot 6") || !strings.Contains(err.Error(), "slot 0") {
		t.Fatalf("message = %v", err)
	}
}

// A 73-byte write is confirmed only if what the device reports beyond it is
// blank: a populated tail means the curve on the device is not the one sent.
func TestEqMatchRejectsDirtyTail(t *testing.T) {
	cur := mustHex(t, realClassico6)
	m := -300
	sent, err := ApplyEq(cur, EqOp{MasterCenti: &m})
	if err != nil || len(sent) != EqLen {
		t.Fatalf("%v len %d", err, len(sent))
	}
	match := EqMatch(sent, EqOp{MasterCenti: &m})

	echo := append([]byte(nil), cur...) // 143 bytes, 0xFF empty markers in the tail
	echo[1], echo[2] = byte(int16(m)), byte(int16(m)>>8)
	if !match(echo) {
		t.Fatal("echo with the 0xFF empty-type markers refused")
	}
	if !match(sent) {
		t.Fatal("exact 73-byte echo refused")
	}
	zero := append(append([]byte(nil), sent...), make([]byte, EqReadLen-EqLen)...)
	if !match(zero) {
		t.Fatal("echo with a zero tail refused")
	}
	// a band at position 10 (freq 1000 Hz) in the echoed tail
	dirty := append([]byte(nil), zero...)
	o := 3 + 10*7
	dirty[o], dirty[o+1] = 0xe8, 0x03
	if match(dirty) {
		t.Fatal("echo whose tail holds a band confirmed a 73-byte write")
	}
	// a stray non-zero byte that is not an empty-type marker
	dirty2 := append([]byte(nil), zero...)
	dirty2[EqReadLen-1] = 0x02
	if match(dirty2) {
		t.Fatal("echo with a stray tail byte confirmed")
	}
	// the head of the echo still has to match
	head := append([]byte(nil), zero...)
	head[10] ^= 1
	if match(head) {
		t.Fatal("echo with a changed band confirmed")
	}
}

func TestEqWriteFailureClassifiesOtherSlot(t *testing.T) {
	sent := fixture(0, 0)
	other := fixture(11, 0)
	err := EqWriteFailure(sent, other, "/b/eq-slot11-x.hex")
	var ee *EqError
	if !errors.Is(err, ErrUnconfirmed) || !errors.As(err, &ee) {
		t.Fatalf("other-slot echo: %v", err)
	}
	if ee.Code != CodeUnconfirmed || ee.Backup != "/b/eq-slot11-x.hex" || !bytes.Equal(ee.Sent, sent) || !bytes.Equal(ee.Got, other) {
		t.Fatalf("%+v", ee)
	}
	for _, want := range []string{"unconfirmed", "slot 0", "slot 11", "/b/eq-slot11-x.hex", hex.EncodeToString(sent)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	// same slot, different content: a plain mismatch, not "unconfirmed"
	stale := fixture(0, 50)
	err = EqWriteFailure(sent, stale, "/b/x")
	if err == nil || errors.Is(err, ErrUnconfirmed) || !strings.Contains(err.Error(), "readback mismatch") {
		t.Fatalf("same-slot echo: %v", err)
	}
	// silence is not unconfirmed either
	if err := EqWriteFailure(sent, nil, "/b/x"); err == nil || errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("no echo: %v", err)
	}
}
