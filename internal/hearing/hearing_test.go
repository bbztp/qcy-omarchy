package hearing

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"
)

// drive answers the current band with the given answers and returns the
// recorded gain, failing if the band is not finished exactly at the end.
func drive(t *testing.T, s *Session, answers ...bool) {
	t.Helper()
	ear, band := s.ear, s.band
	for i, a := range answers {
		if s.Done() || s.ear != ear || s.band != band {
			t.Fatalf("band ended early after %d answers", i)
		}
		if err := s.Answer(a); err != nil {
			t.Fatal(err)
		}
	}
	if !s.Done() && s.ear == ear && s.band == band {
		t.Fatalf("band not finished after %d answers (gain %d)", len(answers), s.gain)
	}
}

func rep(v bool, n int) []bool {
	a := make([]bool, n)
	for i := range a {
		a[i] = v
	}
	return a
}

func TestToneLevel(t *testing.T) {
	cases := []struct {
		db   float64
		f, g int
		want float64
	}{
		{-65.3, 63, 2, -55.3},
		{-65.3, 63, 0, -65.3},
		{-65.3, 63, -1, -65.3},  // (-1/2)=0 truncated
		{-65.3, 63, -3, -70.3},  // (-3/2)=-1
		{-65.3, 500, -4, -75.3}, // (-4/2)=-2
		{-86.9, 1000, -3, -101.9},
		{-86.9, 1000, 8, -46.9},
	}
	for _, c := range cases {
		if got := ToneLevel(c.db, c.f, c.g); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("ToneLevel(%v,%d,%d)=%v want %v", c.db, c.f, c.g, got, c.want)
		}
	}
}

func TestStartIsLeftStepTwo(t *testing.T) {
	s := NewSession(DefaultSoundDB)
	c := s.Current()
	if c.Ear != Left || c.Freq != 63 || c.Gain != 2 || math.Abs(c.LevelDB-(-55.3)) > 1e-9 {
		t.Fatalf("%+v", c)
	}
}

func TestLowBandHeardDescends(t *testing.T) {
	s := NewSession(DefaultSoundDB)
	// 2 -> 1 -> 0 -> -2 (extra) -> -4 (endCount 1) -> -6 (extra) -> -6 heard: endCount 2, record -6.
	drive(t, s, true, true, true, true, true, true)
	l, _ := s.Result()
	if l[0] != -6 {
		t.Fatalf("got %d", l[0])
	}
	if s.Current().Freq != 125 || s.Current().Gain != 2 {
		t.Fatalf("next band not reset: %+v", s.Current())
	}
}

func TestHighBandHeardDescends(t *testing.T) {
	s := NewSession(DefaultSoundDB)
	for i := 0; i < 4; i++ { // skip bands 0..3 as "not heard" up to +6
		drive(t, s, rep(false, 6)...)
	}
	if s.Current().Freq != 1000 {
		t.Fatalf("%+v", s.Current())
	}
	// 2,1,0,-1,-2 (endCount 1),-3 heard: endCount 2 -> record -3.
	drive(t, s, rep(true, 6)...)
	l, _ := s.Result()
	if l[4] != -3 {
		t.Fatalf("got %d", l[4])
	}
	if l[0] != 6 {
		t.Fatalf("not heard band should record 6, got %d", l[0])
	}
}

func TestNotHeardRecordsMax(t *testing.T) {
	s := NewSession(DefaultSoundDB)
	// 2,3,4,5,6 (endCount 1),7 (endCount 2 -> record 6): 6 answers.
	drive(t, s, rep(false, 6)...)
	l, _ := s.Result()
	if l[0] != 6 {
		t.Fatalf("got %d", l[0])
	}
}

func TestSameGainTwiceRecords(t *testing.T) {
	s := NewSession(DefaultSoundDB)
	// heard 2 -> 1; heard 1 -> 0; not heard 0 -> 1 (low band, g=0 not <0: +1); heard at 1: same as last? last=0, no.
	// Use: heard at 2 (last=2), not heard... gain climbs 1->? start: heard@2 ->gain1; not heard@1 -> 2; heard@2 == last -> record 2.
	drive(t, s, true, false, true)
	l, _ := s.Result()
	if l[0] != 2 {
		t.Fatalf("got %d", l[0])
	}
}

func TestLowBandNotHeardNegativeStepsTwo(t *testing.T) {
	s := NewSession(DefaultSoundDB)
	drive2 := func(a bool) {
		if err := s.Answer(a); err != nil {
			t.Fatal(err)
		}
	}
	drive2(true) // 1
	drive2(true) // 0
	drive2(true) // -2
	if s.gain != -2 {
		t.Fatal(s.gain)
	}
	drive2(false) // -2 +1 +1 extra = 0
	if s.gain != 0 {
		t.Fatalf("got %d", s.gain)
	}
}

func TestFullTestRightEarAndDone(t *testing.T) {
	s := NewSession(DefaultSoundDB)
	for i := 0; i < 8; i++ {
		drive(t, s, rep(false, 6)...)
	}
	if s.Current().Ear != Right || s.Current().Band != 0 {
		t.Fatalf("%+v", s.Current())
	}
	for i := 0; i < 8; i++ {
		drive(t, s, rep(false, 6)...)
	}
	if !s.Done() {
		t.Fatal("not done")
	}
	if err := s.Answer(true); err == nil {
		t.Fatal("answer after done should fail")
	}
	l, r := s.Result()
	for i := range l {
		if l[i] != 6 || r[i] != 6 {
			t.Fatal(l, r)
		}
	}
}

func TestLowestStepRecorded(t *testing.T) {
	s := NewSession(DefaultSoundDB)
	// A heard answer at the floor must not leave the range.
	s.gain = MinStep
	s.endCnt = 0
	if err := s.Answer(true); err != nil {
		t.Fatal(err)
	}
	l, _ := s.Result()
	if l[0] != MinStep {
		t.Fatalf("got %d", l[0])
	}
}

func TestMasterGainZeroCurve(t *testing.T) {
	if g := MasterGain([Bands]float64{}); g != -10.0 {
		t.Fatalf("got %v", g)
	}
}

func TestMasterGainLowBoostReduces(t *testing.T) {
	g := MasterGain([Bands]float64{6, 6, 6, 0, 0, 0, 0, 0})
	if g >= -10.0 {
		t.Fatalf("got %v", g)
	}
	// Mild curve below volumeLevel keeps the base.
	if g := MasterGain([Bands]float64{-6, -3, 0, 1, 1, 0, 0, 0}); g != -10 {
		t.Fatalf("got %v", g)
	}
	// High-frequency boost: reduced only past volumeLevel+4.
	if g := MasterGain([Bands]float64{0, 0, 0, 0, 0, 0, 2, 2}); g != -10 {
		t.Fatalf("got %v", g)
	}
}

func TestWriteFrameBytes(t *testing.T) {
	g := [Bands]float64{-8, -3.5, 0, 1, 2, 3, 4, 6}
	f := WriteFrame(Left, g, -10.0)
	if len(f) != 4+59 {
		t.Fatalf("len %d", len(f))
	}
	head := []byte{0xff, 0x3d, 0x46, 0x3b, 0x0b, 0x18, 0xfc} // mg -1000 = 0xFC18
	if !bytes.Equal(f[:7], head) {
		t.Fatalf("head %x", f[:7])
	}
	// band 0: freq 63, gain -800 (0xFCE0), q 70, type 2
	if got := hex.EncodeToString(f[7:14]); got != "3f00e0fc460002" {
		t.Fatalf("band0 %s", got)
	}
	// band 1: freq 125, gain -350 = 0xFEA2
	if got := hex.EncodeToString(f[14:21]); got != "7d00a2fe460002" {
		t.Fatalf("band1 %s", got)
	}
	if WriteFrame(Right, g, -10)[2] != 0x47 {
		t.Fatal("right ear cmd")
	}
	if !bytes.Equal(FlagFrame(true), []byte{0xff, 3, 0x45, 1, 1}) || !bytes.Equal(FlagFrame(false), []byte{0xff, 3, 0x45, 1, 0}) {
		t.Fatal("flag")
	}
	// bodyLen counts every byte after itself.
	if int(f[1]) != len(f)-2 || int(f[3]) != len(f)-4 {
		t.Fatal("length bytes")
	}
}

func TestMasterGainTruncatesInFrame(t *testing.T) {
	f := WriteFrame(Left, [Bands]float64{}, -10.567)
	if f[5] != 0xa8 || f[6] != 0xfb { // int(-1056.7) = -1056 = 0xFBE0? check below
		// -1056 = 0xFBE0
		if f[5] != 0xe0 || f[6] != 0xfb {
			t.Fatalf("%x %x", f[5], f[6])
		}
	}
}

func TestToneChannelAndRamp(t *testing.T) {
	for _, ear := range []Ear{Left, Right} {
		pcm, err := GenerateTone(ear, 1000, -20, 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(pcm) != 44100*200/1000*4 {
			t.Fatalf("len %d", len(pcm))
		}
		var peak int
		for i := 0; i+3 < len(pcm); i += 4 {
			l := int(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
			r := int(int16(uint16(pcm[i+2]) | uint16(pcm[i+3])<<8))
			own, other := l, r
			if ear == Right {
				own, other = r, l
			}
			if other != 0 {
				t.Fatalf("other channel not silent at %d", i/4)
			}
			if a := int(math.Abs(float64(own))); a > peak {
				peak = a
			}
		}
		want := 0.1 * 32767
		if math.Abs(float64(peak)-want) > want*0.02 {
			t.Fatalf("peak %d want ~%v", peak, want)
		}
		// First sample is silent (ramp starts at 0).
		if pcm[0] != 0 || pcm[1] != 0 || pcm[2] != 0 || pcm[3] != 0 {
			t.Fatal("no ramp-in")
		}
	}
	if _, err := GenerateTone(Left, 0, -20, 100); err == nil {
		t.Fatal("expected error")
	}
}

func TestSynthChunksAreContinuousAndRamped(t *testing.T) {
	s := NewSynth(Right, 1000, -20)
	r := RampSamples()
	in := s.Next(r, FadeIn)
	mid := s.Next(441, Steady)
	out := s.Next(r, FadeOut)
	if len(in) != r*4 || len(mid) != 441*4 {
		t.Fatalf("sizes %d %d", len(in), len(mid))
	}
	smp := func(b []byte, i int) (l, r int16) {
		return int16(uint16(b[i*4]) | uint16(b[i*4+1])<<8), int16(uint16(b[i*4+2]) | uint16(b[i*4+3])<<8)
	}
	if l, r0 := smp(in, 0); l != 0 || r0 != 0 {
		t.Fatal("must start at zero")
	}
	if l, _ := smp(mid, 10); l != 0 {
		t.Fatal("left must stay silent for the right ear")
	}
	peak := int16(0)
	for i := 0; i < 441; i++ {
		if _, v := smp(mid, i); v > peak {
			peak = v
		}
	}
	if want := 3277; peak < int16(want-5) || peak > int16(want+5) {
		t.Fatalf("steady peak %d, want ~%d", peak, want)
	}
	if _, v := smp(out, r-1); v != 0 {
		t.Fatalf("must end at zero, got %d", v)
	}
}

func TestSessionFloorFlags(t *testing.T) {
	// Always heard: every band ends at the staircase floor.
	s := NewSession(DefaultSoundDB)
	for !s.Done() {
		if err := s.Answer(true); err != nil {
			t.Fatal(err)
		}
	}
	fl, fr := s.Floor()
	for i := 0; i < Bands; i++ {
		if !fl[i] || !fr[i] {
			t.Fatalf("band %d not flagged: %v %v", i, fl, fr)
		}
	}
	// Never heard: ends at +6, no floor.
	s = NewSession(DefaultSoundDB)
	for !s.Done() {
		s.Answer(false)
	}
	fl, fr = s.Floor()
	for i := 0; i < Bands; i++ {
		if fl[i] || fr[i] {
			t.Fatalf("band %d flagged without being heard", i)
		}
	}
}

// answerSeq feeds answers to the first band of a session ("h"/"n").
func answerSeq(t *testing.T, s *Session, seq string, wantGains []int) {
	t.Helper()
	for i, c := range seq {
		if got := s.Current().Gain; got != wantGains[i] {
			t.Fatalf("step %d: tone at %+d, want %+d", i, got, wantGains[i])
		}
		if err := s.Answer(c == 'h'); err != nil {
			t.Fatal(err)
		}
	}
}

// advance records bands with "never heard" answers until the session stands
// at the given band of the left ear.
func advance(s *Session, band int) {
	for s.Current().Band != band {
		s.Answer(false)
	}
}

func TestFloorOnlyWhenTheThresholdWasNeverBracketed(t *testing.T) {
	low := func(seq string, gains []int) (rec int, floor bool) {
		s := NewSession(DefaultSoundDB)
		answerSeq(t, s, seq, gains)
		l, _ := s.Result()
		fl, _ := s.Floor()
		if s.Current().Band != 1 {
			t.Fatalf("band 0 did not end: %+v", s.Current())
		}
		return l[0], fl[0]
	}
	// 63 Hz: 2 1 0 -2 -4 -6 all heard: recorded -6 at the floor.
	if g, f := low("hhhhhh", []int{2, 1, 0, -2, -4, -6}); g != -6 || !f {
		t.Fatalf("heard to the bottom: %d floor=%v", g, f)
	}
	// heard -4, not heard -6, heard -4: bracketed, a measurement.
	if g, f := low("hhhhhnh", []int{2, 1, 0, -2, -4, -6, -4}); g != -4 || f {
		t.Fatalf("bracketed: %d floor=%v", g, f)
	}
	// heard +2, not heard +1, heard +2: not floor.
	if g, f := low("hnh", []int{2, 1, 2}); g != 2 || f {
		t.Fatalf("+2: %d floor=%v", g, f)
	}
	// 2000 Hz (not low): 2 1 0 -1 -2 -3 all heard -> floor; bracketed -> not.
	hi := func(seq string, gains []int) (int, bool) {
		s := NewSession(DefaultSoundDB)
		advance(s, 5)
		answerSeq(t, s, seq, gains)
		l, _ := s.Result()
		fl, _ := s.Floor()
		return l[5], fl[5]
	}
	if g, f := hi("hhhhhh", []int{2, 1, 0, -1, -2, -3}); g != -3 || !f {
		t.Fatalf("high heard to the bottom: %d floor=%v", g, f)
	}
	if g, f := hi("hhhhhnh", []int{2, 1, 0, -1, -2, -3, -2}); g != -2 || f {
		t.Fatalf("high bracketed: %d floor=%v", g, f)
	}
}

// Floor is decided by POSITION: only a "not heard" BELOW the recorded gain
// brackets the threshold. A stray "not heard" at the top of the staircase
// before the descent does not hide a listener who then hears everything down
// to the bottom.
func TestFloorIgnoresNotHeardAboveTheRecordedGain(t *testing.T) {
	run := func(seq string, gains []int) (int, bool) {
		s := NewSession(DefaultSoundDB)
		answerSeq(t, s, seq, gains)
		l, _ := s.Result()
		fl, _ := s.Floor()
		if s.Current().Band != 1 {
			t.Fatalf("band 0 did not end: %+v", s.Current())
		}
		return l[0], fl[0]
	}
	// not heard at +2, then heard 3, 2, 1, 0, -2, -4, -6.
	if g, f := run("nhhhhhhh", []int{2, 3, 2, 1, 0, -2, -4, -6}); g != -6 || !f {
		t.Fatalf("stray no at +2: %d floor=%v", g, f)
	}
	// not heard at +2 and +3, then heard 4 down to -6.
	if g, f := run("nnhhhhhhhh", []int{2, 3, 4, 3, 2, 1, 0, -2, -4, -6}); g != -6 || !f {
		t.Fatalf("two stray no: %d floor=%v", g, f)
	}
	// the same stray no, but the threshold IS bracketed from below (not heard
	// at -6, heard again at -4): a measurement.
	if g, f := run("nhhhhhhnh", []int{2, 3, 2, 1, 0, -2, -4, -6, -4}); g != -4 || f {
		t.Fatalf("bracketed: %d floor=%v", g, f)
	}
}

func TestWriteFrameBandsZeroBaseline(t *testing.T) {
	g := [Bands]float64{3, 4, 2, 1, 0, -2, 0, 5}
	f := WriteFrameBands(Left, ZeroBaselineBands(g), -10)
	// FF <bodyLen> 46 <paramLen> 0B <mg> 13 bands: paramLen 7*13+3 = 0x5E
	if f[0] != 0xff || f[2] != 0x46 || f[3] != 0x5e || f[1] != 0x60 || len(f) != 4+0x5e || f[4] != 0x0b {
		t.Fatalf("header % x len %d", f[:8], len(f))
	}
	body := f[7:] // after FF len cmd paramLen 0B mg(2)
	for i, b := range FactoryBaseline {
		o := i * 7
		freq := int(body[o]) | int(body[o+1])<<8
		gain := int(int16(uint16(body[o+2]) | uint16(body[o+3])<<8))
		q := int(body[o+4]) | int(body[o+5])<<8
		if freq != b.Freq || gain != 0 || q != b.Q || int(body[o+6]) != b.Type {
			t.Fatalf("baseline band %d: %d Hz %d q %d", i, freq, gain, q)
		}
	}
	// the 8 test bands follow, identical to the ones of the plain frame
	plain := WriteFrame(Left, g, -10)
	if string(f[7+5*7:]) != string(plain[7:]) {
		t.Fatal("the test bands differ from WriteFrame's")
	}
	// the 8-band builder is the old frame
	var eight []FrameBand
	for i, v := range g {
		eight = append(eight, FrameBand{Freq: Freqs[i], Gain: v, Q: 70, Type: 2})
	}
	if string(WriteFrameBands(Right, eight, -10)) != string(WriteFrame(Right, g, -10)) {
		t.Fatal("WriteFrameBands(8 bands) != WriteFrame")
	}
}
