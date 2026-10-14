package qcycore

import "testing"

func earsCurve(baseline string, left []float64, master float64) Curve {
	freqs := []int{63, 125, 250, 500, 1000, 2000, 4000, 8000}
	l := make([]EqBand, 8)
	for i := range l {
		l[i] = EqBand{Index: i, Freq: freqs[i], Gain: left[i], Q: 70, Type: 2}
	}
	return Curve{MasterGain: master, Bands: l, Ears: &EarCurves{Left: l, Right: l, MasterGain: master, Baseline: baseline}}
}

func devBands(prefix int, prefixGain float64, left []float64) []EqBand {
	pf := []int{50, 200, 900, 2800, 8000, 50, 200, 900, 2800, 8000}
	freqs := []int{63, 125, 250, 500, 1000, 2000, 4000, 8000}
	var out []EqBand
	for i := 0; i < prefix; i++ {
		out = append(out, EqBand{Index: i, Freq: pf[i], Gain: prefixGain, Q: 60, Type: 2})
	}
	for i, g := range left {
		out = append(out, EqBand{Index: prefix + i, Freq: freqs[i], Gain: g, Q: 70, Type: 2})
	}
	return out
}

func TestCurveMatchesDeviceEars(t *testing.T) {
	left := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	other := []float64{1, 2, 3, 4, 5, 6, 7, 9}
	cases := []struct {
		name string
		c    Curve
		m    float64
		dev  []EqBand
		want bool
	}{
		{"13 same", earsCurve("", left, -10), -10, devBands(5, 3, left), true},
		{"bare 8 same", earsCurve("", left, -10), -10, devBands(0, 0, left), true},
		{"13 other gains", earsCurve("", left, -10), -10, devBands(5, 3, other), false},
		{"master differs", earsCurve("", left, -10), -9, devBands(5, 3, left), false},
		{"18 same", earsCurve("", left, -10), -10, devBands(10, 0, left), true},
		{"zero 13 same", earsCurve("zero", left, -10), -10, devBands(5, 0, left), true},
		{"zero 18 other gains", earsCurve("zero", left, -10), -10, devBands(10, 0, other), false},
		{"fewer than 8", earsCurve("", left, -10), -10, devBands(0, 0, left)[1:], false},
	}
	for _, tc := range cases {
		if got := CurveMatchesDevice(tc.c, tc.m, tc.dev); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Measured readback of slot 11 after `library apply experiencia-auditiva` with
// Popular in Personalizado before it: a 6-band prefix left by that effect.
func TestCurveMatchesDeviceEarsMeasuredPrefix(t *testing.T) {
	rb := func(last4000 float64) []EqBand {
		pairs := [][2]float64{{200, -9}, {900, -1}, {2800, 2}, {8500, -7}, {7000, -1}, {15000, -2},
			{63, 3}, {125, -2}, {250, -4}, {500, -2}, {1000, 1}, {2000, -1}, {4000, last4000}, {8000, 1}}
		out := make([]EqBand, len(pairs))
		for i, p := range pairs {
			out[i] = EqBand{Index: i, Freq: int(p[0]), Gain: p[1], Q: 70, Type: 2}
		}
		return out
	}
	c := earsCurve("", []float64{3, -2, -4, -2, 1, -1, -1, 1}, -10)
	for i, f := range []int{63, 125, 250, 500, 1000, 2000, 4000, 8000} {
		c.Ears.Left[i].Freq = f
	}
	if !CurveMatchesDevice(c, -10, rb(-1)) {
		t.Error("measured readback must match")
	}
	if CurveMatchesDevice(c, -10, rb(-2)) {
		t.Error("one of the last 8 gains swapped must not match")
	}
}

func TestCurveMatchesDevicePlain(t *testing.T) {
	b := []EqBand{{Freq: 100, Gain: 1, Q: 100, Type: 2}, {Freq: 1000, Gain: -2, Q: 100, Type: 2}}
	c := Curve{MasterGain: -1, Bands: b}
	if !CurveMatchesDevice(c, -1.005, []EqBand{{Freq: 100, Gain: 1.005, Q: 100, Type: 2}, b[1]}) {
		t.Error("centi-dB rounding should match")
	}
	if CurveMatchesDevice(c, -1, []EqBand{b[0], {Freq: 1000, Gain: -2.5, Q: 100, Type: 2}}) {
		t.Error("gain differs")
	}
	if CurveMatchesDevice(c, -1, []EqBand{b[0], {Freq: 1000, Gain: -2, Q: 100, Type: 0}}) {
		t.Error("type differs")
	}
	if CurveMatchesDevice(c, -1, b[:1]) {
		t.Error("count differs")
	}
}
