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

	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
)

// `qcyctl library ...`: the curve library (named EQ curves stored under the
// data directory). "preset" is deliberately not used in the name: it already
// means a hardware slot. list/show/save/delete/export/import/seed are offline;
// only import-device and apply talk to the earbuds, through the same path as
// `eq set` (runViaDaemon, eqApply, backup, readback).

const libraryUsage = `usage: qcyctl library <subcommand>
  library list [--json]
  library show <id> [--json]
  library save <name> --gains g1,...,g10 [--master dB] [--note text] [--replace] [--json]
  library delete <id> --yes
  library export <id> <file>
  library import <file>
  library seed [--json]
  library import-device <name> [seconds]            (reads the active EQ from the earbuds)
  library apply <id> --yes [--full-length] [--json] [seconds]   (writes the curve to slot 0)`

// libraryRunner opens the link to the earbuds (daemon first, direct otherwise).
// A test replaces it with a fake; nothing else in this file touches Bluetooth.
var libraryRunner = runViaDaemon

// defaultBandTable is the layout of slot 0 as the device reported it on first
// read (eq-slot0-first.hex): ten bands, Q 100, type 0.
var defaultBandTable = [qcycore.EqBands]int{31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}

const defaultBandQ = 100

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
	case "apply":
		return cmdLibraryApply(rest)
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

func sourceRank(s string) int {
	switch s {
	case qcycore.SourceUser:
		return 0
	case qcycore.SourceDeviceImport:
		return 1
	}
	return 2
}

func sortCurves(cs []qcycore.Curve) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if ra, rb := sourceRank(a.Source), sourceRank(b.Source); ra != rb {
			return ra < rb
		}
		if an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name); an != bn {
			return an < bn
		}
		return a.ID < b.ID
	})
}

func cmdLibraryList(args []string) error {
	a, err := parseLibArgs(args, []string{"--json"}, nil)
	if err != nil || len(a.pos) != 0 {
		return errors.New("usage: qcyctl library list [--json]")
	}
	curves, errs := qcycore.ListCurves()
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "qcyctl: library: skipping %v\n", e)
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
	const usage = "usage: qcyctl library save <name> --gains g1,...,g10 [--master dB] [--note text] [--replace] [--json]"
	a, err := parseLibArgs(args, []string{"--json", "--replace"}, []string{"--gains", "--master", "--note"})
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
		bands[i] = qcycore.EqBand{Index: i, Freq: defaultBandTable[i], Gain: float64(g) / 100, Q: defaultBandQ, Type: 0}
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

// seedLibrary creates the curves that are missing from the backup directory's
// raw payloads. Only the hex files and the "payload" field of the slot 6
// snapshots are read; an id already in the library is skipped.
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

	type hexSrc struct {
		slot int
		path string
	}
	var hexes []hexSrc
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if m := firstHexRe.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
				if n, err := strconv.Atoi(m[1]); err == nil && n <= 255 {
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
		// Slot 6 keeps the legacy name: "Clássico (slot 6)" is the factory snapshot's.
		if eff, ok := qcycore.EffectName(h.slot); ok && h.slot != 6 {
			name, lg = fmt.Sprintf("%s (slot %d)", eff, h.slot), legacy
		}
		add(qcycore.SourceDeviceImport, h.slot, name, lg, string(data))
	}

	jsons, _ := filepath.Glob(filepath.Join(dir, "preset-map", "v3-*-slot6.json"))
	sort.Strings(jsons)
	for _, p := range jsons {
		data, err := readSmallFile(p)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			continue
		}
		var f struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(data, &f); err != nil || f.Payload == "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: no usable payload field", p))
			continue
		}
		add(qcycore.SourceFactorySnapshot, 6, "Clássico (slot 6)", "", f.Payload)
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
	const usage = "usage: qcyctl library import-device <name> [seconds]"
	a, err := parseLibArgs(args, []string{"--json"}, nil)
	if err != nil || len(a.pos) < 1 || len(a.pos) > 2 {
		return errors.New(usage)
	}
	name := a.pos[0]
	// Validate the name (and refuse a taken id) before connecting.
	id := qcycore.Slug(name)
	if _, err := qcycore.LoadCurve(id); err == nil {
		return fmt.Errorf("curve %q already exists; pick another name", id)
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
		c, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, snap.Slot, name, snap.Payload)
		if err != nil {
			return err
		}
		if a.bools["--json"] {
			return printJSON(newLibraryEntry(c))
		}
		fmt.Printf("imported %s from slot %d (%q)\n", c.ID, c.Slot, c.Name)
		return nil
	})
}

// libraryApplyOp builds the EqOp that writes c to slot 0, or refuses with the
// reason. It never sets AllowProtected. Everything it can know offline it
// checks here, so nothing is connected for a curve that cannot be written.
func libraryApplyOp(c qcycore.Curve, fullLength bool) (qcycore.EqOp, error) {
	var op qcycore.EqOp
	if ok, why := c.Applyable(); !ok {
		tail := false
		for _, b := range c.Bands {
			if b.Index >= qcycore.EqBands {
				tail = true
			}
		}
		if !tail {
			return op, fmt.Errorf("curve %q cannot be applied: %s", c.ID, why)
		}
		if !fullLength {
			return op, fmt.Errorf("curve %q cannot be applied: %s; writing it needs the 143-byte format, which has NOT been proven on the hardware; pass --full-length to allow it", c.ID, why)
		}
		// With --full-length only the tail may be what blocks it: check the rest.
		head := c
		head.Payload = ""
		head.Bands = nil
		for _, b := range c.Bands {
			if b.Index < qcycore.EqBands {
				head.Bands = append(head.Bands, b)
			}
		}
		if ok, why := head.Applyable(); !ok {
			return op, fmt.Errorf("curve %q cannot be applied: %s", c.ID, why)
		}
	}
	mc, err := qcycore.MasterCentiFromDB(c.MasterGain)
	if err != nil {
		return op, fmt.Errorf("curve %q: master gain: %w", c.ID, err)
	}
	slot := 0
	bands := make([]qcycore.BandSpec, len(c.Bands))
	for i, b := range c.Bands {
		bands[i] = qcycore.BandSpec{Index: b.Index, Freq: b.Freq, Gain: b.Gain, Q: b.Q, Type: b.Type}
	}
	op = qcycore.EqOp{Slot: &slot, MasterCenti: &mc, Bands: bands, AllowFullLength: fullLength}
	// Dry run against a neutral active slot 0: band ranges, band count and the
	// length rule fail here, offline. The protected-slot rule depends on the
	// real active slot and is enforced by ApplyEq on the live payload.
	if _, err := qcycore.ApplyEq(make([]byte, qcycore.EqLen), op); err != nil {
		return op, fmt.Errorf("curve %q cannot be applied: %w", c.ID, err)
	}
	return op, nil
}

type libraryApplyWrote struct {
	Slot       int                `json:"slot"`
	MasterGain float64            `json:"masterGain"`
	Bands      []qcycore.BandSpec `json:"bands"`
	FullLength bool               `json:"fullLength"`
}

type libraryApplyOut struct {
	ID         string            `json:"id"`
	Confirmed  bool              `json:"confirmed"`
	Wrote      libraryApplyWrote `json:"wrote"`
	Result     *qcycore.EqResult `json:"result,omitempty"`
	Backup     string            `json:"backup,omitempty"`
	Error      string            `json:"error,omitempty"`
	Code       string            `json:"code,omitempty"`
	Sent       string            `json:"sent,omitempty"`
	DeviceSaid string            `json:"deviceReported,omitempty"`
}

func cmdLibraryApply(args []string) error {
	const usage = "usage: qcyctl library apply <id> --yes [--full-length] [--json] [seconds]"
	a, err := parseLibArgs(args, []string{"--yes", "--full-length", "--json"}, nil)
	if err != nil || len(a.pos) < 1 || len(a.pos) > 2 {
		return errors.New(usage)
	}
	if !a.bools["--yes"] {
		return errors.New("library apply: this overwrites the EQ curve of slot 0 and makes it active; pass --yes to confirm (the active slot's payload is backed up first)")
	}
	seconds, err := parseSeconds(a.pos[1:], 30)
	if err != nil {
		return err
	}
	c, err := qcycore.LoadCurve(a.pos[0])
	if err != nil {
		return err
	}
	full := a.bools["--full-length"]
	op, err := libraryApplyOp(c, full)
	if err != nil {
		return err
	}
	jsonOut := a.bools["--json"]
	out := libraryApplyOut{
		ID: c.ID,
		Wrote: libraryApplyWrote{
			Slot: *op.Slot, MasterGain: float64(*op.MasterCenti) / 100, Bands: op.Bands,
		},
	}
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
		fmt.Printf("library apply confirmed: %s -> slot=%d masterGain=%+.2fdB %s\n", c.ID, res.Slot, res.MasterGain, fmtEq(&res.EqSnapshot))
		warnFullLength(res)
		fmt.Printf("backup: %s\n", res.BackupHistory)
		return nil
	})
}
