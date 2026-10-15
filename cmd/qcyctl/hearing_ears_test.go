package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/hearing"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

var (
	earsL = [hearing.Bands]int{1, -2, 3, 0, -4, 2, 5, -1}
	earsR = [hearing.Bands]int{2, 1, -3, 4, 0, -2, 1, 6}
)

const earsMaster = -3.0

// slot11State is a slot-11 payload as the earbuds report it: 5 factory-prefix
// bands then the eight bands of the left ear.
func slot11State(t *testing.T, left [hearing.Bands]int, master float64) []byte {
	t.Helper()
	var specs []qcycore.BandSpec
	for i, b := range hearing.FactoryBaseline {
		specs = append(specs, qcycore.BandSpec{Index: i, Freq: b.Freq, Gain: 0, Q: b.Q, Type: b.Type})
	}
	for i, g := range left {
		specs = append(specs, qcycore.BandSpec{Index: len(hearing.FactoryBaseline) + i, Freq: hearing.Freqs[i], Gain: float64(g), Q: 70, Type: 2})
	}
	slot := qcycore.HearingTestSlot
	mc := int(master * 100)
	p, err := qcycore.ApplyEq(make([]byte, qcycore.EqLen), qcycore.EqOp{Slot: &slot, MasterCenti: &mc, Bands: specs, AllowProtected: true, AllowFullLength: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func earStatus(left, right [hearing.Bands]int, master float64) map[string]interface{} {
	return map[string]interface{}{"eq": map[string]interface{}{"preset": 11.0, "masterGain": master,
		"ears": map[string]interface{}{"left": earBandMaps(left), "right": earBandMaps(right)}}}
}

func importHearing(t *testing.T, extra ...string) (error, string) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		err = cmdLibrary(append([]string{"import-device", "Experiência auditiva", "--expect-slot", "11", "--json"}, extra...))
	})
	return err, out
}

func TestImportDeviceKeepsBothEarsInTheShapeOfATestResult(t *testing.T) {
	libEnv(t)
	l, _ := useFakeLink(t, slot11State(t, earsL, earsMaster))
	l.statusMap = earStatus(earsL, earsR, earsMaster)
	if err, out := importHearing(t, "--require-ears"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	got, err := qcycore.LoadCurve("experiencia-auditiva")
	if err != nil {
		t.Fatal(err)
	}
	want, err := hearingResultCurve(earsL, earsR, earsMaster, time.Now(), 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ears == nil || got.ID != want.ID || got.Name != want.Name || got.Source != want.Source || got.Slot != want.Slot ||
		got.Payload != want.Payload || got.MasterGain != want.MasterGain {
		t.Fatalf("shape differs:\n got %+v\nwant %+v", got, want)
	}
	gj, _ := json.Marshal(got.Ears)
	wj, _ := json.Marshal(want.Ears)
	bj, _ := json.Marshal(got.Bands)
	wb, _ := json.Marshal(want.Bands)
	if !bytes.Equal(gj, wj) || !bytes.Equal(bj, wb) {
		t.Fatalf("ears/bands differ:\n%s\n%s", gj, wj)
	}
	// What the card holds is what `library apply` writes: both ears.
	r := newRig(t, nil)
	r.link.earsReport = true
	if err := hearingApplyEars(r.env, got, false, false); err != nil {
		t.Fatal(err)
	}
	if r.link.wroteL != earsL || r.link.wroteR != earsR {
		t.Fatalf("wrote %v / %v", r.link.wroteL, r.link.wroteR)
	}
	if !strings.Contains(r.out.String(), "both ears confirmed by readback") {
		t.Fatal(r.out.String())
	}
}

func TestImportDeviceWithoutEars(t *testing.T) {
	libEnv(t)
	l, _ := useFakeLink(t, slot11State(t, earsL, earsMaster))
	// Older daemon or failed read: no ears in the status.
	l.statusMap = map[string]interface{}{"eq": map[string]interface{}{"preset": 11.0, "masterGain": earsMaster}}
	if err, _ := importHearing(t, "--require-ears"); err == nil {
		t.Fatal("--require-ears saved a card without ears")
	}
	if _, err := qcycore.LoadCurve("experiencia-auditiva"); err == nil {
		t.Fatal("a card was saved")
	}
	if err, out := importHearing(t); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if c, _ := qcycore.LoadCurve("experiencia-auditiva"); c.Ears != nil {
		t.Fatal("ears invented")
	}
}

func TestImportDeviceRefusesEarsThatAreNotThisStateOrNotATestResult(t *testing.T) {
	other := earsL
	other[3] += 2
	bad := earStatus(earsL, earsR, earsMaster)
	bad["eq"].(map[string]interface{})["ears"].(map[string]interface{})["right"].([]interface{})[0].(map[string]interface{})["q"] = 80
	frac := earStatus(earsL, earsR, earsMaster)
	frac["eq"].(map[string]interface{})["ears"].(map[string]interface{})["left"].([]interface{})[1].(map[string]interface{})["gain"] = 1.5
	for name, st := range map[string]map[string]interface{}{
		"left is not the 0x22 left": earStatus(other, earsR, earsMaster),
		"another master":            earStatus(earsL, earsR, earsMaster+1),
		"Q is not the test's":       bad,
		"fractional gain":           frac,
	} {
		t.Run(name, func(t *testing.T) {
			libEnv(t)
			l, _ := useFakeLink(t, slot11State(t, earsL, earsMaster))
			l.statusMap = st
			if err, _ := importHearing(t, "--require-ears"); err == nil {
				t.Fatal("imported")
			}
			if _, err := qcycore.LoadCurve("experiencia-auditiva"); err == nil {
				t.Fatal("a card was saved")
			}
		})
	}
}

func TestRequireEarsOnlyForTheHearingCard(t *testing.T) {
	libEnv(t)
	l, _ := useFakeLink(t, slot11State(t, earsL, earsMaster))
	l.statusMap = earStatus(earsL, earsR, earsMaster)
	err := cmdLibrary([]string{"import-device", "Outra", "--expect-slot", "11", "--require-ears"})
	if err == nil {
		t.Fatal("accepted")
	}
	if err := cmdLibrary([]string{"import-device", "Outra", "--expect-slot", "11"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := qcycore.LoadCurve("outra"); c.Ears != nil {
		t.Fatal("a copy under another name got ears")
	}
}

func matchesOut(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = cmdLibrary(append([]string{"matches"}, args...)) })
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		t.Fatalf("output %q", out)
	}
	return m
}

func TestMatchesHearingCardOnlyAgainstSlot11AndBothEars(t *testing.T) {
	libEnv(t)
	c, err := hearingResultCurve(earsL, earsR, earsMaster, time.Now(), 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := qcycore.CreateCurve(c); err != nil {
		t.Fatal(err)
	}
	dev, _ := qcycore.DecodeEq(slot11State(t, earsL, earsMaster))
	bj, _ := json.Marshal(dev.Bands)
	ears := func(l, r [hearing.Bands]int) string {
		b, _ := json.Marshal(map[string]any{"left": earBandMaps(l), "right": earBandMaps(r)})
		return string(b)
	}
	base := []string{"experiencia-auditiva", "--master", "-3.00", "--bands", string(bj)}
	with := func(extra ...string) map[string]any {
		return matchesOut(t, append(append([]string{}, base...), extra...)...)
	}
	if m := with("--slot", "11", "--ears", ears(earsL, earsR)); m["match"] != true {
		t.Fatalf("both ears equal: %v", m)
	}
	otherR := earsR
	otherR[2]++
	if m := with("--slot", "11", "--ears", ears(earsL, otherR)); m["match"] != false {
		t.Fatalf("right ear differs: %v", m)
	}
	if m := with("--slot", "255"); m["match"] != false {
		t.Fatalf("slot 255 report: %v", m)
	}
	if m := with("--slot", "255", "--ears", ears(earsL, earsR)); m["match"] != false {
		t.Fatalf("slot 255 report with ears: %v", m)
	}
	// Without --ears: the left ear only, as before.
	if m := with("--slot", "11"); m["match"] != true {
		t.Fatalf("left only: %v", m)
	}
	// --any-user never names an entry with ears, even a user's own copy.
	u := c
	u.ID, u.Name, u.Source = "minha-audicao", "Minha audição", qcycore.SourceUser
	if err := qcycore.CreateCurve(u); err != nil {
		t.Fatal(err)
	}
	plain := dev
	pj, _ := json.Marshal(plain.Bands)
	if m := matchesOut(t, "--any-user", "--master", "-3.00", "--bands", string(pj)); m["match"] != false {
		t.Fatalf("any-user named an entry with ears: %v", m)
	}
}

func TestStatusEarsRejectsAnythingInvalid(t *testing.T) {
	good := earStatus(earsL, earsR, earsMaster)
	if e, _ := statusEars(good); e == nil || len(e.Left) != hearing.Bands {
		t.Fatal("valid ears refused")
	}
	for name, st := range map[string]map[string]interface{}{
		"no eq":       {},
		"no ears":     {"eq": map[string]interface{}{"preset": 11.0}},
		"not slot 11": {"eq": map[string]interface{}{"preset": 255.0, "ears": good["eq"].(map[string]interface{})["ears"]}},
		"7 bands":     {"eq": map[string]interface{}{"preset": 11.0, "ears": map[string]interface{}{"left": earBandMaps(earsL)[:7], "right": earBandMaps(earsR)}}},
	} {
		if e, _ := statusEars(st); e != nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
