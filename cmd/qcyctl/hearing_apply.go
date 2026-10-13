package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/hearing"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// The write path shared by `hearing run --apply --yes` and `hearing apply
// <file>`: backup first, then both ears, readback, mode off.

// ---------- slot 11 backup ----------

// errNoSlot11Copy: slot 11 is not the active slot (the earbuds only report the
// active one) and the library holds no earlier copy of it.
var errNoSlot11Copy = errors.New("no copy of the current slot 11 is available: it is not the active slot (the earbuds cannot read an inactive slot) and the library has no earlier copy; pass --no-slot11-backup to write anyway (the hearing-test curve in slot 11 is overwritten for good)")

// slot11Backup is the library entry that preserves the slot 11 curve.
type slot11Backup struct {
	ID   string
	Name string
	Note string
	Live bool // read from the active slot 11 now; false = clone of the last known copy
	// Warnings: library entries that could not be read although a usable copy
	// was found.
	Warnings []string
}

// slot11LibraryError: the library could not be read completely and no usable
// copy of slot 11 was found. It is NOT waived by --no-slot11-backup: a copy may
// exist in an entry that failed to load.
type slot11LibraryError struct{ Failed []string }

func (e *slot11LibraryError) Error() string {
	return "no usable copy of slot 11 was found and these library entries could not be read, so one may exist among them: " + strings.Join(e.Failed, "; ")
}

// slot11BackupPrefix marks the entries this tool creates: they are never the
// source of a later backup (after an apply, slot 11 holds the test result).
const slot11BackupPrefix = "slot11-backup-"

// slot11BackupName is the visible name of the backup card.
func slot11BackupName(now time.Time) string {
	return "Experiência auditiva (backup " + now.Format("2006-01-02 15:04") + ")"
}

// hearingSlot11Backup saves what slot 11 holds as a USER entry of the library
// ("source":"user" is what the plugin shows as a customized card, with the
// edit/delete menu):
//
//	a) slot 11 is the active slot: the raw active payload is saved;
//	b) otherwise the last known copy (id experiencia-auditiva, else the most
//	   recent entry with slot 11) is cloned, with a note saying so;
//	c) no copy at all: errNoSlot11Copy.
func hearingSlot11Backup(snap *qcycore.EqSnapshot, now time.Time) (slot11Backup, error) {
	// The id carries the time with seconds (Slug truncates long names, so the
	// readable name cannot be the id); a clash within one second gets a suffix.
	create := func(build func(name string) (qcycore.Curve, error)) (slot11Backup, error) {
		name := slot11BackupName(now)
		base := "slot11-backup-" + now.Format("20060102-150405")
		for n := 0; n < 5; n++ {
			c, err := build(name)
			if err != nil {
				return slot11Backup{}, err
			}
			c.ID = base
			if n > 0 {
				c.ID = fmt.Sprintf("%s-%d", base, n+1)
			}
			err = qcycore.CreateCurve(c)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			if err != nil {
				return slot11Backup{}, err
			}
			return slot11Backup{ID: c.ID, Name: c.Name, Note: c.Note}, nil
		}
		return slot11Backup{}, errors.New("the backup entry already exists")
	}
	stamp := now.UTC().Format(time.RFC3339)

	if snap != nil && snap.Slot == 11 {
		b, err := create(func(name string) (qcycore.Curve, error) {
			p, err := hex.DecodeString(strings.TrimSpace(snap.Payload))
			if err != nil {
				return qcycore.Curve{}, fmt.Errorf("slot 11 payload: %w", err)
			}
			if err := qcycore.ValidateEq(p); err != nil {
				return qcycore.Curve{}, err
			}
			d, err := qcycore.DecodeEq(p)
			if err != nil {
				return qcycore.Curve{}, err
			}
			return qcycore.Curve{
				Version: qcycore.CurveVersion, Name: name,
				Source: qcycore.SourceUser, Slot: 11, MasterGain: d.MasterGain, Payload: d.Payload, Bands: d.Bands,
				Note:      "Leitura direta do slot 11 (ativo) feita no aparelho em " + now.Format("2006-01-02 15:04") + ", antes de gravar o teste auditivo.",
				CreatedAt: stamp,
			}, nil
		})
		b.Live = true
		return b, err
	}

	curves, listErrs := qcycore.ListCurves()
	var failed []string
	for _, e := range listErrs {
		failed = append(failed, e.Error())
	}
	// The source is the OLDEST known device read of slot 11 (the entry
	// "experiencia-auditiva" first): once a test result has been written, slot
	// 11 holds that result and later copies would be stale. Backups made by
	// this tool are never sources.
	var src *qcycore.Curve
	rank := func(c *qcycore.Curve) int {
		switch {
		case c.ID == "experiencia-auditiva":
			return 0
		case c.Source == qcycore.SourceDeviceImport:
			return 1
		}
		return 2
	}
	for i := range curves {
		c := &curves[i]
		if c.Slot != 11 || c.Payload == "" || strings.HasPrefix(c.ID, slot11BackupPrefix) {
			continue
		}
		if ok, _ := c.Applyable(); !ok {
			continue
		}
		if src == nil || rank(c) < rank(src) || (rank(c) == rank(src) && c.CreatedAt < src.CreatedAt) {
			src = c
		}
	}
	if src == nil {
		if len(failed) > 0 {
			return slot11Backup{}, &slot11LibraryError{Failed: failed}
		}
		return slot11Backup{}, errNoSlot11Copy
	}
	clone := *src
	when := src.CreatedAt
	if when == "" {
		when = "data desconhecida"
	}
	b, err := create(func(name string) (qcycore.Curve, error) {
		c := clone
		c.Name, c.Source, c.CreatedAt = name, qcycore.SourceUser, stamp
		c.Note = "Última leitura do slot 11 feita no aparelho em " + when + " (entrada " + src.ID +
			"); o aparelho não permite ler o slot 11 enquanto inativo. Depois de gravar um resultado do teste auditivo o slot 11 contém esse resultado; esta é a leitura mais antiga conhecida."
		return c, nil
	})
	if err == nil && len(failed) > 0 {
		b.Warnings = append(b.Warnings, "some library entries could not be read (the copy used may not be the best one): "+strings.Join(failed, "; "))
	}
	return b, err
}

// ---------- backup ----------

// hearingBackupInfo is where the pre-write state went.
type hearingBackupInfo struct {
	Path, Restore                 string // JSON file; command that restores the active curve
	Slot11, Slot11Name, Slot11Cmd string // library entry preserving slot 11 ("" when waived)
	Slot11Live                    bool
	// Warnings are printed by the caller's goroutine (printBackup), never from
	// the backup goroutine.
	Warnings []string
}

// hearingTakeBackup reads the full status and the raw active EQ, preserves
// slot 11 as a library entry (the write overwrites it), stores the active
// curve in the library (the restore point) and writes the JSON backup. Any
// failure is returned and means nothing may be written. Everything here
// happens BEFORE the first frame or audible effect of the caller.
func hearingTakeBackup(env hearingEnv, link hearingLink, noSlot11 bool) (info hearingBackupInfo, err error) {
	st, err := link.status()
	if err != nil {
		return info, fmt.Errorf("status: %w", err)
	}
	snap, err := link.eqRead()
	if err != nil {
		return info, fmt.Errorf("cannot read the active EQ: %w", err)
	}
	cfg, err := env.configDir()
	if err != nil {
		return info, err
	}
	// Slot 11 first: it is the curve the write destroys.
	b, err := env.slot11(snap)
	switch {
	case err == nil:
		info.Slot11, info.Slot11Name, info.Slot11Live = b.ID, b.Name, b.Live
		info.Slot11Cmd = "qcyctl library apply " + b.ID + " --yes"
		info.Warnings = append(info.Warnings, b.Warnings...)
	case errors.Is(err, errNoSlot11Copy) && noSlot11:
		info.Warnings = append(info.Warnings, fmt.Sprintf("--no-slot11-backup: slot 11 (your hearing-test curve) will be overwritten and cannot be restored from this tool: %v", err))
	default:
		return info, fmt.Errorf("slot 11 backup: %w", err)
	}
	stamp := env.now().Format("20060102-150405")
	libID, err := env.saveCurve("backup antes do teste auditivo "+stamp, snap)
	if err != nil {
		return info, fmt.Errorf("library entry failed: %w", err)
	}
	dir := filepath.Join(cfg, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return info, err
	}
	info.Path = filepath.Join(dir, "hearing-before-"+stamp+".json")
	info.Restore = "qcyctl library apply " + libID + " --yes"
	if err := writeJSONFile(info.Path, hearingBackup{Timestamp: stamp, Status: st, Eq: snap, Library: libID, Restore: info.Restore,
		Slot11: info.Slot11, Slot11Restore: info.Slot11Cmd}); err != nil {
		return info, err
	}
	return info, nil
}

// printBackup tells where the backups are and how to restore them.
func printBackup(out io.Writer, in hearingBackupInfo) {
	for _, w := range in.Warnings {
		fmt.Fprintf(out, "WARNING: %s\n", w)
	}
	fmt.Fprintf(out, "backup: %s\nto restore the previous state: %s\n", in.Path, in.Restore)
	if in.Slot11 != "" {
		how := "clone of the last known copy"
		if in.Slot11Live {
			how = "read from the active slot 11"
		}
		fmt.Fprintf(out, "slot 11 backup: %s (%q, %s)\nto restore slot 11: %s\n", in.Slot11, in.Slot11Name, how, in.Slot11Cmd)
	}
}

// hearingCommit writes the left then the right curve (confirm:true is added by
// the link), reports each readback, switches the test mode off (modeOff may be
// nil), reads the device back and says "applied" or NOT CONFIRMED. The restore
// command is printed on every bad outcome.
func hearingCommit(out io.Writer, link hearingLink, l, r [hearing.Bands]int, restoreCmd string, modeOff func()) (writes []*hearingCall, final *qcycore.EqSnapshot, err error) {
	wrote := 0
	var unconfirmed []string
	for _, w := range []struct {
		ear         hearing.Ear
		gains, peer [hearing.Bands]int
	}{{hearing.Left, l, r}, {hearing.Right, r, l}} {
		c, e := link.hearingWrite(w.ear, w.gains, w.peer)
		if e != nil {
			state := "no ear was confirmed written"
			if wrote > 0 {
				state = "MIXED STATE: the left ear was written and the right was not"
			}
			fmt.Fprintf(out, "ERROR writing the %s ear: %v\n%s; to restore the previous state: %s\n", w.ear, e, state, restoreCmd)
			return writes, nil, fmt.Errorf("write %s ear: %w", w.ear, e)
		}
		wrote++
		writes = append(writes, c)
		fmt.Fprintf(out, "wrote %-5s sent %s  ack=%v", w.ear, c.Sent, c.Ack)
		if c.Readback != nil {
			fmt.Fprintf(out, "  device: slot %d %q masterGain %.2f dB", c.Readback.Slot, c.Readback.Effect, c.Readback.MasterGain)
		} else if c.ReadbackError != "" {
			fmt.Fprintf(out, "  readback failed: %s", c.ReadbackError)
		}
		fmt.Fprintln(out)
		if !writeConfirmed(c, w.gains) {
			unconfirmed = append(unconfirmed, w.ear.String())
			fmt.Fprintf(out, "NOT CONFIRMED: the earbuds sent no echo for the %s ear and the 0x22 readback does not show the written curve; to restore the previous state: %s\n", w.ear, restoreCmd)
		}
	}
	if modeOff != nil {
		modeOff()
	}
	if snap, e := link.eqRead(); e != nil {
		fmt.Fprintf(out, "warning: final read: %v\n", e)
	} else {
		final = snap
		fmt.Fprintf(out, "device now reports: slot %d %q masterGain %.2f dB, %d bands\n", snap.Slot, snap.Effect, snap.MasterGain, len(snap.Bands))
	}
	if len(unconfirmed) > 0 {
		return writes, final, fmt.Errorf("write NOT CONFIRMED for the %s ear: the result may not have been applied (restore: %s)", strings.Join(unconfirmed, " and "), restoreCmd)
	}
	fmt.Fprintf(out, "applied. to restore the previous state: %s\n", restoreCmd)
	return writes, final, nil
}

// ---------- hearing apply <file> ----------

// hearingResultFile is what `hearing run --out` writes, as far as apply needs.
type hearingResultFile struct {
	Timestamp  string            `json:"timestamp"`
	Sink       string            `json:"sink"`
	SinkVolume int               `json:"sinkVolume"`
	SoundDB    []float64         `json:"soundDb"`
	Answers    []heardRec        `json:"answers"`
	Left       []int             `json:"left"`
	Right      []int             `json:"right"`
	Floor      map[string][]bool `json:"floor"`
	Result     struct {
		MasterGain float64 `json:"masterGain"`
	} `json:"result"`
}

type hearingApplyOpts struct {
	yes, acceptFloor, deviceMode bool
	noSlot11Backup               bool
}

const (
	// maxResultFile bounds what `hearing apply` reads; a real result is ~100 KB.
	maxResultFile = 1 << 20
	// maxResultAnswers bounds the replay (a full test is ~100 answers).
	maxResultAnswers = 5000
)

// readResultFile reads a regular file of at most maxResultFile bytes without
// following a symlink at path.
func readResultFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxResultFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxResultFile {
		return nil, fmt.Errorf("%s is larger than %d bytes: not a hearing result", path, maxResultFile)
	}
	return b, nil
}

// loadHearingResult reads and validates a `hearing run --out` file. A dry-run
// file (no sink/answers/floor) or an incomplete one is rejected. The floor
// flags of the file are NOT trusted: the answers are replayed through the
// staircase engine and the gains and the flags it produces must be the ones
// stored; nFloor comes from the replay.
func loadHearingResult(path string) (l, r [hearing.Bands]int, f *hearingResultFile, nFloor int, err error) {
	b, err := readResultFile(path)
	if err != nil {
		return l, r, nil, 0, err
	}
	f = &hearingResultFile{}
	if err := json.Unmarshal(b, f); err != nil {
		return l, r, nil, 0, fmt.Errorf("%s: not a hearing result: %w", path, err)
	}
	bad := func(why string) error {
		return fmt.Errorf("%s: not a complete result of 'hearing run --out' (%s)", path, why)
	}
	if f.Sink == "" || f.SinkVolume <= 0 || len(f.Answers) == 0 {
		return l, r, nil, 0, bad("no sink/sinkVolume/answers: a dry run or an aborted test cannot be applied")
	}
	if len(f.Answers) > maxResultAnswers {
		return l, r, nil, 0, bad(fmt.Sprintf("%d answers (at most %d)", len(f.Answers), maxResultAnswers))
	}
	if len(f.Left) != hearing.Bands || len(f.Right) != hearing.Bands {
		return l, r, nil, 0, bad(fmt.Sprintf("need %d gains per ear, got %d and %d", hearing.Bands, len(f.Left), len(f.Right)))
	}
	fl, fr := f.Floor["left"], f.Floor["right"]
	if len(fl) != hearing.Bands || len(fr) != hearing.Bands {
		return l, r, nil, 0, bad("the floor flags are missing")
	}
	for i := 0; i < hearing.Bands; i++ {
		for _, g := range []int{f.Left[i], f.Right[i]} {
			if g < hearing.MinStep || g > hearing.MaxGain {
				return l, r, nil, 0, bad(fmt.Sprintf("gain %d at band %d is outside %d..+%d", g, i, hearing.MinStep, hearing.MaxGain))
			}
		}
		l[i], r[i] = f.Left[i], f.Right[i]
		if (f.Left[i] == hearing.MinStep && !fl[i]) || (f.Right[i] == hearing.MinStep && !fr[i]) {
			return l, r, nil, 0, bad(fmt.Sprintf("band %d sits at the staircase floor (%d) but is not flagged floor", i, hearing.MinStep))
		}
	}
	var gl, gr [hearing.Bands]float64
	for i := range l {
		gl[i], gr[i] = float64(l[i]), float64(r[i])
	}
	if d := computeResult(gl, gr).MasterGain - f.Result.MasterGain; d > 0.05 || d < -0.05 {
		return l, r, nil, 0, bad(fmt.Sprintf("the stored master gain %.2f dB does not match the gains (%.2f dB)", f.Result.MasterGain, computeResult(gl, gr).MasterGain))
	}

	// Replay the answers through the engine.
	if len(f.SoundDB) != hearing.Bands {
		return l, r, nil, 0, bad("soundDb is missing: the answers cannot be replayed")
	}
	var sdb [hearing.Bands]float64
	for i, v := range f.SoundDB {
		if v != v || v > 1e6 || v < -1e6 {
			return l, r, nil, 0, bad("soundDb has an invalid value")
		}
		sdb[i] = v
	}
	mod := fmt.Errorf("%s: the file was modified or produced by another version (its answers do not give its gains/floor flags)", path)
	sess := hearing.NewSession(sdb)
	for i, a := range f.Answers {
		if sess.Done() {
			return l, r, nil, 0, mod
		}
		t := sess.Current()
		if a.Tone.Ear != t.Ear || a.Tone.Band != t.Band || a.Tone.Gain != t.Gain {
			return l, r, nil, 0, fmt.Errorf("%w (answer %d)", mod, i+1)
		}
		if e := sess.Answer(a.Heard); e != nil {
			return l, r, nil, 0, mod
		}
	}
	if !sess.Done() {
		return l, r, nil, 0, bad("the answers do not complete the test")
	}
	rl, rr := sess.Result()
	rfl, rfr := sess.Floor()
	for i := 0; i < hearing.Bands; i++ {
		if rl[i] != l[i] || rr[i] != r[i] || rfl[i] != fl[i] || rfr[i] != fr[i] {
			return l, r, nil, 0, mod
		}
		if rfl[i] {
			nFloor++
		}
		if rfr[i] {
			nFloor++
		}
	}
	return l, r, f, nFloor, nil
}

// hearingApplyFile writes a stored hearing result to the earbuds: no audio, no
// pactl, no music. Backup before any effect; 0x45 only with --device-mode.
func hearingApplyFile(env hearingEnv, path string, o hearingApplyOpts) (err error) {
	out := env.out
	l, r, f, nFloor, err := loadHearingResult(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "result: %s  (test at sink volume %d%%, %s)\nleft : %v\nright: %v\n", path, f.SinkVolume, f.Timestamp, l, r)
	fmt.Fprintf(out, "master gain: %.2f dB\n", f.Result.MasterGain)
	if nFloor > 0 {
		printFloorWarning(out, nFloor)
	}
	if !o.yes {
		return errors.New("hearing apply: refused, pass --yes to write this to the earbuds; nothing was sent")
	}
	if nFloor > 0 && !o.acceptFloor {
		return fmt.Errorf("refused to write: %d band(s) were still heard at the lowest level, so the result is only relative; rerun the test with a lower --sink-volume, or pass --accept-floor; nothing was sent", nFloor)
	}

	var (
		abortOnce sync.Once
		abortCh   = make(chan struct{})
		watchDone = make(chan struct{})
	)
	go func() {
		select {
		case <-env.sigs:
			abortOnce.Do(func() { close(abortCh) })
		case <-watchDone:
		}
	}()
	defer close(watchDone)
	aborted := func() error {
		select {
		case <-abortCh:
			return errInterrupted
		default:
			return nil
		}
	}

	var undo []func()
	defer func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()
	fmt.Fprintln(out, "checking the earbuds…")
	link, err := env.link()
	if err != nil {
		return fmt.Errorf("hearing apply needs the qcyctld daemon: %w", err)
	}
	// Closed last, by the chain: the mode-off frame needs the open link.
	undo = append(undo, func() { _ = link.Close() })
	verified, err := link.capabilities()
	if err != nil {
		return fmt.Errorf("capabilities: %w", err)
	}
	if !verified {
		return errors.New("hearing apply is only for a verified model (this earbud is not)")
	}
	fmt.Fprintln(out, "reading the earbuds' state for the backup…")
	info, err := hearingTakeBackup(env, link, o.noSlot11Backup)
	if err != nil {
		return fmt.Errorf("backup failed, nothing was sent to the earbuds: %w", err)
	}
	restoreCmd := info.Restore
	printBackup(out, info)
	if e := aborted(); e != nil {
		return e
	}

	modeOn := false
	modeOff := func() {
		if !modeOn {
			return
		}
		modeOn = false
		if _, e := link.hearingMode(false); e != nil {
			fmt.Fprintf(out, "warning: could not switch the test mode off (%v); run: qcyctl hearing mode off --yes; to restore the previous state: %s\n", e, restoreCmd)
		}
	}
	undo = append(undo, modeOff)
	if o.deviceMode {
		modeOn = true
		if _, e := link.hearingMode(true); e != nil {
			return fmt.Errorf("hearing mode on: %w", e)
		}
	} else {
		fmt.Fprintln(out, "device test mode (0x45) not sent: pass --device-mode to enable it")
	}
	if e := aborted(); e != nil {
		return e
	}
	_, _, err = hearingCommit(out, link, l, r, restoreCmd, modeOff)
	return err
}

// hearingApplyCmd is `qcyctl hearing apply <file> --yes [--accept-floor]
// [--device-mode]`.
func hearingApplyCmd(args []string, out io.Writer) error {
	const usage = "usage: qcyctl hearing apply <result.json> --yes [--accept-floor] [--device-mode] [--no-slot11-backup]"
	fs := hearingFlags("apply")
	yes := fs.Bool("yes", false, "")
	accept := fs.Bool("accept-floor", false, "")
	dev := fs.Bool("device-mode", false, "")
	noS11 := fs.Bool("no-slot11-backup", false, "")
	var path string
	var rest []string
	for _, a := range args {
		if path == "" && !strings.HasPrefix(a, "-") {
			path = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || path == "" {
		return errors.New(usage)
	}
	env, done := liveHearingEnv(out)
	defer done()
	return hearingApplyFile(env, path, hearingApplyOpts{yes: *yes, acceptFloor: *accept, deviceMode: *dev, noSlot11Backup: *noS11})
}
