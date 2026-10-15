package qcycore

import "math"

const (
	matchTolCenti = 0.011 // payload resolution: centi-dB
	earBands      = 8
)

// CurveMatchesDevice reports whether the EQ the earbuds report (master gain
// and bands of the active slot) is the one library entry c describes.
//
// An entry without Ears is a plain copy of a payload: same band count, and per
// position the same freq, q and type with the gain within a centi-dB.
//
// An entry with Ears is a hearing-test result: the earbuds report the LEFT ear
// only, behind a prefix of whatever bands the previous effect left (its length
// and content vary: measured 5 after a factory effect, 6 after Popular in
// Personalizado). So only the LAST eight bands are compared with Ears.Left
// (freq and gain within a centi-dB) and the master gain with Ears.MasterGain;
// the prefix is ignored, any length >= 8 is fine.
func CurveMatchesDevice(c Curve, master float64, dev []EqBand) bool {
	if c.Ears == nil {
		if len(dev) == 0 || len(dev) != len(c.Bands) || math.Abs(c.MasterGain-master) > matchTolCenti {
			return false
		}
		for i, b := range dev {
			w := c.Bands[i]
			if b.Freq != w.Freq || b.Q != w.Q || b.Type != w.Type || math.Abs(b.Gain-w.Gain) > matchTolCenti {
				return false
			}
		}
		return true
	}
	e := c.Ears
	n := len(dev)
	if len(e.Left) != earBands || n < earBands || math.Abs(e.MasterGain-master) > matchTolCenti {
		return false
	}
	for i, w := range e.Left {
		b := dev[n-earBands+i]
		if b.Freq != w.Freq || math.Abs(b.Gain-w.Gain) > matchTolCenti {
			return false
		}
	}
	return true
}

// CurveMatchesDeviceEars is CurveMatchesDevice for an entry with Ears when the
// earbuds also reported the two ears (left, right: the eight bands of each, as
// read while slot 11 is active): both ears must equal the entry's, by
// frequency and gain within a centi-dB. An entry without Ears is compared as
// CurveMatchesDevice does.
func CurveMatchesDeviceEars(c Curve, master float64, dev, left, right []EqBand) bool {
	if !CurveMatchesDevice(c, master, dev) {
		return false
	}
	if c.Ears == nil {
		return true
	}
	return earsEqual(c.Ears.Left, left) && earsEqual(c.Ears.Right, right)
}

func earsEqual(want, got []EqBand) bool {
	if len(want) != earBands || len(got) != earBands {
		return false
	}
	for i, w := range want {
		if got[i].Freq != w.Freq || math.Abs(got[i].Gain-w.Gain) > matchTolCenti {
			return false
		}
	}
	return true
}
