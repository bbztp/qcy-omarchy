// Package hearing is the pure core of the QCY app's "hearing test" (the
// personalised listening experience, sound effect id 11). It holds the
// per-band tone level rule, the staircase state machine that turns the
// listener's answers into one gain per band and ear, the master-gain rule, the
// bytes of the two write frames and a PCM tone generator.
//
// Nothing here talks to the earbuds or to an audio device. The frame builders
// only construct bytes; sending them is the caller's decision. Everything was
// taken from the vendor app and is NOT verified on hardware.
package hearing

import (
	"errors"
	"fmt"
	"math"
)

// Bands is the number of test bands per ear.
const Bands = 8

// Freqs are the tested frequencies in Hz (individuation_eq.freq).
var Freqs = [Bands]int{63, 125, 250, 500, 1000, 2000, 4000, 8000}

// DefaultSoundDB is the per-band reference tone level in dBFS. These behaviours were derived and verified on a QCY MeloBuds N70, the device used as the test model for this code; other models are unverified. The values are those of the QCY MeloBuds N70 (vendorId 23877 / 23872): `individuation_eq.soundDB` of the control panel
// 23877 (product/findControlPanels, server panel JSON,
// layout 2). It is not part of the embedded models snapshot, so it is a
// constant here; callers may pass another table per model.
var DefaultSoundDB = [Bands]float64{-65.3, -68.0, -72.6, -80.7, -86.9, -90.3, -90.4, -81.6}

const (
	// MinStep and MaxStep bound the staircase position of a band.
	MinStep = -8
	MaxStep = 8
	// MaxGain is the highest gain that is stored (a band that is not heard at
	// +6 is recorded as +6).
	MaxGain = 6
	// InitialStep is the first step of every band.
	InitialStep = 2
	// StepDB is the level change per step, in dB.
	StepDB = 5
	// LowBandMaxHz: bands up to this frequency use the "low" rules.
	LowBandMaxHz = 500
)

// Ear selects a side.
type Ear int

const (
	Left Ear = iota
	Right
)

func (e Ear) String() string {
	if e == Left {
		return "left"
	}
	return "right"
}

// ToneLevel is the level in dBFS of the test tone of a band at a step:
// soundDB + shift, where the shift is gain*5 for gain > 0, (gain/2)*5 with a
// truncating integer division for gain < 0 at f <= 500 Hz, and gain*5 for
// gain < 0 at f > 500 Hz. Gain 0 adds nothing.
func ToneLevel(soundDB float64, freq, gain int) float64 {
	switch {
	case gain > 0:
		return soundDB + float64(gain*StepDB)
	case gain < 0 && freq <= LowBandMaxHz:
		return soundDB + float64((gain/2)*StepDB) // Go's / truncates toward zero
	case gain < 0:
		return soundDB + float64(gain*StepDB)
	}
	return soundDB
}

// Tone is what the session asks to be played next.
type Tone struct {
	Ear     Ear     `json:"ear"`
	Band    int     `json:"band"`
	Freq    int     `json:"freq"`
	Gain    int     `json:"gain"`
	LevelDB float64 `json:"levelDb"`
}

// Session is the staircase state machine: left ear first, then right, eight
// bands each. The caller plays Current(), then reports Answer(heard).
//
// Assumption (documented, not verified): endCount and lastListenedGain are
// reset at the start of every band, and lastListenedGain starts as "none".
type Session struct {
	soundDB [Bands]float64
	ear     Ear
	band    int
	gain    int
	endCnt  int
	last    int
	hasLast bool
	done    bool
	res     [2][Bands]int
}

// NewSession starts a test with the given per-band reference levels.
func NewSession(soundDB [Bands]float64) *Session {
	s := &Session{soundDB: soundDB}
	s.startBand()
	return s
}

func (s *Session) startBand() {
	s.gain = InitialStep
	s.endCnt = 0
	s.hasLast = false
}

// Done reports whether all 16 bands were recorded.
func (s *Session) Done() bool { return s.done }

// Current is the tone to play now. It is the zero Tone once Done.
func (s *Session) Current() Tone {
	if s.done {
		return Tone{}
	}
	f := Freqs[s.band]
	return Tone{Ear: s.ear, Band: s.band, Freq: f, Gain: s.gain, LevelDB: ToneLevel(s.soundDB[s.band], f, s.gain)}
}

func clampStep(g int) int {
	if g < MinStep {
		return MinStep
	}
	if g > MaxStep {
		return MaxStep
	}
	return g
}

func (s *Session) record(g int) {
	if g > MaxGain {
		g = MaxGain
	}
	if g < MinStep {
		g = MinStep
	}
	s.res[s.ear][s.band] = g
	s.band++
	if s.band == Bands {
		s.band = 0
		if s.ear == Left {
			s.ear = Right
		} else {
			s.done = true
			return
		}
	}
	s.startBand()
}

// Answer feeds the listener's response to Current(): heard true is "I hear it".
func (s *Session) Answer(heard bool) error {
	if s.done {
		return errors.New("hearing: session is complete")
	}
	f := Freqs[s.band]
	low := f <= LowBandMaxHz
	g := s.gain
	if !heard {
		if g >= MaxGain {
			s.endCnt++
			if s.endCnt >= 2 {
				s.record(MaxGain)
				return nil
			}
		}
		step := 1
		if low && g < 0 {
			step = 2
		}
		s.gain = clampStep(g + step)
		return nil
	}
	same := s.hasLast && g == s.last
	if (low && g <= -4) || (!low && g <= -2) || same {
		s.endCnt++
	}
	s.last, s.hasLast = g, true
	if s.endCnt >= 2 || same {
		s.record(g)
		return nil
	}
	step := 1
	if low && g <= 0 {
		step = 2
	}
	if g-step < MinStep {
		// Cannot go lower: the quietest tone was heard, record it.
		s.record(g)
		return nil
	}
	s.gain = g - step
	return nil
}

// Result is the recorded gain per band, in dB, for each ear. Only meaningful
// once Done.
func (s *Session) Result() (left, right [Bands]int) { return s.res[Left], s.res[Right] }

// ---------- master gain ----------

const (
	baseMasterGain = -10.0
	sampleRate     = 44100.0
	peakQ          = 0.7
	// VolumeLevel is the app's volumeLevel constant used by the master-gain rule.
	VolumeLevel = 4.0
)

// peaking returns the magnitude in dB at f of an RBJ peaking filter.
func peaking(f0, gainDB, q, f float64) float64 {
	if gainDB == 0 {
		return 0
	}
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * f0 / sampleRate
	alpha := math.Sin(w0) / (2 * q)
	b0, b1, b2 := 1+alpha*a, -2*math.Cos(w0), 1-alpha*a
	a0, a1, a2 := 1+alpha/a, -2*math.Cos(w0), 1-alpha/a
	w := 2 * math.Pi * f / sampleRate
	c1, s1 := math.Cos(w), math.Sin(w)
	c2, s2 := math.Cos(2*w), math.Sin(2*w)
	nr := b0 + b1*c1 + b2*c2
	ni := -(b1*s1 + b2*s2)
	dr := a0 + a1*c1 + a2*c2
	di := -(a1*s1 + a2*s2)
	return 10 * math.Log10((nr*nr+ni*ni)/(dr*dr+di*di))
}

// CurvePoint is one sample of the summed response.
type CurvePoint struct {
	Freq float64 `json:"freq"`
	DB   float64 `json:"db"`
}

// Curve is the summed peaking response (fs 44100, Q 0.7) of the eight bands,
// 20..20000 Hz in 5 Hz steps. The app also adds the default EQ curve; here it
// is taken as all zeros (documented assumption), so only the bands count.
func Curve(gains [Bands]float64) []CurvePoint {
	var out []CurvePoint
	for f := 20.0; f <= 20000; f += 5 {
		sum := 0.0
		for i, g := range gains {
			sum += peaking(float64(Freqs[i]), g, peakQ, f)
		}
		out = append(out, CurvePoint{Freq: f, DB: sum})
	}
	return out
}

// MasterGain is the master gain in dB the app writes with the curve: base
// -10; peak = highest point of Curve; d3 = peak + 0.5; if d3 > volumeLevel and
// the peak is below 1500 Hz then base + (volumeLevel - d3); if d3 >
// volumeLevel + 4 and the peak is at or above 1500 Hz then
// base + (volumeLevel + 4 - d3); else base.
func MasterGain(gains [Bands]float64) float64 {
	peak, pf := math.Inf(-1), 0.0
	for _, p := range Curve(gains) {
		if p.DB > peak {
			peak, pf = p.DB, p.Freq
		}
	}
	d3 := peak + 0.5
	switch {
	case d3 > VolumeLevel && pf < 1500:
		return baseMasterGain + (VolumeLevel - d3)
	case d3 > VolumeLevel+4 && pf >= 1500:
		return baseMasterGain + (VolumeLevel + 4 - d3)
	}
	return baseMasterGain
}

// MasterLimit is the largest master gain magnitude, in dB, that is written:
// PROTOCOL.md records -12..+12 as accepted by the 0x22 master gain field; the
// 0x46/0x47 field is assumed to be the same and is NOT verified.
const MasterLimit = 12.0

// ClampMaster limits a master gain to -MasterLimit..+MasterLimit and reports
// whether it had to.
func ClampMaster(mg float64) (float64, bool) {
	switch {
	case mg < -MasterLimit:
		return -MasterLimit, true
	case mg > MasterLimit:
		return MasterLimit, true
	}
	return mg, false
}

// ---------- frames ----------

const (
	// EffectID is the sound effect id of the personalised listening experience.
	EffectID = 11
	cmdLeft  = 0x46
	cmdRight = 0x47
	cmdFlag  = 0x45
	qCenti   = 70 // Q 0.7 * 100
	bandType = 2
)

func le16(b []byte, v int) []byte { return append(b, byte(v), byte(v>>8)) }

// ClampGain clamps a gain to the storable range -8..+6.
func ClampGain(g float64) float64 {
	return math.Max(MinStep, math.Min(MaxGain, g))
}

// WriteFrame builds (never sends) the curve frame of one ear:
//
//	FF <bodyLen> 46|47 <7n+3> 0B <mg int16 LE> {freq i16, gain i16, q u16, type u8} * n
//
// bodyLen counts every byte after itself (paramLen + 2, PROTOCOL.md "Packet
// format"). The 0x0B (effect 11) takes the place of the slot byte of the 0x22
// layout; the band layout is the 0x22 one (7 bytes). Not verified on hardware.
func WriteFrame(ear Ear, gains [Bands]float64, masterGain float64) []byte {
	params := []byte{EffectID}
	params = le16(params, int(masterGain*100)) // (int)(masterGain*100): truncates
	for i, g := range gains {
		params = le16(params, Freqs[i])
		params = le16(params, int(math.Round(ClampGain(g)*100)))
		params = le16(params, qCenti)
		params = append(params, bandType)
	}
	cmd := byte(cmdLeft)
	if ear == Right {
		cmd = cmdRight
	}
	out := []byte{0xff, byte(len(params) + 2), cmd, byte(len(params))}
	return append(out, params...)
}

// FlagFrame builds the frame that switches the personalised effect flag.
func FlagFrame(on bool) []byte {
	v := byte(0)
	if on {
		v = 1
	}
	return []byte{0xff, 0x03, cmdFlag, 0x01, v}
}

// ---------- tone generator ----------

// RampMs is the fade in and fade out length of a tone.
const RampMs = 20

// Tone generation: PCM s16le, stereo, 44100 Hz, a continuous sine with
// amplitude 10^(dB/20)*32767, only on the channel of the ear (the other is 0),
// with a 20 ms linear ramp at both ends.
func GenerateTone(ear Ear, freq int, levelDB float64, ms int) ([]byte, error) {
	if freq <= 0 || ms <= 0 {
		return nil, fmt.Errorf("hearing: invalid tone (freq %d, %d ms)", freq, ms)
	}
	n := int(sampleRate) * ms / 1000
	ramp := int(sampleRate) * RampMs / 1000
	if ramp*2 > n {
		ramp = n / 2
	}
	amp := math.Pow(10, levelDB/20) * 32767
	out := make([]byte, 0, n*4)
	for i := 0; i < n; i++ {
		env := 1.0
		if i < ramp {
			env = float64(i) / float64(ramp)
		} else if i >= n-ramp {
			env = float64(n-1-i) / float64(ramp)
		}
		v := int16(math.Round(amp * env * math.Sin(2*math.Pi*float64(freq)*float64(i)/sampleRate)))
		l, r := int16(0), int16(0)
		if ear == Left {
			l = v
		} else {
			r = v
		}
		out = append(out, byte(l), byte(uint16(l)>>8), byte(r), byte(uint16(r)>>8))
	}
	return out, nil
}

// ---------- streaming tone ----------

// Fade selects the envelope of one Synth chunk.
type Fade int

const (
	// Steady is the full amplitude.
	Steady Fade = iota
	// FadeIn ramps 0 -> 1 linearly over the chunk.
	FadeIn
	// FadeOut ramps 1 -> 0 linearly over the chunk.
	FadeOut
)

// Synth produces the same signal as GenerateTone (one channel, the other
// silent) as consecutive chunks with continuous phase, so a tone can be played
// for as long as the listener needs: a FadeIn chunk of RampMs, any number of
// Steady chunks, one FadeOut chunk of RampMs.
type Synth struct {
	ear  Ear
	freq int
	amp  float64
	pos  int
}

// NewSynth starts a continuous tone at levelDB dBFS.
func NewSynth(ear Ear, freq int, levelDB float64) *Synth {
	return &Synth{ear: ear, freq: freq, amp: math.Pow(10, levelDB/20) * 32767}
}

// RampSamples is the number of frames of one fade chunk.
func RampSamples() int { return int(sampleRate) * RampMs / 1000 }

// Next returns n stereo frames (s16le, 4 bytes each).
func (s *Synth) Next(n int, fade Fade) []byte {
	out := make([]byte, 0, n*4)
	for i := 0; i < n; i++ {
		env := 1.0
		switch fade {
		case FadeIn:
			env = float64(i) / float64(n)
		case FadeOut:
			env = float64(n-1-i) / float64(n)
		}
		v := int16(math.Round(s.amp * env * math.Sin(2*math.Pi*float64(s.freq)*float64(s.pos)/sampleRate)))
		s.pos++
		l, r := int16(0), int16(0)
		if s.ear == Left {
			l = v
		} else {
			r = v
		}
		out = append(out, byte(l), byte(uint16(l)>>8), byte(r), byte(uint16(r)>>8))
	}
	return out
}
