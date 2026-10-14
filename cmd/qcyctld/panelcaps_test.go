package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/qcycore"
	"github.com/bbztp/qcy-omarchy/internal/qcycore/models"
)

// Fixtures, all real vendorIds of the embedded snapshot:
//
//	23880  N70i      10-band EQ + hearing slot 11, type-10 keys, fit, beep 4/6/8/10, wear sensitivity
//	19794  C30       10-band EQ, type-10 keys, beep 6/10/12, no ANC
//	19786  Pro       10-band EQ, type-10 keys, packed ANC, wear, no beep/fit
//	16755  H3        10-band EQ whose effects carry no ids (list order is the id)
//	18863  T2C       5-band EQ
//	19777  ANC       legacy single-byte ANC values, 10-band EQ, type-10 keys

// eqVendorDialer is an eqDialer that also reports a vendorId and does not
// pose as the N70 by name.
type eqVendorDialer struct {
	sess   *eqSession
	vendor uint16
	mu     sync.Mutex
}

func (f *eqVendorDialer) Dial(onNotify func([]byte)) (session, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sess.notify = onNotify
	return f.sess, "QCY Earbuds", nil
}

func (f *eqVendorDialer) VendorID() (uint16, bool) { return f.vendor, true }

func eqVendorDaemon(t *testing.T, vendor uint16, state []byte, onWrite func(*eqSession, []byte)) (*daemon, *eqSession, string) {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	old := eqConfirmTimeout
	eqConfirmTimeout = 700 * time.Millisecond
	t.Cleanup(func() { eqConfirmTimeout = old })
	sess := &eqSession{state: append([]byte(nil), state...), onWrite: onWrite}
	cfg := defaultLifecycle()
	cfg.idle = time.Hour
	d := newDaemon(&eqVendorDialer{sess: sess, vendor: vendor}, cfg)
	mustStatus(t, d)
	settle(t, d)
	if h := d.knownHandler(); h == nil || h.Capabilities().Verified {
		t.Fatal("daemon is not on an untested model")
	}
	return d, sess, data + "/qcy-omarchy/backups"
}

func TestEQWriteOnAnUntestedDeclaredModelBacksUpAndConfirms(t *testing.T) {
	plain := mustHexBytes(t, realClassico6)
	plain[0] = 8 // an unprotected active slot with a blank tail
	d, sess, bdir := eqVendorDaemon(t, 23880, plain, applyWrite(false))
	res, err := eqApplyReq(d, map[string]interface{}{"masterGain": float64(-300)})
	if err != nil {
		t.Fatal(err)
	}
	if sess.writeCount() != 1 || backupCount(t, bdir) == 0 {
		t.Fatalf("writes %d backups %d", sess.writeCount(), backupCount(t, bdir))
	}
	r := res.(*qcycore.EqResult)
	if r.MasterGain != -3 || r.Previous != fmt.Sprintf("%x", plain) || r.BackupFirst == "" {
		t.Fatalf("result %+v", r)
	}
	// The panel's hearing slot is protected, and so is the classic curve.
	for _, slot := range []float64{11, 6} {
		_, err := eqApplyReq(d, map[string]interface{}{"slot": slot})
		if responseCode(err) != "protected_slot" {
			t.Errorf("slot %v: %v", slot, err)
		}
	}
	if sess.writeCount() != 1 {
		t.Fatal("a refused write reached the device")
	}
}

func TestEQSlotsAreTheOnesThePanelDeclares(t *testing.T) {
	plain := mustHexBytes(t, realClassico6)
	plain[0] = 8
	// 19786 has no hearing slot: 11 is not one of its slots, so it is refused
	// before any write (and is not "protected" there).
	d, sess, bdir := eqVendorDaemon(t, 19786, plain, applyWrite(false))
	_, err := eqApplyReq(d, map[string]interface{}{"slot": float64(11)})
	if err == nil || !strings.Contains(err.Error(), "not one of the slots") {
		t.Fatalf("slot 11 on 19786: %v", err)
	}
	// A declared slot works; 255 is always allowed.
	if _, err := eqApplyReq(d, map[string]interface{}{"slot": float64(3)}); err != nil {
		t.Fatalf("declared slot 3: %v", err)
	}
	if sess.writeCount() != 1 || backupCount(t, bdir) == 0 {
		t.Fatalf("writes %d", sess.writeCount())
	}

	// 16755: effects without ids take their list position + 1.
	c := GetHandler("qcy-16755").Capabilities()
	if c.EQ == nil || fmt.Sprint(c.EQ.Slots) != "[1 2 3 4 5 6]" || fmt.Sprint(c.EQ.ProtectedSlots) != "[6]" {
		t.Fatalf("16755 eq %+v", c.EQ)
	}
}

func TestEQNotOfferedForAFiveBandPanel(t *testing.T) {
	d, sess, _ := eqVendorDaemon(t, 18863, mustHexBytes(t, realClassico6), applyWrite(false))
	for _, cmd := range []string{"eqApply", "eqMasterGain"} {
		_, err := d.handleRequest(request{Cmd: cmd, Args: map[string]interface{}{"masterGain": float64(0)}})
		if err == nil || !strings.Contains(err.Error(), "does not declare") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	if sess.writeCount() != 0 {
		t.Fatal("write reached the device")
	}
	c := GetHandler("qcy-18863").Capabilities()
	if c.EQ != nil || !hasNote(c, "eq: 5-band") {
		t.Errorf("caps eq=%v notes=%v", c.EQ, c.Notes)
	}
}

func hasNote(c Capabilities, prefix string) bool {
	for _, n := range c.Notes {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func TestLegacyANCIsNotOfferedAndSaysWhy(t *testing.T) {
	c := GetHandler("qcy-19777").Capabilities()
	if declaresAction(c, "anc") || len(c.ANCModes) != 0 {
		t.Errorf("legacy ANC offered: %+v", c.ANCModes)
	}
	for _, f := range c.Features {
		if f == FeatureANC {
			t.Error("feature anc declared")
		}
	}
	if !hasNote(c, "anc: encoding not supported") {
		t.Errorf("notes %v", c.Notes)
	}
	// The rest of this panel is still offered.
	if len(c.Features) < 3 {
		t.Errorf("features %v", c.Features)
	}
	// A packed panel carries decoded parameters for the 0x17 write.
	n := GetHandler("qcy-19786").Capabilities()
	var nc *ANCModeSpec
	for i := range n.ANCModes {
		if strings.Contains(strings.ToLower(n.ANCModes[i].Name), "cancel") {
			nc = &n.ANCModes[i]
		}
	}
	if nc == nil || nc.Start != 65792 || fmt.Sprint(nc.Params) != "[1 1 2]" {
		t.Errorf("19786 ANC modes %+v", n.ANCModes)
	}
}

func TestPromptVolumeUsesThePanelsSteps(t *testing.T) {
	d, s := vendorScriptDaemon(t, 19794, func(s *scriptSession, op byte, p []byte) {
		if op == 0x1D && len(p) >= 1 {
			s.set(0x1D, []byte{p[0], 12})
		}
	})
	c := d.knownHandler().Capabilities()
	if fmt.Sprint(c.PromptVolumeSteps) != "[6 10 12]" || !declaresAction(c, "promptVolume") {
		t.Fatalf("caps steps %v", c.PromptVolumeSteps)
	}
	m := d.modelReport()
	if fmt.Sprint(m["promptVolumeSteps"]) != "[6 10 12]" {
		t.Fatalf("model.promptVolumeSteps = %v", m["promptVolumeSteps"])
	}
	// 4 is an N70 step, not one of this panel's.
	if _, err := d.handleRequest(setReq("promptVol", 4)); err == nil {
		t.Error("4 accepted on a panel that lists 6/10/12")
	}
	if len(framesOf(s, 0x1D)) != 0 {
		t.Fatal("refused value was written")
	}
	got, err := d.handleRequest(setReq("promptVol", 12))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := got.([]int); len(b) < 1 || b[0] != 12 {
		t.Fatalf("confirmed payload %v", got)
	}
	fr := framesOf(s, 0x1D)
	if len(fr) != 1 || !bytes.Equal(fr[0], []byte{0xFF, 0x04, 0x1D, 0x02, 0x0C, 0x00}) {
		t.Fatalf("frames %x", fr)
	}
}

func TestWearSensitivityOnAnUntestedDeclaredModel(t *testing.T) {
	d, s := vendorScriptDaemon(t, 23880, func(s *scriptSession, op byte, p []byte) {
		if op == 0x48 && len(p) >= 1 {
			s.set(0x48, []byte{p[0]})
		}
	})
	for _, v := range []byte{1, 3} {
		if _, err := d.handleRequest(setReq("wearSensitivity", v)); err != nil {
			t.Fatalf("%d: %v", v, err)
		}
	}
	if _, err := d.handleRequest(setReq("wearSensitivity", 4)); err == nil {
		t.Error("4 accepted")
	}
	fr := framesOf(s, 0x48)
	if len(fr) != 2 || !bytes.Equal(fr[1], []byte{0xFF, 0x03, 0x48, 0x01, 0x03}) {
		t.Fatalf("frames %x", fr)
	}
	if v, _ := mustStatus(t, d)["wearSensitivity"].(int); v != 3 {
		t.Errorf("status wearSensitivity = %v", v)
	}
	if m := d.modelReport(); len(m["wearSensitivityItems"].([]NamedValue)) != 3 {
		t.Errorf("model %v", m)
	}
}

func TestLdacIsWrittenOnADeclaringModelAndRefusedOnOneThatDoesNot(t *testing.T) {
	d, s := vendorScriptDaemon(t, 23880, func(s *scriptSession, op byte, p []byte) {
		if op == 0x23 && len(p) >= 1 {
			s.set(0x23, []byte{p[0]})
		}
	})
	if _, err := d.handleRequest(setReq("ldac", 1)); err != nil {
		t.Fatal(err)
	}
	if fr := framesOf(s, 0x23); len(fr) != 1 || !bytes.Equal(fr[0], []byte{0xFF, 0x03, 0x23, 0x01, 0x01}) {
		t.Fatalf("frames %x", fr)
	}
	// The generic fallback declares the shared set but no panel: ldac stays off.
	g, gs := unverifiedScriptDaemon(t, nil)
	if _, err := g.handleRequest(setReq("ldac", 1)); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("generic ldac: %v", err)
	}
	if len(framesOf(gs, 0x23)) != 0 {
		t.Fatal("written")
	}
}

func TestOffBytesComeFromThePanel(t *testing.T) {
	d, s := vendorScriptDaemon(t, 23880, func(s *scriptSession, op byte, p []byte) {
		if op == 0x2C || op == 0x2A {
			s.set(op, append([]byte(nil), p...))
		}
	})
	s.set(0x2C, []byte{1, 5, 6})
	if _, err := d.handleRequest(request{Cmd: "wear", Args: map[string]interface{}{"state": float64(2)}}); err != nil {
		t.Fatal(err)
	}
	if fr := framesOf(s, 0x2C); len(fr) != 1 || fr[0][4] != 0x00 {
		t.Fatalf("wear off frame %x", fr)
	}
	if _, err := d.handleRequest(setReq("wind", 2)); err != nil {
		t.Fatal(err)
	}
	if fr := framesOf(s, 0x2A); len(fr) != 1 || fr[0][4] != 0x00 {
		t.Fatalf("wind off frame %x", fr)
	}
}

// keysSession is a scripted link that has the key-remap characteristic.
type keysSession struct {
	*scriptSession
	mu   sync.Mutex
	keys []byte
}

func (k *keysSession) Has(c charKind) bool { return c == charKeys || k.scriptSession.Has(c) }
func (k *keysSession) Read(c charKind, buf []byte) (int, error) {
	if c == charKeys {
		k.mu.Lock()
		defer k.mu.Unlock()
		return copy(buf, k.keys), nil
	}
	return k.scriptSession.Read(c, buf)
}
func (k *keysSession) WriteKeys(raw []byte) error {
	k.mu.Lock()
	k.keys = append([]byte(nil), raw...)
	k.mu.Unlock()
	return nil
}

type keysDialer struct {
	sess   *keysSession
	vendor uint16
}

func (f keysDialer) Dial(onNotify func([]byte)) (session, string, error) {
	f.sess.scriptSession.mu.Lock()
	f.sess.scriptSession.notify = onNotify
	f.sess.scriptSession.mu.Unlock()
	return f.sess, "QCY Earbuds", nil
}
func (f keysDialer) VendorID() (uint16, bool) { return f.vendor, true }

func keysDaemon(t *testing.T, vendor uint16) (*daemon, *keysSession) {
	t.Helper()
	ks := &keysSession{scriptSession: &scriptSession{state: map[byte][]byte{}}, keys: []byte{1, 0, 2, 0}}
	cfg := defaultLifecycle()
	cfg.idle = time.Hour
	d := newDaemon(keysDialer{ks, vendor}, cfg)
	mustStatus(t, d)
	settle(t, d)
	return d, ks
}

func TestKeysRemapOnAnUntestedDeclaredModelChecksThePanelsActions(t *testing.T) {
	d, ks := keysDaemon(t, 19794)
	raw := func(b ...byte) request {
		l := make([]interface{}, len(b))
		for i, x := range b {
			l[i] = float64(x)
		}
		return request{Cmd: "keysSet", Args: map[string]interface{}{"raw": l}}
	}
	if _, err := d.handleRequest(raw(1, 2)); err != nil {
		t.Fatalf("listed pair: %v", err)
	}
	for _, bad := range [][]byte{{1, 99}, {0x40, 1}} {
		if _, err := d.handleRequest(raw(bad...)); err == nil || !strings.Contains(err.Error(), "not one the panel lists") {
			t.Errorf("%x: %v", bad, err)
		}
	}
	if !bytes.Equal(ks.keys, []byte{1, 2}) {
		t.Fatalf("keys %x", ks.keys)
	}
	// A model whose panel has only the legacy layout is refused outright.
	d2, _ := keysDaemon(t, 18863)
	if _, err := d2.handleRequest(raw(1, 2)); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Errorf("no type-10 keys: %v", err)
	}
}

// Capability derivation over every vendorId of the snapshot: it never panics
// and never declares a feature whose parameters are missing.
func TestCapabilityDerivationOverAllVendorIDs(t *testing.T) {
	var n, withEQ, withKeys, withANC, withPrompt, withWear, withFit int
	for _, p := range models.All() {
		v := p.VendorID
		t.Run(strconv.Itoa(int(v)), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			c := GetHandler(detectModel(v, true, "", "")).Capabilities()
			feats := map[FeatureFlag]bool{}
			for _, f := range c.Features {
				feats[f] = true
			}
			if feats[FeatureEQ] {
				e := c.EQ
				if e == nil || e.Bands != qcycore.EqBands || len(e.Freqs) != e.Bands || len(e.Slots) == 0 || e.MinDb >= 0 || e.MaxDb <= 0 {
					t.Errorf("eq declared with parameters %+v", e)
				}
				for _, ps := range e.ProtectedSlots {
					if !containsInt(e.Slots, ps) {
						t.Errorf("protected slot %d not among slots %v", ps, e.Slots)
					}
				}
			} else if c.EQ != nil {
				t.Errorf("eq parameters without the feature: %+v", c.EQ)
			}
			if feats[FeatureKeysRemap] && len(keyActionsFor(v)) == 0 {
				t.Error("keysRemap declared without key actions")
			}
			if feats[FeaturePromptVolume] != declaresAction(c, "promptVolume") {
				t.Error("promptVolume feature and action disagree")
			}
			if declaresAction(c, "promptVolume") && len(c.PromptVolumeSteps) == 0 {
				t.Error("promptVolume declared without steps")
			}
			if declaresAction(c, "wearSensitivity") && len(c.WearSensitivityItems) == 0 {
				t.Error("wearSensitivity declared without items")
			}
			if feats[FeatureANC] && c.ModelID != "melobuds-n70" && len(c.ANCModes) == 0 {
				t.Error("anc declared without modes")
			}
			for _, m := range c.ANCModes {
				if len(m.Params) != 3 || m.Start < 1<<16 {
					t.Errorf("anc mode %+v", m)
				}
			}
			if feats[FeatureFit] && p.Panel != nil && p.Panel.Fit == nil {
				t.Error("fit declared without a panel entry")
			}
			if c.ModelID == "generic" {
				return
			}
			n++
			if feats[FeatureEQ] {
				withEQ++
			}
			if feats[FeatureKeysRemap] {
				withKeys++
			}
			if len(c.ANCModes) > 0 {
				withANC++
			}
			if declaresAction(c, "promptVolume") {
				withPrompt++
			}
			if declaresAction(c, "wearSensitivity") {
				withWear++
			}
			if feats[FeatureFit] {
				withFit++
			}
		})
	}
	t.Logf("models %d: eq %d, keys %d, ancModes %d, promptVolume %d, wearSensitivity %d, fit %d",
		n, withEQ, withKeys, withANC, withPrompt, withWear, withFit)
}
