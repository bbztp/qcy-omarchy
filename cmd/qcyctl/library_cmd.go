package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// `qcyctl library ...`: the curve library (named EQ curves stored under the
// data directory). "preset" is deliberately not used in the name: it already
// means a hardware slot. list/show/save/delete/export/import/seed are offline;
// import-device, apply and compose (without --dry-run) talk to the earbuds,
// through the same path as `eq set` (runViaDaemon, eqApply, backup, readback).

const libraryUsage = `usage: qcyctl library <subcommand>
  library list [--all] [--json]                      (factory effects in the app's order, then Experiência auditiva, then yours; --all also shows the old seeds a newer curve replaced)
  library show <id> [--json]
  library save <name> --gains g1,...,g10 [--master dB] [--note text] [--replace] [--json]
  library delete <id> --yes
  library export <id> <file>
  library import <file>
  library seed [--json]
  library import-device <name> [seconds] [--expect-slot N] [--user] [--replace [--backup]]   (reads the active EQ from the earbuds, never writes to them; --expect-slot saves nothing unless that slot is the active one; --user saves it as one of your own curves; --replace overwrites the existing entry of that name, needs --expect-slot, and --backup first copies the old entry to the backup directory)
  library matches (<id> | --any-user) --master dB --bands <json>      (offline: does the EQ the earbuds report (status eq.masterGain, eq.bands) match this entry, or one of your own curves? prints {"match":bool[,"id":...]})
  library apply <id>|--file <curve.json> --yes [--full-length] [--slot 0|255] [--json] [seconds]   (writes the curve to slot 255 by default = the phone app's "Personalizado" buffer, the only target allowed while slot 6 or 11 is active, and the only one that takes bands at positions 10-19 without --full-length; --slot 0 is the old test slot; --file applies a curve JSON such as a restore point from the backup directory without adding it to the library)
  library compose --base <id|none> --gains g1,...,g10 [--master dB] (--yes | --dry-run) [--json] [seconds]   (the phone app's layout in slot 255: your non-zero bars, then the base effect's bands)`

// libraryRunner opens the link to the earbuds (daemon first, direct otherwise).
// A test replaces it with a fake; nothing else in this file touches Bluetooth.
var libraryRunner = runViaDaemon

// defaultBandTable is the layout of slot 0 as the device reported it on first
// read (eq-slot0-first.hex): ten bands, Q 100, type 0.
var defaultBandTable = [qcycore.EqBands]int{31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}

const defaultBandQ = 100

// savedBandType is the type of the bars of a saved curve: 2, the bell every
// factory effect is made of and the one confirmed audible. Type 0, which this
// code used to write, sounded broken on the earbuds (quiet, treble gone).
const savedBandType = 2

func cmdLibrary(args []string) error {
	if len(args) < 1 {
		return errors.New(libraryUsage)
	}
	rest := args[1:]
	switch args[0] {
	case "list":
		return cmdLibraryList(rest)
	case "show":
		return cmdLibraryShow(rest)
	case "save":
		return cmdLibrarySave(rest)
	case "delete":
		return cmdLibraryDelete(rest)
	case "export":
		return cmdLibraryExport(rest)
	case "import":
		return cmdLibraryImport(rest)
	case "seed":
		return cmdLibrarySeed(rest)
	case "import-device":
		return cmdLibraryImportDevice(rest)
	case "matches":
		return cmdLibraryMatches(rest)
	case "apply":
		return cmdLibraryApply(rest)
	case "compose":
		return cmdLibraryCompose(rest)
	}
	return fmt.Errorf("library: unknown subcommand %q\n%s", args[0], libraryUsage)
}

// libArgs is a command line split into flags and positionals.
type libArgs struct {
	bools map[string]bool
	vals  map[string]string
	pos   []string
}

// parseLibArgs splits args. boolFlags take no value; valFlags take one
// ("--f v" or "--f=v", the value may start with "-"). Anything else starting
// with "--" is an error.
func parseLibArgs(args []string, boolFlags, valFlags []string) (libArgs, error) {
	a := libArgs{bools: map[string]bool{}, vals: map[string]string{}}
	isIn := func(l []string, s string) bool {
		for _, x := range l {
			if x == s {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			a.pos = append(a.pos, arg)
			continue
		}
		name, val, hasVal := strings.Cut(arg, "=")
		switch {
		case isIn(boolFlags, name) && !hasVal:
			a.bools[name] = true
		case isIn(valFlags, name):
			if !hasVal {
				if i+1 >= len(args) {
					return a, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			a.vals[name] = val
		default:
			return a, fmt.Errorf("unknown option %q", arg)
		}
	}
	return a, nil
}

// ---------- list / show ----------

type libraryEntry struct {
	qcycore.Curve
	NBands    int    `json:"nBands"`
	Applyable bool   `json:"applyable"`
	Reason    string `json:"applyableReason,omitempty"`
}

func newLibraryEntry(c qcycore.Curve) libraryEntry {
	ok, why := c.Applyable()
	return libraryEntry{Curve: c, NBands: len(c.Bands), Applyable: ok, Reason: why}
}

// curveRank orders the library: 0 the factory effects (in the app's order, see
// qcycore.FactoryEffects), 1 the hearing-test curve, 2 the user's own curves, 3
// everything else (device imports, older seeds). sub is the position among the
// factory effects.
func curveRank(c qcycore.Curve) (rank, sub int) {
	if c.Source == qcycore.SourceFactorySnapshot {
		for i, e := range qcycore.FactoryEffects {
			if c.ID == qcycore.FactoryCurveID(e) {
				return 0, i
			}
		}
	}
	switch {
	case c.Source != qcycore.SourceUser && c.ID == qcycore.HearingTestCurveID():
		return 1, 0
	case c.Source == qcycore.SourceUser:
		return 2, 0
	}
	return 3, 0
}

func sortCurves(cs []qcycore.Curve) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		ra, sa := curveRank(a)
		rb, sb := curveRank(b)
		if ra != rb {
			return ra < rb
		}
		if sa != sb {
			return sa < sb
		}
		if an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name); an != bn {
			return an < bn
		}
		return a.ID < b.ID
	})
}

// hideSuperseded drops the curves an older seed created under another id
// (qcycore.SupersededCurveIDs) while their replacement is in the library. A
// user's own curve is never hidden. It returns the kept curves and how many it
// dropped.
func hideSuperseded(cs []qcycore.Curve) ([]qcycore.Curve, int) {
	have := map[string]bool{}
	for _, c := range cs {
		have[c.ID] = true
	}
	sup := qcycore.SupersededCurveIDs()
	kept := cs[:0:0]
	for _, c := range cs {
		if repl, ok := sup[c.ID]; ok && c.Source != qcycore.SourceUser && c.ID != repl && have[repl] {
			continue
		}
		kept = append(kept, c)
	}
	return kept, len(cs) - len(kept)
}

func cmdLibraryList(args []string) error {
	const usage = "usage: qcyctl library list [--all] [--json]"
	a, err := parseLibArgs(args, []string{"--json", "--all"}, nil)
	if err != nil || len(a.pos) != 0 {
		return errors.New(usage)
	}
	curves, errs := qcycore.ListCurves()
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "qcyctl: library: skipping %v\n", e)
	}
	hidden := 0
	if !a.bools["--all"] {
		curves, hidden = hideSuperseded(curves)
	}
	sortCurves(curves)
	entries := make([]libraryEntry, len(curves))
	for i, c := range curves {
		entries[i] = newLibraryEntry(c)
	}
	if a.bools["--json"] {
		return printJSON(entries)
	}
	if len(entries) == 0 {
		fmt.Println("library is empty (qcyctl library seed, save or import-device)")
		return nil
	}
	for _, e := range entries {
		ap := "yes"
		if !e.Applyable {
			ap = "no: " + e.Reason
		}
		fmt.Printf("%-24s %-16s slot=%-3d bands=%-2d master=%+.2fdB applyable=%s  %q\n",
			e.ID, e.Source, e.Slot, e.NBands, e.MasterGain, ap, e.Name)
	}
	if hidden > 0 {
		fmt.Printf("(%d older curve(s) replaced by a newer one are hidden; --all shows them)\n", hidden)
	}
	return nil
}

func cmdLibraryShow(args []string) error {
	a, err := parseLibArgs(args, []string{"--json"}, nil)
	if err != nil || len(a.pos) != 1 {
		return errors.New("usage: qcyctl library show <id> [--json]")
	}
	c, err := qcycore.LoadCurve(a.pos[0])
	if err != nil {
		return err
	}
	e := newLibraryEntry(c)
	if a.bools["--json"] {
		return printJSON(e)
	}
	fmt.Printf("id:         %s\nname:       %s\nsource:     %s\nslot:       %d\nmasterGain: %+.2f dB\ncreatedAt:  %s\n", c.ID, c.Name, c.Source, c.Slot, c.MasterGain, c.CreatedAt)
	if e.Applyable {
		fmt.Println("applyable:  yes")
	} else {
		fmt.Printf("applyable:  no (%s)\n", e.Reason)
	}
	if c.Note != "" {
		fmt.Printf("note:       %s\n", c.Note)
	}
	if c.Payload != "" {
		fmt.Printf("payload:    %s\n", c.Payload)
	}
	for _, b := range c.Bands {
		fmt.Printf("  band %-2d %5d Hz  %+.2f dB  q=%d type=%d\n", b.Index, b.Freq, b.Gain, b.Q, b.Type)
	}
	return nil
}

// ---------- save / delete / export / import ----------

func cmdLibrarySave(args []string) error {
	const usage = "usage: qcyctl library save <name> --gains g1,...,g10 [--master dB] [--base <id>] [--note text] [--replace] [--json]"
	a, err := parseLibArgs(args, []string{"--json", "--replace"}, []string{"--gains", "--master", "--note", "--base"})
	if err != nil {
		return fmt.Errorf("library save: %w\n%s", err, usage)
	}
	if len(a.pos) != 1 {
		return errors.New(usage)
	}
	gainsArg, ok := a.vals["--gains"]
	if !ok {
		return errors.New("library save: --gains is required\n" + usage)
	}
	gains, err := qcycore.ParseGains(gainsArg)
	if err != nil {
		return fmt.Errorf("library save: %w", err)
	}
	var master float64
	if v, ok := a.vals["--master"]; ok {
		c, err := parseDBArg("library save: --master", v)
		if err != nil {
			return err
		}
		master = float64(c) / 100
	}
	bands := make([]qcycore.EqBand, qcycore.EqBands)
	for i, g := range gains {
		bands[i] = qcycore.EqBand{Index: i, Freq: defaultBandTable[i], Gain: float64(g) / 100, Q: defaultBandQ, Type: savedBandType}
	}
	// With --base the gains are bars ON TOP of that curve, saved the way
	// `compose` writes them (the bars that are not zero, then the base's own
	// bands), so applying the saved curve plays exactly what compose played.
	if baseID, ok := a.vals["--base"]; ok && baseID != "none" {
		base, err := qcycore.LoadCurve(baseID)
		if err != nil {
			return fmt.Errorf("library save: %w", err)
		}
		specs, err := composeBands(&base, gains)
		if err != nil {
			return err
		}
		bands = bands[:0]
		for _, b := range specs {
			bands = append(bands, qcycore.EqBand{Index: b.Index, Freq: b.Freq, Gain: b.Gain, Q: b.Q, Type: b.Type})
		}
	}
	c := qcycore.Curve{
		Version:    qcycore.CurveVersion,
		ID:         qcycore.Slug(a.pos[0]),
		Name:       strings.TrimSpace(a.pos[0]),
		Source:     qcycore.SourceUser,
		Slot:       -1,
		MasterGain: master,
		Bands:      bands,
		Note:       a.vals["--note"],
	}
	if a.bools["--replace"] {
		old, err := qcycore.LoadCurve(c.ID)
		if err != nil {
			return fmt.Errorf("library save --replace: %w", err)
		}
		if old.Source != qcycore.SourceUser {
			return fmt.Errorf("library save --replace: curve %q has source %q; only source=user curves can be replaced", old.ID, old.Source)
		}
		c.CreatedAt = old.CreatedAt
		if err := qcycore.UpdateCurve(c); err != nil {
			return err
		}
	} else if err := qcycore.CreateCurve(c); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w (pick another name or pass --replace)", err)
		}
		return err
	}
	saved, err := qcycore.LoadCurve(c.ID)
	if err != nil {
		return err
	}
	if a.bools["--json"] {
		return printJSON(newLibraryEntry(saved))
	}
	fmt.Printf("saved %s (%q)\n", saved.ID, saved.Name)
	return nil
}

func cmdLibraryDelete(args []string) error {
	a, err := parseLibArgs(args, []string{"--yes"}, nil)
	if err != nil || len(a.pos) != 1 {
		return errors.New("usage: qcyctl library delete <id> --yes")
	}
	if !a.bools["--yes"] {
		return errors.New("library delete: pass --yes to confirm (the file is moved to <library>/.trash, not erased)")
	}
	if err := qcycore.DeleteCurve(a.pos[0]); err != nil {
		return err
	}
	fmt.Printf("deleted %s (moved to the library .trash)\n", a.pos[0])
	return nil
}

func cmdLibraryExport(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: qcyctl library export <id> <file>")
	}
	if err := qcycore.ExportCurve(args[0], args[1]); err != nil {
		return err
	}
	fmt.Printf("exported %s to %s\n", args[0], args[1])
	return nil
}

func cmdLibraryImport(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: qcyctl library import <file>")
	}
	c, err := qcycore.ImportCurve(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("imported %s (%q)\n", c.ID, c.Name)
	return nil
}

// ---------- seed ----------

var firstHexRe = regexp.MustCompile(`^eq-slot(\d+)-first\.hex$`)

type seedItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

type seedReport struct {
	Created []seedItem `json:"created"`
	Skipped []seedItem `json:"skipped"`
	Errors  []string   `json:"errors"`
}

func readSmallFile(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, qcycore.MaxCurveFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > qcycore.MaxCurveFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, qcycore.MaxCurveFileSize)
	}
	return data, nil
}

// hearingTestPayload finds the user's hearing-test payload in the backups:
// eq-slot11-first.hex, else the "payload" field of the first
// preset-map/experiencia-auditiva-slot11-*.json that has one.
func hearingTestPayload(dir string) (payload string, errs []string) {
	if data, err := readSmallFile(filepath.Join(dir, "eq-slot11-first.hex")); err == nil {
		return string(data), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err.Error())
	}
	jsons, _ := filepath.Glob(filepath.Join(dir, "preset-map", "experiencia-auditiva-slot11-*.json"))
	sort.Strings(jsons)
	for _, p := range jsons {
		data, err := readSmallFile(p)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		var f struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(data, &f); err != nil || f.Payload == "" {
			errs = append(errs, fmt.Sprintf("%s: no usable payload field", p))
			continue
		}
		return f.Payload, errs
	}
	return "", errs
}

// seedLibrary creates the curves that are missing, never touching one that is
// there:
//
//  1. one factory-snapshot curve per qcycore.FactoryEffects entry, from the
//     payloads embedded in the binary (no backup directory needed);
//  2. "Experiência auditiva" (device-import, slot 11) from the user's backups;
//  3. a device-import curve for every other eq-slotN-first.hex (slot 0, 255,
//     ...), named as older versions named them. Slots the embedded factory data
//     or the hearing-test curve now cover are not seeded again under the old
//     "(slot N)" names: those ids are listed as superseded (qcycore.SupersededCurveIDs).
//
// An id already in the library is skipped, never renamed or replaced.
func seedLibrary() (seedReport, error) {
	rep := seedReport{Created: []seedItem{}, Skipped: []seedItem{}, Errors: []string{}}
	dir, err := qcycore.BackupDir()
	if err != nil {
		return rep, err
	}
	add := func(source string, slot int, name, legacy, payload string) {
		id := qcycore.Slug(name)
		// Curves seeded by older versions keep their old id; do not duplicate
		// them under the readable name.
		if legacy != "" {
			lid := qcycore.Slug(legacy)
			if _, err := qcycore.LoadCurve(lid); err == nil {
				rep.Skipped = append(rep.Skipped, seedItem{ID: lid, Reason: "already in the library"})
				return
			}
		}
		c, err := qcycore.SeedFromPayload(source, slot, name, payload)
		switch {
		case err == nil:
			rep.Created = append(rep.Created, seedItem{ID: c.ID})
		case errors.Is(err, os.ErrExist):
			rep.Skipped = append(rep.Skipped, seedItem{ID: id, Reason: "already in the library"})
		default:
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", name, err))
		}
	}

	covered := map[int]bool{qcycore.HearingTestSlot: true}
	for _, e := range qcycore.FactoryEffects {
		covered[e.Slot] = true
		add(qcycore.SourceFactorySnapshot, e.Slot, e.Name, "", e.Payload)
	}

	payload, errs := hearingTestPayload(dir)
	rep.Errors = append(rep.Errors, errs...)
	switch {
	case payload != "":
		add(qcycore.SourceDeviceImport, qcycore.HearingTestSlot, qcycore.HearingTestName, "", payload)
	default:
		id := qcycore.HearingTestCurveID()
		reason := "no backup of the hearing-test curve (eq-slot11-first.hex or preset-map/experiencia-auditiva-slot11-*.json)"
		if _, err := qcycore.LoadCurve(id); err == nil {
			reason = "already in the library"
		}
		rep.Skipped = append(rep.Skipped, seedItem{ID: id, Reason: reason})
	}

	type hexSrc struct {
		slot int
		path string
	}
	var hexes []hexSrc
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if m := firstHexRe.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
				if n, err := strconv.Atoi(m[1]); err == nil && n <= 255 && !covered[n] {
					hexes = append(hexes, hexSrc{n, filepath.Join(dir, e.Name())})
				}
			}
		}
	}
	sort.Slice(hexes, func(i, j int) bool { return hexes[i].slot < hexes[j].slot })
	for _, h := range hexes {
		data, err := readSmallFile(h.path)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			continue
		}
		legacy := fmt.Sprintf("Slot %d (primeira leitura)", h.slot)
		name, lg := legacy, ""
		if eff, ok := qcycore.EffectName(h.slot); ok {
			name, lg = fmt.Sprintf("%s (slot %d)", eff, h.slot), legacy
		}
		add(qcycore.SourceDeviceImport, h.slot, name, lg, string(data))
	}
	return rep, nil
}

func cmdLibrarySeed(args []string) error {
	a, err := parseLibArgs(args, []string{"--json"}, nil)
	if err != nil || len(a.pos) != 0 {
		return errors.New("usage: qcyctl library seed [--json]")
	}
	rep, err := seedLibrary()
	if err != nil {
		return err
	}
	for _, e := range rep.Errors {
		fmt.Fprintf(os.Stderr, "qcyctl: library seed: %s\n", e)
	}
	if a.bools["--json"] {
		return printJSON(rep)
	}
	for _, c := range rep.Created {
		fmt.Printf("created %s\n", c.ID)
	}
	for _, s := range rep.Skipped {
		fmt.Printf("skipped %s (%s)\n", s.ID, s.Reason)
	}
	if len(rep.Created) == 0 && len(rep.Skipped) == 0 {
		fmt.Println("nothing to seed")
	}
	return nil
}

// ---------- import-device / apply ----------

func cmdLibraryImportDevice(args []string) error {
	const usage = "usage: qcyctl library import-device <name> [seconds] [--expect-slot N] [--user] [--replace [--backup]] [--json]"
	a, err := parseLibArgs(args, []string{"--json", "--user", "--replace", "--backup"}, []string{"--expect-slot"})
	if err != nil || len(a.pos) < 1 || len(a.pos) > 2 {
		return errors.New(usage)
	}
	replace, backup := a.bools["--replace"], a.bools["--backup"]
	if backup && !replace {
		return errors.New("library import-device: --backup only goes with --replace\n" + usage)
	}
	expect := -1
	if v, ok := a.vals["--expect-slot"]; ok {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 || n > 255 {
			return fmt.Errorf("--expect-slot: %q is not a slot number", v)
		}
		expect = n
	}
	if replace && expect < 0 {
		return errors.New("library import-device: --replace needs --expect-slot, so nothing is overwritten when the active slot moved")
	}
	name := a.pos[0]
	source := qcycore.SourceDeviceImport
	if a.bools["--user"] {
		source = qcycore.SourceUser
	}
	// Validate the name (and refuse a taken id, or a missing one to replace)
	// before connecting.
	id := qcycore.Slug(name)
	existing, loadErr := qcycore.LoadCurve(id)
	switch {
	case !replace && loadErr == nil:
		return fmt.Errorf("curve %q already exists; pick another name", id)
	case replace && loadErr != nil:
		return fmt.Errorf("library import-device --replace: %w", loadErr)
	case replace && existing.Ears != nil && !backup:
		return fmt.Errorf("library import-device --replace: %q has per-ear curves (hearing-test result) that the new entry will not have; pass --backup to keep a copy first", id)
	}
	seconds, err := parseSeconds(a.pos[1:], 30)
	if err != nil {
		return err
	}
	return libraryRunner(seconds, func(dc daemonAPI) error {
		snap, err := dc.eqRead()
		if err != nil {
			return fmt.Errorf("library import-device: %w", err)
		}
		if expect >= 0 && snap.Slot != expect {
			return fmt.Errorf("library import-device: the active slot is %d, not %d; nothing saved", snap.Slot, expect)
		}
		c, err := qcycore.CurveFromPayload(source, snap.Slot, name, snap.Payload)
		if err != nil {
			return err
		}
		if replace {
			// Re-read: the entry is what gets copied, so it must be the one
			// about to be overwritten.
			old, err := qcycore.LoadCurve(c.ID)
			if err != nil {
				return fmt.Errorf("library import-device --replace: %w", err)
			}
			if backup {
				p, err := qcycore.BackupCurve(old, time.Now())
				if err != nil {
					return fmt.Errorf("library import-device: backup failed, nothing replaced: %w", err)
				}
				if !a.bools["--json"] {
					fmt.Printf("backed up %s to %s\n", old.ID, p)
				}
			}
			if _, err := qcycore.UpsertCurve(c); err != nil {
				return err
			}
		} else if err := qcycore.CreateCurve(c); err != nil {
			return err
		}
		if a.bools["--json"] {
			return printJSON(newLibraryEntry(c))
		}
		fmt.Printf("imported %s from slot %d (%q)\n", c.ID, c.Slot, c.Name)
		return nil
	})
}

// cmdLibraryMatches is offline: it compares an entry with the EQ the earbuds
// reported (qcycore.CurveMatchesDevice), so the panel does not duplicate the
// rule. With --any-user it tries every curve of the user and the hearing-test
// card (not the factory effects nor the editor's working copy) and names the first that
// matches, in library order.
func cmdLibraryMatches(args []string) error {
	const usage = "usage: qcyctl library matches (<id> | --any-user) --master dB --bands <json>"
	a, err := parseLibArgs(args, []string{"--any-user"}, []string{"--master", "--bands"})
	anyUser := a.bools["--any-user"]
	if err != nil || (anyUser && len(a.pos) != 0) || (!anyUser && len(a.pos) != 1) {
		return errors.New(usage)
	}
	master, err := strconv.ParseFloat(a.vals["--master"], 64)
	if err != nil {
		return fmt.Errorf("library matches: --master: %w", err)
	}
	var bands []qcycore.EqBand
	if err := json.Unmarshal([]byte(a.vals["--bands"]), &bands); err != nil {
		return fmt.Errorf("library matches: --bands: %w", err)
	}
	if !anyUser {
		c, err := qcycore.LoadCurve(a.pos[0])
		if err != nil {
			return err
		}
		return printJSON(map[string]bool{"match": qcycore.CurveMatchesDevice(c, master, bands)})
	}
	curves, _ := qcycore.ListCurves()
	sortCurves(curves)
	for _, c := range curves {
		hearing := c.ID == qcycore.HearingTestCurveID()
		if (c.Source != qcycore.SourceUser && !hearing) || c.ID == "rascunho" {
			continue
		}
		// In Personalizado the earbuds report the entry's own bands (a card
		// applied there is written whole), not the left-ear layout of slot 11.
		plain := c
		plain.Ears = nil
		if qcycore.CurveMatchesDevice(plain, master, bands) {
			return printJSON(map[string]any{"match": true, "id": c.ID})
		}
	}
	return printJSON(map[string]any{"match": false})
}

// libraryApplyOp builds the EqOp that writes c to slot (0 or the app's custom
// slot 255, validated by the caller), or refuses with the reason. It never sets
// AllowProtected. Everything it can know offline it checks here, so nothing is
// connected for a curve that cannot be written. Bands at positions 10-19 make a
// 143-byte payload: slot 255 takes it as is, slot 0 needs fullLength.
func libraryApplyOp(c qcycore.Curve, fullLength bool, slot int) (qcycore.EqOp, error) {
	var op qcycore.EqOp
	if ok, why := c.Applyable(); !ok {
		return op, fmt.Errorf("curve %q cannot be applied: %s", c.ID, why)
	}
	mc, err := qcycore.MasterCentiFromDB(c.MasterGain)
	if err != nil {
		return op, fmt.Errorf("curve %q: master gain: %w", c.ID, err)
	}
	bands := make([]qcycore.BandSpec, len(c.Bands))
	for i, b := range c.Bands {
		bands[i] = qcycore.BandSpec{Index: b.Index, Freq: b.Freq, Gain: b.Gain, Q: b.Q, Type: b.Type}
	}
	op = qcycore.EqOp{Slot: &slot, MasterCenti: &mc, Bands: bands, AllowFullLength: fullLength}
	if _, err := dryRunEq(op); err != nil {
		if errors.Is(err, qcycore.ErrFullLengthRequired) {
			return op, &qcycore.EqError{Code: qcycore.CodeFullLengthRequired, Msg: fmt.Sprintf(
				"curve %q cannot be applied to slot %d: it has bands at positions 10-19, which need the 143-byte format; that format has NOT been proven on the hardware for slot %d; pass --full-length to allow it (slot %d takes it without the flag)",
				c.ID, slot, slot, qcycore.SlotCustomApp)}
		}
		return op, fmt.Errorf("curve %q cannot be applied: %w", c.ID, err)
	}
	return op, nil
}

// dryRunEq builds the payload op would write against a neutral active slot 0:
// band ranges, band count and the length rule fail here, offline. The
// protected-slot rule depends on the real active slot and is enforced by
// ApplyEq on the live payload.
func dryRunEq(op qcycore.EqOp) ([]byte, error) {
	return qcycore.ApplyEq(make([]byte, qcycore.EqLen), op)
}

type libraryApplyWrote struct {
	Slot       int                `json:"slot"`
	MasterGain float64            `json:"masterGain"`
	Bands      []qcycore.BandSpec `json:"bands"`
	FullLength bool               `json:"fullLength"`
}

type libraryApplyOut struct {
	ID         string            `json:"id"`
	Base       string            `json:"base,omitempty"` // compose only
	Confirmed  bool              `json:"confirmed"`
	Wrote      libraryApplyWrote `json:"wrote"`
	Result     *qcycore.EqResult `json:"result,omitempty"`
	Backup     string            `json:"backup,omitempty"`
	Error      string            `json:"error,omitempty"`
	Code       string            `json:"code,omitempty"`
	Sent       string            `json:"sent,omitempty"`
	DeviceSaid string            `json:"deviceReported,omitempty"`
}

// libraryWrite is the write path shared by `library apply` and `library
// compose`: the daemon (or a direct link) reads the active payload, builds the
// payload from op, backs it up, writes and confirms by readback. verb and
// subject only shape the text output.
func libraryWrite(verb, subject string, out libraryApplyOut, op qcycore.EqOp, seconds int, jsonOut bool) error {
	out.Wrote = libraryApplyWrote{Slot: *op.Slot, MasterGain: float64(*op.MasterCenti) / 100, Bands: op.Bands}
	return libraryRunner(seconds, func(dc daemonAPI) error {
		res, err := dc.eqApply(op)
		if err != nil {
			var ee *qcycore.EqError
			if jsonOut && errors.As(err, &ee) {
				out.Error, out.Code, out.Backup = ee.Msg, ee.Code, ee.Backup
				if ee.Sent != nil {
					out.Sent = fmt.Sprintf("%x", ee.Sent)
					out.DeviceSaid = fmt.Sprintf("%x", ee.Got)
				}
				_ = printJSON(out)
			}
			return err
		}
		out.Confirmed = true
		out.Result = res
		out.Backup = res.BackupHistory
		out.Wrote.FullLength = res.FullLength
		if jsonOut {
			return printJSON(out)
		}
		fmt.Printf("library %s confirmed: %s -> slot=%d masterGain=%+.2fdB %s\n", verb, subject, res.Slot, res.MasterGain, fmtEq(&res.EqSnapshot))
		warnFullLength(res)
		fmt.Printf("backup: %s\n", res.BackupHistory)
		return nil
	})
}

func cmdLibraryApply(args []string) error {
	const usage = "usage: qcyctl library apply <id> | --file <curve.json> --yes [--full-length] [--slot 0|255] [--json] [seconds]"
	a, err := parseLibArgs(args, []string{"--yes", "--full-length", "--json"}, []string{"--slot", "--file"})
	if err != nil {
		return errors.New(usage)
	}
	file, fromFile := a.vals["--file"]
	// --file replaces <id>: the curve comes from a backup file (or an exported
	// curve) and is not stored in the library.
	if fromFile && len(a.pos) > 1 || !fromFile && (len(a.pos) < 1 || len(a.pos) > 2) {
		return errors.New(usage)
	}
	if !a.bools["--yes"] {
		return errors.New("library apply: this overwrites the EQ curve of the target slot (255 by default, or 0) and makes it active; pass --yes to confirm (only the ACTIVE slot's payload is backed up first; the previous content of slot 255, when it is not the active one, is REPLACED with no copy, as the phone app does when you pick a curve)")
	}
	slot := qcycore.SlotCustomApp
	if v, ok := a.vals["--slot"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 0 && n != qcycore.SlotCustomApp) {
			return fmt.Errorf("library apply: --slot must be 0 or %d (the phone app's \"Personalizado\" buffer), got %q; protected and factory slots are never written", qcycore.SlotCustomApp, v)
		}
		slot = n
	}
	rest := a.pos
	if !fromFile {
		rest = a.pos[1:]
	}
	seconds, err := parseSeconds(rest, 30)
	if err != nil {
		return err
	}
	var c qcycore.Curve
	if fromFile {
		c, err = qcycore.LoadCurveFile(file)
	} else {
		c, err = qcycore.LoadCurve(a.pos[0])
	}
	if err != nil {
		return err
	}
	if c.Ears != nil {
		// A hearing-test result: the app re-sends 0x46/0x47 index 11 from the
		// two ear curves it keeps; it never writes it through 0x22.
		if _, ok := a.vals["--slot"]; ok || a.bools["--full-length"] {
			return errors.New("library apply: --slot/--full-length do not apply to a hearing-test card: it is written as 0x46/0x47 index 11, not as a 0x22 curve")
		}
		out := io.Writer(os.Stdout)
		if a.bools["--json"] {
			out = os.Stderr // stdout carries the JSON result only
		}
		env, done := liveHearingEnv(out)
		defer done()
		return hearingApplyEars(env, c, a.bools["--json"], false)
	}
	op, err := libraryApplyOp(c, a.bools["--full-length"], slot)
	if err != nil {
		return err
	}
	return libraryWrite("apply", c.ID, libraryApplyOut{ID: c.ID}, op, seconds, a.bools["--json"])
}

// ---------- compose ----------

// composeMasterDefault is the master gain of a compose without a base curve
// and without --master: the -12 dB every factory effect reports.
const composeMasterDefault = -12.0

// composeBands lays bands out the way the phone app does in slot 255: first
// the user's bars (only those with a non-zero gain, at the ISO frequency of the
// bar, Q 100, type 255), then the bands of the base effect in their original
// order. Positions are consecutive from 0. More than EqMaxBands is an error.
func composeBands(base *qcycore.Curve, gains [qcycore.EqBands]int) ([]qcycore.BandSpec, error) {
	var out []qcycore.BandSpec
	for i, g := range gains {
		if g == 0 {
			continue
		}
		out = append(out, qcycore.BandSpec{Index: len(out), Freq: defaultBandTable[i], Gain: float64(g) / 100, Q: defaultBandQ, Type: composeUserBandType})
	}
	user := len(out)
	if base != nil {
		for _, b := range base.Bands {
			out = append(out, qcycore.BandSpec{Index: len(out), Freq: b.Freq, Gain: b.Gain, Q: b.Q, Type: b.Type})
		}
	}
	if len(out) > qcycore.EqMaxBands {
		return nil, fmt.Errorf("library compose: %d bars of yours + %d bands of the base = %d bands, but the earbuds hold at most %d; zero some bars or pick a base with fewer bands", user, len(out)-user, len(out), qcycore.EqMaxBands)
	}
	if len(out) == 0 {
		return nil, errors.New("library compose: nothing to write: every gain is 0 and there is no base curve")
	}
	return out, nil
}

// composeUserBandType is the type byte the phone app gives to the user's bars.
const composeUserBandType = 0xff

type libraryComposePlan struct {
	Base       string           `json:"base"`
	Slot       int              `json:"slot"`
	MasterGain float64          `json:"masterGain"`
	NBands     int              `json:"nBands"`
	FullLength bool             `json:"fullLength"`
	Payload    string           `json:"payload"`
	Bands      []qcycore.EqBand `json:"bands"`
}

func cmdLibraryCompose(args []string) error {
	const usage = "usage: qcyctl library compose --base <id|none> --gains g1,...,g10 [--master dB] (--yes | --dry-run) [--json] [seconds]"
	a, err := parseLibArgs(args, []string{"--yes", "--dry-run", "--json"}, []string{"--base", "--gains", "--master"})
	if err != nil {
		return fmt.Errorf("library compose: %w\n%s", err, usage)
	}
	if len(a.pos) > 1 {
		return errors.New(usage)
	}
	baseArg, okB := a.vals["--base"]
	gainsArg, okG := a.vals["--gains"]
	if !okB || !okG {
		return errors.New("library compose: --base and --gains are required\n" + usage)
	}
	dry := a.bools["--dry-run"]
	if !dry && !a.bools["--yes"] {
		return errors.New("library compose: this overwrites slot 255 (the phone app's \"Personalizado\") and makes it active; pass --yes to confirm (only the ACTIVE slot's payload is backed up first; the previous content of slot 255, when it is not the active one, is REPLACED with no copy, as the phone app does when you pick a curve), or --dry-run to only print the payload")
	}
	gains, err := qcycore.ParseGains(gainsArg)
	if err != nil {
		return fmt.Errorf("library compose: %w", err)
	}
	seconds, err := parseSeconds(a.pos, 30)
	if err != nil {
		return err
	}
	var base *qcycore.Curve
	masterC, err := qcycore.MasterCentiFromDB(composeMasterDefault)
	if err != nil {
		return err
	}
	baseName := "none"
	if baseArg != "none" {
		c, err := qcycore.LoadCurve(baseArg)
		if err != nil {
			return err
		}
		if ok, why := c.Applyable(); !ok {
			return fmt.Errorf("library compose: base %q cannot be written: %s", c.ID, why)
		}
		if masterC, err = qcycore.MasterCentiFromDB(c.MasterGain); err != nil {
			return fmt.Errorf("library compose: base %q: master gain: %w", c.ID, err)
		}
		base, baseName = &c, c.ID
	}
	if v, ok := a.vals["--master"]; ok {
		if masterC, err = parseDBArg("library compose: --master", v); err != nil {
			return err
		}
	}
	bands, err := composeBands(base, gains)
	if err != nil {
		return err
	}
	slot := qcycore.SlotCustomApp
	op := qcycore.EqOp{Slot: &slot, MasterCenti: &masterC, Bands: bands}
	payload, err := dryRunEq(op)
	if err != nil {
		return fmt.Errorf("library compose: %w", err)
	}
	snap, err := qcycore.DecodeEq(payload)
	if err != nil {
		return err
	}
	if dry {
		plan := libraryComposePlan{
			Base: baseName, Slot: slot, MasterGain: snap.MasterGain, NBands: len(snap.Bands),
			FullLength: qcycore.FullLength(payload), Payload: snap.Payload, Bands: snap.Bands,
		}
		if a.bools["--json"] {
			return printJSON(plan)
		}
		fmt.Printf("library compose (dry run, nothing written): base=%s slot=%d masterGain=%+.2fdB bands=%d fullLength=%v\npayload: %s\n",
			plan.Base, plan.Slot, plan.MasterGain, plan.NBands, plan.FullLength, plan.Payload)
		for _, b := range plan.Bands {
			fmt.Printf("  band %-2d %5d Hz  %+.2f dB  q=%d type=%d\n", b.Index, b.Freq, b.Gain, b.Q, b.Type)
		}
		return nil
	}
	return libraryWrite("compose", "base="+baseName, libraryApplyOut{ID: "compose", Base: baseName}, op, seconds, a.bools["--json"])
}
