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

// resultFile writes a `hearing run --out` style file (genuine answers from the
// staircase engine, pattern n,n,h,...) and returns its path.
func resultFile(t *testing.T, mut func(m map[string]interface{})) string {
	t.Helper()
	return resultFileFrom(t, func(i int) bool { return i%3 == 2 }, mut)
}

func resultFileFrom(t *testing.T, heard func(i int) bool, mut func(m map[string]interface{})) string {
	t.Helper()
	sess := hearing.NewSession(hearing.DefaultSoundDB)
	var answers []heardRec
	for i := 0; !sess.Done(); i++ {
		h := heard(i)
		answers = append(answers, heardRec{Tone: sess.Current(), Heard: h})
		if err := sess.Answer(h); err != nil {
			t.Fatal(err)
		}
	}
	l, r := sess.Result()
	fl, fr := sess.Floor()
	res := computeResult(toF(l), toF(r))
	m := map[string]interface{}{
		"timestamp": "2026-10-13T14:00:00Z", "sink": "bluez_output.AA_BB_CC_DD_EE_FF.1", "sinkVolume": 90,
		"soundDb": hearing.DefaultSoundDB[:], "answers": answers,
		"left": l[:], "right": r[:],
		"floor":  map[string]interface{}{"left": fl[:], "right": fr[:]},
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
		return resultFileFrom(t, func(int) bool { return true }, nil) // always heard: all floor
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

// The floor flags and the gains of the file are not trusted: the answers are
// replayed through the engine.
func TestHearingApplyReplaysTheAnswers(t *testing.T) {
	allHeard := func(int) bool { return true }
	cases := map[string]string{
		"floor flags cleared": resultFileFrom(t, allHeard, func(m map[string]interface{}) {
			z := make([]bool, hearing.Bands)
			m["floor"] = map[string]interface{}{"left": z, "right": z}
		}),
		"a gain at -8 without the flag": resultFileFrom(t, allHeard, func(m map[string]interface{}) {
			z := make([]bool, hearing.Bands)
			m["left"] = []int{-8, -8, -8, -8, -8, -8, -8, -8}
			m["floor"] = map[string]interface{}{"left": z, "right": z}
		}),
		"gains edited against the answers": resultFile(t, func(m map[string]interface{}) {
			g := m["left"].([]int)
			g[0] = g[0] - 1
			m["left"] = g
			m["result"] = map[string]interface{}{"masterGain": -10.0}
		}),
		"an answer flipped": resultFile(t, func(m map[string]interface{}) {
			a := m["answers"].([]heardRec)
			a[3].Heard = !a[3].Heard
		}),
		"answers truncated": resultFile(t, func(m map[string]interface{}) {
			a := m["answers"].([]heardRec)
			m["answers"] = a[:len(a)-3]
		}),
		"extra answers": resultFile(t, func(m map[string]interface{}) {
			a := m["answers"].([]heardRec)
			m["answers"] = append(a, a[0])
		}),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			err := hearingApplyFile(r.env, p, hearingApplyOpts{yes: true, acceptFloor: true})
			if err == nil || r.link.list() != "" || r.link.statusCalls != 0 || len(r.saved) != 0 {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	// a genuine file with --accept-floor still goes through
	r := newRig(t, nil)
	if err := hearingApplyFile(r.env, resultFileFrom(t, allHeard, nil), hearingApplyOpts{yes: true, acceptFloor: true}); err != nil {
		t.Fatal(err)
	}
}

func TestHearingApplyFileLimits(t *testing.T) {
	r := newRig(t, nil)
	dir := t.TempDir()
	big := filepath.Join(dir, "big.json")
	os.WriteFile(big, bytes.Repeat([]byte(" "), maxResultFile+10), 0o600)
	if err := hearingApplyFile(r.env, big, hearingApplyOpts{yes: true}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("%v", err)
	}
	real := resultFile(t, nil)
	link := filepath.Join(dir, "link.json")
	os.Symlink(real, link)
	if err := hearingApplyFile(r.env, link, hearingApplyOpts{yes: true}); err == nil {
		t.Fatal("a symlink was followed")
	}
	if err := hearingApplyFile(r.env, dir, hearingApplyOpts{yes: true}); err == nil {
		t.Fatal("a directory was read")
	}
	if err := hearingApplyFile(r.env, "/dev/zero", hearingApplyOpts{yes: true}); err == nil {
		t.Fatal("a device file was read")
	}
	many := resultFile(t, func(m map[string]interface{}) {
		a := m["answers"].([]heardRec)
		for len(a) <= maxResultAnswers {
			a = append(a, a[0])
		}
		m["answers"] = a
	})
	if err := hearingApplyFile(r.env, many, hearingApplyOpts{yes: true}); err == nil || !strings.Contains(err.Error(), "answers") {
		t.Fatalf("%v", err)
	}
	if r.link.list() != "" || len(r.saved) != 0 {
		t.Fatal("the daemon was touched")
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
	if strings.Join(r.order, ",") != "slot11,save,record" {
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
	// the "experiencia-auditiva" entry wins over any other slot 11 copy
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

func TestSlot11BackupPrefersTheOldestDeviceReadAndSkipsOwnBackups(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sandboxCurve(t, "velho", "Velho", qcycore.SourceDeviceImport, 11, "2026-09-01T10:00:00Z")
	sandboxCurve(t, "novo", "Novo", qcycore.SourceDeviceImport, 11, "2026-10-10T10:00:00Z")
	sandboxCurve(t, "meu", "Meu", qcycore.SourceUser, 11, "2026-08-01T10:00:00Z")
	sandboxCurve(t, "slot11-backup-20260801-000000", "Backup antigo", qcycore.SourceUser, 11, "2026-07-01T10:00:00Z")
	b, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 255, Payload: slot11Payload}, time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := qcycore.LoadCurve(b.ID)
	if !strings.Contains(c.Note, "2026-09-01T10:00:00Z") || !strings.Contains(c.Note, "(entrada velho)") ||
		!strings.Contains(c.Note, "antes de gravar um novo resultado") {
		t.Fatalf("note: %q", c.Note)
	}
	// Only our own backups in the library: not a source.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sandboxCurve(t, "slot11-backup-20260801-000000", "Backup antigo", qcycore.SourceUser, 11, "2026-07-01T10:00:00Z")
	if _, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 255}, time.Now()); !errors.Is(err, errNoSlot11Copy) {
		t.Fatalf("%v", err)
	}
}

// An unreadable library entry may hide the copy: with no usable copy that is
// an error the waiver does not cover; with a usable copy it is a warning.
func TestSlot11BackupLibraryReadErrors(t *testing.T) {
	brokenSibling := func(t *testing.T, id string) {
		var dir string
		filepath.WalkDir(os.Getenv("XDG_DATA_HOME"), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && filepath.Base(p) == id+".json" {
				dir = filepath.Dir(p)
			}
			return nil
		})
		if dir == "" {
			t.Fatal("library directory not found")
		}
		os.WriteFile(filepath.Join(dir, "quebrada.json"), []byte("{not json"), 0o600)
	}
	// no usable copy + a failing entry: not errNoSlot11Copy
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sandboxCurve(t, "slot0", "Slot 0", qcycore.SourceDeviceImport, 0, "2026-10-12T10:00:00Z")
	brokenSibling(t, "slot0")
	_, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 255}, time.Now())
	var le *slot11LibraryError
	if !errors.As(err, &le) || errors.Is(err, errNoSlot11Copy) || !strings.Contains(err.Error(), "quebrada.json") {
		t.Fatalf("%v", err)
	}
	// ... and the waiver does not cover it (through hearingTakeBackup)
	r := newRig(t, nil)
	r.env.slot11 = func(*qcycore.EqSnapshot) (slot11Backup, error) {
		return hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 255}, time.Now()) // slot 11 is not the active one
	}
	err = hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, noSlot11Backup: true})
	if err == nil || !strings.Contains(err.Error(), "quebrada.json") || r.link.list() != "" || len(r.saved) != 0 {
		t.Fatalf("%v %s", err, r.link.list())
	}
	// usable copy + a failing entry: a warning
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sandboxCurve(t, "experiencia-auditiva", "Experiência auditiva", qcycore.SourceDeviceImport, 11, "2026-10-01T10:00:00Z")
	brokenSibling(t, "experiencia-auditiva")
	b, err := hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 255}, time.Now())
	if err != nil || len(b.Warnings) != 1 || !strings.Contains(b.Warnings[0], "quebrada.json") {
		t.Fatalf("%v %+v", err, b)
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

// ---------- the hearing-test card (ears) ----------

func TestHearingApplyRecordsTheResultAsTheHearingTestCard(t *testing.T) {
	r := newRig(t, nil)
	p := resultFile(t, nil)
	if err := hearingApplyFile(r.env, p, hearingApplyOpts{yes: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.order, ",") != "slot11,save,record" {
		t.Fatalf("order %v (the backups come first, the card is updated after the writes)", r.order)
	}
	if len(r.recorded) != 1 {
		t.Fatal("not recorded")
	}
	c := r.recorded[0]
	l, rr, _, _, err := loadHearingResult(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "experiencia-auditiva" || c.Name != "Experiência auditiva" || c.Source != qcycore.SourceDeviceImport || c.Slot != 11 || c.Ears == nil {
		t.Fatalf("%+v", c)
	}
	for i := 0; i < hearing.Bands; i++ {
		el, er := c.Ears.Left[i], c.Ears.Right[i]
		if el.Freq != hearing.Freqs[i] || int(el.Gain) != l[i] || int(er.Gain) != rr[i] || el.Q != 70 || el.Type != 2 {
			t.Fatalf("band %d: %+v %+v", i, el, er)
		}
		if want := float64(l[i]+rr[i]) / 2; c.Bands[i].Gain != want {
			t.Fatalf("band %d mean %v want %v", i, c.Bands[i].Gain, want)
		}
	}
	if !strings.Contains(c.Note, "Resultado do teste auditivo") || !strings.Contains(c.Note, "90%") || !strings.Contains(c.Note, "0x46/0x47") {
		t.Fatalf("note %q", c.Note)
	}
	if err := qcycore.ValidateCurve(c); err != nil {
		t.Fatal(err)
	}
}

func TestHearingApplyDoesNotRecordWhenNotConfirmedOrSendFails(t *testing.T) {
	r := newRig(t, nil)
	r.link.badReadback = true
	if err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true}); err == nil {
		t.Fatal("expected NOT CONFIRMED")
	}
	if len(r.recorded) != 0 || strings.Contains(strings.Join(r.order, ","), "record") {
		t.Fatalf("recorded after NOT CONFIRMED: %v", r.order)
	}
	if !strings.Contains(r.out.String(), "NOT updated") {
		t.Fatal(r.out.String())
	}
	r2 := newRig(t, nil)
	r2.link.failWriteOn = 2
	if err := hearingApplyFile(r2.env, resultFile(t, nil), hearingApplyOpts{yes: true}); err == nil || len(r2.recorded) != 0 {
		t.Fatalf("%v %d", err, len(r2.recorded))
	}
}

// The earbuds are written; if the library cannot be updated afterwards that is
// said loudly, nothing is undone and the run does not report a failed write.
func TestHearingApplyLibraryFailureAfterTheWriteWarnsWithoutUndoing(t *testing.T) {
	r := newRig(t, nil)
	r.recordErr = errors.New("disk full")
	err := hearingApplyFile(r.env, resultFile(t, nil), hearingApplyOpts{yes: true, deviceMode: true})
	if err != nil {
		t.Fatalf("the write succeeded: %v", err)
	}
	out := r.out.String()
	if !strings.Contains(out, "WARNING: the earbuds were written, but the library entry") || !strings.Contains(out, "NOT undone") ||
		!strings.Contains(out, "qcyctl library apply backup-antes-do-teste-auditivo-") {
		t.Fatal(out)
	}
	if ev := r.link.list(); ev != "mode:on,write:left,write:right,mode:off" {
		t.Fatalf("events %s", ev)
	}
}

func earsCard(t *testing.T, mut func(c *qcycore.Curve)) qcycore.Curve {
	t.Helper()
	l := [hearing.Bands]int{1, 0, -2, -3, 0, 1, 0, 2}
	r := [hearing.Bands]int{0, 0, -1, -2, 0, 0, 1, 1}
	c, err := hearingResultCurve(l, r, -10, time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC), 90, "2026-10-13T13:00:00Z", "confirmado pelo eco do aparelho")
	if err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(&c)
	}
	return c
}

// Applying the card sends 0x46/0x47 index 11 and nothing else: no 0x22, no
// slot 255, no 0x45, and the backups come first.
func TestLibraryApplyEarsCardSendsHearingFramesOnly(t *testing.T) {
	r := newRig(t, nil)
	c := earsCard(t, nil)
	if err := hearingApplyEars(r.env, c, false, false); err != nil {
		t.Fatal(err)
	}
	if ev := r.link.list(); ev != "write:left,write:right" {
		t.Fatalf("events %s", ev)
	}
	if strings.Join(r.order, ",") != "slot11,save" || len(r.recorded) != 0 {
		t.Fatalf("order %v recorded %d", r.order, len(r.recorded))
	}
	if r.ex.count("pactl") != 0 {
		t.Fatal("audio touched")
	}
}

func TestLibraryApplyEarsCardRefusesFractionsAndUnverified(t *testing.T) {
	r := newRig(t, nil)
	c := earsCard(t, func(c *qcycore.Curve) { c.Ears.Left[2].Gain = 1.5 })
	if err := hearingApplyEars(r.env, c, false, false); err == nil || r.link.list() != "" || len(r.saved) != 0 {
		t.Fatalf("%v", err)
	}
	r2 := newRig(t, nil)
	r2.link.verified = false
	if err := hearingApplyEars(r2.env, earsCard(t, nil), false, false); err == nil || r2.link.list() != "" || len(r2.saved) != 0 {
		t.Fatalf("%v", err)
	}
}

// ---------- through cmdLibrary: which path an entry takes ----------

func TestLibraryApplyEntryWithEarsNeverTakesTheSlot255Path(t *testing.T) {
	libEnv(t)
	l, opened := useFakeLink(t, hexBytes(t, slot0Flat))
	if err := qcycore.CreateCurve(earsCard(t, nil)); err != nil {
		t.Fatal(err)
	}
	err := cmdLibrary([]string{"apply", "experiencia-auditiva", "--yes"})
	// the hearing path needs the daemon, which the test sandbox refuses
	if err == nil || !strings.Contains(err.Error(), errHardwareDisabled.Error()) {
		t.Fatalf("%v", err)
	}
	if *opened != 0 || len(l.writes) != 0 {
		t.Fatalf("the 0x22 path was used: opened %d writes %d", *opened, len(l.writes))
	}
	if err := cmdLibrary([]string{"apply", "experiencia-auditiva", "--yes", "--slot", "0"}); err == nil || !strings.Contains(err.Error(), "0x46/0x47") {
		t.Fatalf("--slot accepted: %v", err)
	}
}

func TestLibraryApplyLegacyEntryKeepsTheSlot255Path(t *testing.T) {
	libEnv(t)
	l, opened := useFakeLink(t, hexBytes(t, slot0Flat))
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, "Experiência auditiva", realCustom11); err != nil {
		t.Fatal(err)
	}
	if err := cmdLibrary([]string{"apply", "experiencia-auditiva", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if *opened != 1 || len(l.writes) != 1 {
		t.Fatalf("legacy path: opened %d writes %d", *opened, len(l.writes))
	}
}

// ---------- the library: schema, upsert, trash ----------

func TestCurveWithoutEarsStillLoadsAndEarsAreValidated(t *testing.T) {
	libEnv(t)
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, "Experiência auditiva", realCustom11); err != nil {
		t.Fatal(err)
	}
	c, err := qcycore.LoadCurve("experiencia-auditiva")
	if err != nil || c.Ears != nil {
		t.Fatalf("%v %+v", err, c.Ears)
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), `"ears"`) {
		t.Fatal("ears must be omitted when absent")
	}
	bad := map[string]func(c *qcycore.Curve){
		"7 bands left":  func(c *qcycore.Curve) { c.Ears.Left = c.Ears.Left[:7] },
		"9 bands right": func(c *qcycore.Curve) { c.Ears.Right = append(c.Ears.Right, c.Ears.Right[0]) },
		"bad type":      func(c *qcycore.Curve) { c.Ears.Left[0].Type = 1 },
		"gain range":    func(c *qcycore.Curve) { c.Ears.Right[3].Gain = 99 },
		"freq range":    func(c *qcycore.Curve) { c.Ears.Left[1].Freq = 5 },
		"order":         func(c *qcycore.Curve) { c.Ears.Left[2].Index = 0 },
		"master":        func(c *qcycore.Curve) { c.Ears.MasterGain = 99 },
	}
	for name, mut := range bad {
		cc := earsCard(t, mut)
		cc.ID = "x-" + strings.ReplaceAll(name, " ", "-")
		if err := qcycore.ValidateCurve(cc); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestUpsertIsAtomicAndKeepsThePreviousVersionInTheTrash(t *testing.T) {
	libEnv(t)
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, "Experiência auditiva", realCustom11); err != nil {
		t.Fatal(err)
	}
	old, _ := qcycore.LoadCurve("experiencia-auditiva")
	replaced, err := qcycore.UpsertCurve(earsCard(t, nil))
	if err != nil || !replaced {
		t.Fatalf("%v %v", replaced, err)
	}
	cur, err := qcycore.LoadCurve("experiencia-auditiva")
	if err != nil || cur.Ears == nil || cur.Source != qcycore.SourceDeviceImport || cur.Slot != 11 {
		t.Fatalf("%v %+v", err, cur)
	}
	var trashed []string
	filepath.WalkDir(os.Getenv("XDG_DATA_HOME"), func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() && strings.Contains(p, ".trash") && strings.HasPrefix(d.Name(), "experiencia-auditiva-") {
			trashed = append(trashed, p)
		}
		return nil
	})
	if len(trashed) != 1 {
		t.Fatalf("trash: %v", trashed)
	}
	b, _ := os.ReadFile(trashed[0])
	var prev qcycore.Curve
	if err := json.Unmarshal(b, &prev); err != nil || prev.Payload != old.Payload || prev.Ears != nil {
		t.Fatalf("the trashed file is not the previous version: %v", err)
	}
	// first creation: nothing to trash
	libEnv(t)
	if replaced, err := qcycore.UpsertCurve(earsCard(t, nil)); err != nil || replaced {
		t.Fatalf("%v %v", replaced, err)
	}
}

// The backup of slot 11 is cloned from the version that is about to be
// replaced, and the card holds the new result afterwards.
func TestSlot11BackupCloneIsThePreviousVersionThenTheCardIsReplaced(t *testing.T) {
	libEnv(t)
	prev := earsCard(t, func(c *qcycore.Curve) { c.Note = "versao anterior" })
	prev.CreatedAt = "2026-09-01T10:00:00Z"
	if err := qcycore.CreateCurve(prev); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, nil)
	now := r.env.now()
	r.env.slot11 = func(*qcycore.EqSnapshot) (slot11Backup, error) {
		return hearingSlot11Backup(&qcycore.EqSnapshot{Slot: 255}, now)
	}
	r.env.recordCurve = qcycore.UpsertCurve
	// a different result than the previous card
	p := resultFileFrom(t, func(i int) bool { return i%2 == 0 }, nil)
	if err := hearingApplyFile(r.env, p, hearingApplyOpts{yes: true, acceptFloor: true}); err != nil {
		t.Fatal(err)
	}
	clones, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_DATA_HOME"), "*", "*", "slot11-backup-*.json"))
	if len(clones) == 0 {
		filepath.WalkDir(os.Getenv("XDG_DATA_HOME"), func(p string, d os.DirEntry, e error) error {
			if e == nil && strings.HasPrefix(d.Name(), "slot11-backup-") {
				clones = append(clones, p)
			}
			return nil
		})
	}
	if len(clones) != 1 {
		t.Fatalf("clones: %v", clones)
	}
	id := strings.TrimSuffix(filepath.Base(clones[0]), ".json")
	clone, err := qcycore.LoadCurve(id)
	if err != nil || clone.Ears == nil || clone.Source != qcycore.SourceUser || !strings.Contains(clone.Note, "experiencia-auditiva") {
		t.Fatalf("%v %+v", err, clone)
	}
	if clone.Payload != prev.Payload {
		t.Fatal("the clone is not the previous version")
	}
	cur, _ := qcycore.LoadCurve("experiencia-auditiva")
	if cur.Payload == prev.Payload || cur.Ears == nil || !strings.Contains(cur.Note, "Resultado do teste auditivo") {
		t.Fatalf("the card was not replaced: %+v", cur)
	}
}
