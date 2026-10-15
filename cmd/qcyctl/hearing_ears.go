package main

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/hearing"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// Both ears of the hearing experience as the earbuds report them (status
// eq.ears, filled by the daemon only while slot 11 is active): the import of the
// card and the readback of an apply are built on it.

// statusEars extracts eq.ears from a status map for the given master gain; nil
// when the status has none (older daemon, not slot 11, read failed) or what it
// has is not a valid pair of ears.
func statusEars(st map[string]interface{}) (*earsReport, float64) {
	eq, _ := st["eq"].(map[string]interface{})
	if eq == nil || intOf(eq["preset"]) != qcycore.HearingTestSlot {
		return nil, 0
	}
	master, _ := eq["masterGain"].(float64)
	return earsReportFrom(eq["ears"], master), master
}

// earBandsAre says whether the bands are the shape a hearing-test result
// writes: the test frequencies, Q 0.7 and the bell type, whole dB gains.
func earBandsAre(e *earsReport) error {
	for _, side := range []struct {
		name  string
		bands []qcycore.EqBand
	}{{"left", e.Left}, {"right", e.Right}} {
		for i, b := range side.bands {
			if b.Q != hearingBandQ || b.Type != hearingBandType {
				return fmt.Errorf("%s ear band %d is Q %d type %d, not the hearing-test Q %d type %d", side.name, i, b.Q, b.Type, hearingBandQ, hearingBandType)
			}
		}
	}
	_, _, err := earsGains(&qcycore.EarCurves{Left: e.Left, Right: e.Right})
	return err
}

// Q and type of the bands a hearing-test result writes (see hearingResultCurve).
const (
	hearingBandQ    = 70
	hearingBandType = 2
)

// deviceHearingCurve builds the library card of the hearing experience from the
// two ears the earbuds report, in the shape a card made by the test here has
// (hearingResultCurve), so `library apply` writes 0x46/0x47 from it. snap is the
// 0x22 read taken with it; its left ear must be the left curve of the ears.
func deviceHearingCurve(ears *earsReport, snap *qcycore.EqSnapshot, now time.Time) (qcycore.Curve, error) {
	if err := earBandsAre(ears); err != nil {
		return qcycore.Curve{}, fmt.Errorf("the ears the earbuds report are not a hearing-test result: %w", err)
	}
	l, r, err := earsGains(&qcycore.EarCurves{Left: ears.Left, Right: ears.Right})
	if err != nil {
		return qcycore.Curve{}, err
	}
	c, err := hearingResultCurve(l, r, snap.MasterGain, now, 0, "", "", "")
	if err != nil {
		return qcycore.Curve{}, err
	}
	c.Note = "Importado do aparelho: as duas orelhas do teste auditivo (slot 11) lidas pelo aparelho (0x46/0x47)."
	return c, nil
}

// leftEarIsInEQ checks that the left ear of ears is the one 0x22 reports (its
// last eight bands) with the same master gain, so the ears and the snapshot
// describe the same state.
func leftEarIsInEQ(ears *earsReport, snap *qcycore.EqSnapshot, master float64) bool {
	n := len(snap.Bands)
	if n < qcycore.EarBands || len(ears.Left) != qcycore.EarBands || math.Abs(snap.MasterGain-master) > 0.011 {
		return false
	}
	for i, w := range ears.Left {
		b := snap.Bands[n-qcycore.EarBands+i]
		if b.Freq != w.Freq || math.Abs(b.Gain-w.Gain) > 0.011 {
			return false
		}
	}
	return true
}

// importCurve builds the library entry import-device saves for snap. For the
// hearing-experience card made from slot 11 it carries both ears when the daemon
// reports them (status eq.ears); requireEars turns their absence into an error.
func importCurve(dc daemonAPI, snap *qcycore.EqSnapshot, source, name string, requireEars bool) (qcycore.Curve, error) {
	card := snap.Slot == qcycore.HearingTestSlot && qcycore.Slug(name) == qcycore.HearingTestCurveID() && source == qcycore.SourceDeviceImport
	if !card {
		if requireEars {
			return qcycore.Curve{}, errors.New("library import-device: --require-ears is only for the hearing-experience card imported from slot 11; nothing saved")
		}
		return qcycore.CurveFromPayload(source, snap.Slot, name, snap.Payload)
	}
	var ears *earsReport
	if st, err := dc.status(); err == nil {
		var master float64
		if ears, master = statusEars(st); ears != nil && !leftEarIsInEQ(ears, snap, master) {
			ears = nil // the status is from another moment than the 0x22 read
		}
	}
	if ears == nil {
		if requireEars {
			return qcycore.Curve{}, errors.New("library import-device: the earbuds did not report both ears (the right one is only readable while slot 11 is active and the daemon is current); nothing saved")
		}
		return qcycore.CurveFromPayload(source, snap.Slot, name, snap.Payload)
	}
	return deviceHearingCurve(ears, snap, time.Now())
}

// verifyEarsByStatus reads both ears back from the status and compares them with
// what was written. have is false when the status carries no ears (older daemon,
// read failed): the caller keeps the older, weaker check. Otherwise left/right
// say which ear shows the written curve.
func verifyEarsByStatus(link hearingLink, l, r [hearing.Bands]int) (left, right, have bool) {
	st, err := link.status()
	if err != nil || boolOf(st["stale"]) {
		return false, false, false // a snapshot from before the write proves nothing
	}
	ears, _ := statusEars(st)
	if ears == nil {
		return false, false, false
	}
	same := func(got []qcycore.EqBand, want [hearing.Bands]int) bool {
		if len(got) != hearing.Bands {
			return false
		}
		for i, g := range got {
			if g.Freq != hearing.Freqs[i] || math.Abs(g.Gain-float64(want[i])) > 0.011 {
				return false
			}
		}
		return true
	}
	return same(ears.Left, l), same(ears.Right, r), true
}
