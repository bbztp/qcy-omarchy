package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/hearing"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// resultFile writes a `hearing run --out` style file and returns its path.
func resultFile(t *testing.T, mut func(m map[string]interface{})) string {
	t.Helper()
	l := [hearing.Bands]int{0, 1, -2, -3, 0, 1, 0, 2}
	r := [hearing.Bands]int{1, 0, -1, -2, 0, 0, 1, 1}
	res := computeResult(toF(l), toF(r))
	z := make([]bool, hearing.Bands)
	m := map[string]interface{}{
		"timestamp": "2026-10-13T14:00:00Z", "sink": "bluez_output.AA_BB_CC_DD_EE_FF.1", "sinkVolume": 90,
		"answers": []interface{}{map[string]interface{}{"heard": true}},
		"left":    l[:], "right": r[:],
		"floor":  map[string]interface{}{"left": z, "right": z},
		"result": map[string]interface{}{"masterGain": res.MasterGain},
	}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	p := filepath.Join(t.TempDir(), "hearing.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHearingApplyBackupBeforeAnySendThenWritesBothEars(t *testing.T) {
	r := newRig(t, nil)
	backupOnDiskAtFirstSend := false
	// the first frame of any kind must find the backup already on disk.
	r.env.link = func() (hearingLink, error) {
		return &sendWatch{fakeLink: r.link, r: r, flag: &backupOnDiskAtFirstSend}, nil
	}
	if err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true}); err != nil {
		t.Fatal(err)
	}
	if !backupOnDiskAtFirstSend {
		t.Fatal("a frame was sent before the backup was on disk")
	}
	if ev := r.link.list(); ev != "mode:on,write:left,write:right,mode:off" {
		t.Fatalf("events %s", ev)
	}
	out := r.out.String()
	if !strings.Contains(out, "applied") || !strings.Contains(out, "qcyctl library apply backup-antes-do-teste-auditivo-") || len(r.saved) != 1 {
		t.Fatal(out)
	}
	if r.ex.count("pactl") != 0 || r.ex.count("playerctl") != 0 || r.pl.startedCount() != 0 {
		t.Fatalf("audio/music touched: %v", r.ex.calls)
	}
	if r.link.usedAfterClose || !r.link.closed {
		t.Fatal("mode off must precede the close")
	}
}

// sendWatch checks, at the first frame, that the backup file exists.
type sendWatch struct {
	*fakeLink
	r    *rig
	flag *bool
	seen bool
}

func (s *sendWatch) check() {
	if s.seen {
		return
	}
	s.seen = true
	ents, _ := os.ReadDir(filepath.Join(s.r.cfg, "backups"))
	*s.flag = len(ents) == 1 && len(s.r.saved) == 1
}
func (s *sendWatch) hearingMode(on bool) (*hearingCall, error) {
	s.check()
	return s.fakeLink.hearingMode(on)
}
func (s *sendWatch) hearingWrite(e hearing.Ear, g, o [hearing.Bands]int) (*hearingCall, error) {
	s.check()
	return s.fakeLink.hearingWrite(e, g, o)
}

func TestHearingApplyWithoutYesSendsNothing(t *testing.T) {
	r := newRig(t, nil)
	err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatal(err)
	}
	if r.link.list() != "" || r.link.statusCalls != 0 || len(r.saved) != 0 {
		t.Fatal("the daemon was touched")
	}
}

func TestHearingApplyFloorNeedsAcceptFloor(t *testing.T) {
	floorFile := func(t *testing.T) string {
		return resultFile(t, func(m map[string]interface{}) {
			f := make([]bool, hearing.Bands)
			f[2] = true
			m["floor"] = map[string]interface{}{"left": f, "right": make([]bool, hearing.Bands)}
		})
	}
	r := newRig(t, nil)
	err := hearingApplyFile(r.env, floorFile(t), hearingApplyOpts{yes: true})
	if err == nil || !strings.Contains(err.Error(), "--accept-floor") || r.link.list() != "" || len(r.saved) != 0 {
		t.Fatalf("%v %s", err, r.link.list())
	}
	r2 := newRig(t, nil)
	if err := hearingApplyFile(r2.env, floorFile(t), hearingApplyOpts{yes: true, acceptFloor: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r2.link.list(), "write:right") {
		t.Fatal(r2.link.list())
	}
}

func TestHearingApplyRejectsInvalidFiles(t *testing.T) {
	cases := map[string]func(m map[string]interface{}){
		"dry run (no sink/answers)": func(m map[string]interface{}) {
			delete(m, "sink")
			delete(m, "answers")
			m["steps"] = []interface{}{}
		},
		"no answers":     func(m map[string]interface{}) { m["answers"] = []interface{}{} },
		"7 gains":        func(m map[string]interface{}) { m["left"] = []int{0, 0, 0, 0, 0, 0, 0} },
		"gain too high":  func(m map[string]interface{}) { m["left"] = []int{0, 0, 0, 0, 0, 0, 0, 7} },
		"gain too low":   func(m map[string]interface{}) { m["right"] = []int{-9, 0, 0, 0, 0, 0, 0, 0} },
		"no floor flags": func(m map[string]interface{}) { delete(m, "floor") },
		"master differs": func(m map[string]interface{}) { m["result"] = map[string]interface{}{"masterGain": -3.0} },
		"no sink volume": func(m map[string]interface{}) { m["sinkVolume"] = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			err := hearingApplyFile(r.env, resultFile(t, mut), hearingApplyOpts{yes: true})
			if err == nil || r.link.list() != "" || len(r.saved) != 0 || r.link.statusCalls != 0 {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	r := newRig(t, nil)
	p := filepath.Join(t.TempDir(), "x.json")
	os.WriteFile(p, []byte("not json"), 0o600)
	if err := hearingApplyFile(r.env, p, hearingApplyOpts{yes: true}); err == nil {
		t.Fatal("garbage accepted")
	}
	if err := hearingApplyFile(r.env, filepath.Join(t.TempDir(), "missing.json"), hearingApplyOpts{yes: true}); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestHearingApplyUnverifiedModelRefused(t *testing.T) {
	r := newRig(t, nil)
	r.link.verified = false
	err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true})
	if err == nil || !strings.Contains(err.Error(), "verified") || r.link.list() != "" || len(r.saved) != 0 {
		t.Fatalf("%v %s", err, r.link.list())
	}
}

func TestHearingApplyBackupFailureTouchesNothing(t *testing.T) {
	for name, setup := range map[string]func(r *rig){
		"eqRead":  func(r *rig) { r.link.eqErr = errors.New("no eq") },
		"library": func(r *rig) { r.saveErr = errors.New("disk full") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			setup(r)
			err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true})
			if err == nil || !strings.Contains(err.Error(), "backup failed") || r.link.list() != "" {
				t.Fatalf("%v %s", err, r.link.list())
			}
		})
	}
}

func TestHearingApplyPartialFailureIsMixedStateWithRestore(t *testing.T) {
	r := newRig(t, nil)
	r.link.failWriteOn = 2
	err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true})
	out := r.out.String()
	if err == nil || !strings.Contains(out, "MIXED STATE") || !strings.Contains(out, "qcyctl library apply backup-antes-do-teste-auditivo-") {
		t.Fatalf("%v\n%s", err, out)
	}
	if ev := r.link.list(); ev != "mode:on,write:left,write:right,mode:off" {
		t.Fatalf("events %s", ev)
	}
	if r.link.usedAfterClose {
		t.Fatal("mode off hit the closed link")
	}
}

func TestHearingApplyNotConfirmedIsNotSuccess(t *testing.T) {
	r := newRig(t, nil)
	r.link.badReadback = true
	err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true})
	if err == nil || !strings.Contains(err.Error(), "NOT CONFIRMED") || !strings.Contains(r.out.String(), "NOT CONFIRMED") {
		t.Fatal(err)
	}
	if strings.Contains(r.out.String(), "applied.") {
		t.Fatal("declared success")
	}
}

func TestHearingApplyModeIsOffBeforeCloseOnEveryExit(t *testing.T) {
	check := func(t *testing.T, r *rig, wantErr bool, err error) {
		t.Helper()
		if (err != nil) != wantErr {
			t.Fatalf("err %v", err)
		}
		ev := r.link.list()
		if !strings.HasSuffix(ev, "mode:off") || r.link.usedAfterClose || !r.link.closed {
			t.Fatalf("events %s usedAfterClose=%v closed=%v", ev, r.link.usedAfterClose, r.link.closed)
		}
	}
	t.Run("normal", func(t *testing.T) {
		r := newRig(t, nil)
		check(t, r, false, hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true}))
	})
	t.Run("write error", func(t *testing.T) {
		r := newRig(t, nil)
		r.link.failWriteOn = 1
		check(t, r, true, hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true}))
	})
	t.Run("signal before the writes", func(t *testing.T) {
		r := newRig(t, nil)
		sigs := make(chan os.Signal, 1)
		r.env.sigs = sigs
		r.link.onMode = func(on bool) {
			if on {
				sigs <- syscall.SIGINT
				time.Sleep(20 * time.Millisecond) // let the watcher see it
			}
		}
		err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true})
		check(t, r, true, err)
		if strings.Contains(r.link.list(), "write:") {
			t.Fatal("wrote after the signal")
		}
	})
}

func TestHearingApplyNoDeviceModeWithoutFlag(t *testing.T) {
	r := newRig(t, nil)
	if err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true}); err != nil {
		t.Fatal(err)
	}
	if ev := r.link.list(); ev != "write:left,write:right" {
		t.Fatalf("events %s", ev)
	}
}

func TestHearingApplyCmdUsage(t *testing.T) {
	var b bytes.Buffer
	for _, args := range [][]string{{}, {"--yes"}, {"a.json", "b.json", "--yes"}} {
		if err := hearingMain(append([]string{"apply"}, args...), &b); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// a valid command line stops at the hardware guard in the test sandbox
	p := resultFile(t, nil)
	err := hearingMain([]string{"apply", p, "--yes"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), errHardwareDisabled.Error()) {
		t.Fatalf("reached outside the sandbox: %v", err)
	}
}

// ---------- slot 11 backup ----------

const slot11Payload = "0b50fb28006400370002c800d4fe410002840332004b0002f00afa006e0002401f38ff2c01023f0096005000027d00ceff500002fa006400500002f4016aff500002e8033200500002d007c800500002a00f9cff500002401f2c015a000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

func TestHearingApplySlot11BackupPrecedesTheOtherBackupAndAnyFrame(t *testing.T) {
	r := newRig(t, nil)
	r.link.snapPayload = slot11Payload
	if err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.order, ",") != "slot11,save" {
		t.Fatalf("order %v", r.order)
	}
	out := r.out.String()
	if !strings.Contains(out, "qcyctl library apply experiencia-auditiva-backup-2026-10-13-12-00 --yes") || !strings.Contains(out, "slot 11 backup") {
		t.Fatal(out)
	}
	b, _ := os.ReadFile(filepath.Join(r.cfg, "backups", "hearing-before-20261013-120000.json"))
	if !strings.Contains(string(b), `"slot11Backup"`) || !strings.Contains(string(b), `"slot11Restore"`) {
		t.Fatalf("slot 11 backup missing in the JSON: %s", b)
	}
}

func TestHearingApplyNoSlot11CopyRefusesUnlessWaived(t *testing.T) {
	r := newRig(t, nil)
	r.slot11Err = errNoSlot11Copy
	err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true})
	if err == nil || !strings.Contains(err.Error(), "--no-slot11-backup") || r.link.list() != "" || len(r.saved) != 0 {
		t.Fatalf("%v %s", err, r.link.list())
	}
	r2 := newRig(t, nil)
	r2.slot11Err = errNoSlot11Copy
	if err := hearingApplyFile(r2.env, resultFile(t, nil), hearingApplyOpts{yes: true, noSlot11Backup: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r2.out.String(), "WARNING: --no-slot11-backup") || !strings.Contains(r2.link.list(), "write:left") {
		t.Fatal(r2.out.String())
	}
}

// A real failure of the library write aborts before anything is sent, and the
// waiver does not cover it.
func TestHearingApplySlot11LibraryFailureAbortsEvenWithWaiver(t *testing.T) {
	r := newRig(t, nil)
	r.slot11Err = errors.New("library: disk full")
	err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true, noSlot11Backup: true})
	if err == nil || !strings.Contains(err.Error(), "slot 11 backup") || r.link.list() != "" || len(r.saved) != 0 {
		t.Fatalf("%v %s", err, r.link.list())
	}
}

func sandboxCurve(t *testing.T, id, name, source string, slot int, created string) {
	t.Helper()
	p, _ := hexDecode(slot11Payload)
	d, err := qcycore.DecodeEq(p)
	if err != nil {
		t.Fatal(err)
	}
	err = qcycore.CreateCurve(qcycore.Curve{Version: qcycore.CurveVersion, ID: id, Name: name, Source: source, Slot: slot,
		MasterGain: d.MasterGain, Payload: d.Payload, Bands: d.Bands, CreatedAt: created})
	if err != nil {
		t.Fatal(err)
	}
}

func hexDecode(s string) ([]byte, error) {
	b := make([]byte, len(s)/2)
	_, err := fmt.Sscanf(s, "%x", &b)
	if err != nil || len(b) != len(s)/2 {
		// Sscanf %x of a long string into a slice works on this Go; fall back.
		out := make([]byte, 0, len(s)/2)
		for i := 0; i+1 < len(s); i += 2 {
			var v byte
			fmt.Sscanf(s[i:i+2], "%02x", &v)
			out = append(out, v)
		}
		return out, nil
	}
	return b, err
}

func TestSlot11BackupFromTheActiveSlot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	now := time.Date(2026, 10, 13, 14, 5, 7, 0, time.UTC)
	b, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 11, Payload: slot11Payload}, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := qcycore.LoadCurve(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Live || c.Source != qcycore.SourceUser || c.Slot != 11 || c.Name != "Experiência auditiva (backup 2026-10-13 14:05)" ||
		!strings.Contains(c.Note, "Leitura direta do slot 11") || c.Payload != slot11Payload {
		t.Fatalf("%+v", c)
	}
	if ok, why := c.Applyable(); !ok {
		t.Fatal(why)
	}
	// Same second again: still a distinct entry.
	b2, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 11, Payload: slot11Payload}, now)
	if err != nil || b2.ID == b.ID {
		t.Fatalf("%v %s %s", err, b.ID, b2.ID)
	}
}

func TestSlot11BackupClonesTheLastKnownCopy(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sandboxCurve(t, "experiencia-auditiva", "Experiência auditiva", qcycore.SourceDeviceImport, 11, "2026-10-01T10:00:00Z")
	sandboxCurve(t, "outro-slot11", "Outro", qcycore.SourceDeviceImport, 11, "2026-10-10T10:00:00Z")
	sandboxCurve(t, "slot0", "Slot 0", qcycore.SourceDeviceImport, 0, "2026-10-12T10:00:00Z")
	now := time.Date(2026, 10, 13, 14, 5, 0, 0, time.UTC)
	// slot 6 is active: slot 11 cannot be read
	b, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 6, Payload: slot11Payload}, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := qcycore.LoadCurve(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Live || c.Source != qcycore.SourceUser || c.Name != "Experiência auditiva (backup 2026-10-13 14:05)" || c.Slot != 11 {
		t.Fatalf("%+v", c)
	}
	// the "experiencia-auditiva" entry wins over a more recent slot 11 copy
	if !strings.Contains(c.Note, "2026-10-01T10:00:00Z") || !strings.Contains(c.Note, "não permite ler o slot 11 enquanto inativo") {
		t.Fatalf("note: %q", c.Note)
	}
	if ok, why := c.Applyable(); !ok {
		t.Fatal(why)
	}
	// and the original is untouched
	o, err := qcycore.LoadCurve("experiencia-auditiva")
	if err != nil || o.Source != qcycore.SourceDeviceImport || o.Note != "" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestSlot11BackupUsesTheMostRecentWhenThereIsNoNamedEntry(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sandboxCurve(t, "velho", "Velho", qcycore.SourceDeviceImport, 11, "2026-09-01T10:00:00Z")
	sandboxCurve(t, "novo", "Novo", qcycore.SourceUser, 11, "2026-10-10T10:00:00Z")
	b, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 255, Payload: slot11Payload}, time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := qcycore.LoadCurve(b.ID)
	if !strings.Contains(c.Note, "2026-10-10T10:00:00Z") {
		t.Fatalf("note: %q", c.Note)
	}
}

func TestSlot11BackupWithNoCopyAndNoActiveSlotFails(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sandboxCurve(t, "slot0", "Slot 0", qcycore.SourceDeviceImport, 0, "2026-10-12T10:00:00Z")
	_, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 255, Payload: slot11Payload}, time.Now())
	if !errors.Is(err, errNoSlot11Copy) {
		t.Fatalf("%v", err)
	}
	// An empty library as well.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, err := hearingSlot11Backup(nil, time.Now()); !errors.Is(err, errNoSlot11Copy) {
		t.Fatalf("%v", err)
	}
}

func TestSlot11BackupLibraryWriteFailure(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(f, nil, 0o600)
	t.Setenv("XDG_DATA_HOME", f) // the library directory cannot be created
	_, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 11, Payload: slot11Payload}, time.Now())
	if err == nil || errors.Is(err, errNoSlot11Copy) {
		t.Fatalf("%v", err)
	}
}
