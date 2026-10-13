package qcycore

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// The curve library: named EQ curves kept as one JSON file each under
// <DataDir>/library. "Preset" is deliberately not used here, it already means
// a hardware slot.
//
// File format v1 (see Curve). When "payload" (the raw 0x22 bytes, hex) is
// present it is the source of truth and bands/masterGain must match what
// DecodeEq derives from it, so a curve imported from the device round-trips
// byte for byte.

const (
	// CurveVersion is the only file format version this build reads or writes.
	CurveVersion = 1
	// MaxCurveFileSize is the largest curve file that is read or imported.
	MaxCurveFileSize = 64 << 10

	curveNameMax = 80
	curveNoteMax = 500

	// Curve sources.
	SourceUser            = "user"
	SourceDeviceImport    = "device-import"
	SourceFactorySnapshot = "factory-snapshot"

	minFreqHz = 20
	maxFreqHz = 20000
)

var curveIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Curve is one library entry.
type Curve struct {
	Version    int      `json:"version"`
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Source     string   `json:"source"`
	Slot       int      `json:"slot"` // slot the curve came from, -1 if none
	MasterGain float64  `json:"masterGain"`
	Payload    string   `json:"payload,omitempty"`
	Bands      []EqBand `json:"bands"`
	// Ears, when present, holds the two curves of a hearing-test result (what
	// the app keeps on the phone and re-sends as 0x46/0x47 index 11). Bands
	// then holds the per-band mean of the two, for the chart and for the
	// checks that need one curve. Entries without it behave as before.
	Ears      *EarCurves `json:"ears,omitempty"`
	Note      string     `json:"note"`
	CreatedAt string     `json:"createdAt"`
}

// EarBands is the number of bands per ear of a hearing-test result.
const EarBands = 8

// EarCurves is the per-ear curve pair of a hearing-test result.
type EarCurves struct {
	Left       []EqBand `json:"left"`
	Right      []EqBand `json:"right"`
	MasterGain float64  `json:"masterGain"` // dB, one value for both ears
	// Baseline says how the frames are built when the card is re-applied:
	// absent/"factory" = the 8 test bands as they are; "zero" = EXPERIMENT,
	// 13 bands per ear with the 5 factory bands at gain 0 first.
	Baseline string `json:"baseline,omitempty"`
}

// LibraryDir returns the directory curves are stored in.
func LibraryDir() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "library"), nil
}

var slugFold = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o", "ö", "o",
	"ú", "u", "ù", "u", "û", "u", "ü", "u",
	"ç", "c", "ñ", "n",
)

// Slug turns a display name into a valid curve id: lowercase ASCII letters and
// digits, runs of anything else collapsed to one "-", at most 40 characters.
// A name with nothing usable yields "curve".
func Slug(name string) string {
	s := slugFold.Replace(strings.ToLower(strings.TrimSpace(name)))
	var b strings.Builder
	dash := true // swallow leading separators
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	out = strings.Trim(out, "-")
	if out == "" {
		return "curve"
	}
	return out
}

// quantCenti converts dB to centi-dB within +/-max and rejects values that are
// not a multiple of 0.01 dB (they could not survive the int16 payload).
func quantCenti(db, max float64) (int, error) {
	c, err := centiInRange(db, max)
	if err != nil {
		return 0, err
	}
	v := db * 100
	if math.Abs(v-math.Round(v)) > 1e-6 {
		return 0, fmt.Errorf("gain %v dB is not a multiple of 0.01 dB", db)
	}
	return c, nil
}

func validSource(s string) bool {
	return s == SourceUser || s == SourceDeviceImport || s == SourceFactorySnapshot
}

func checkText(label, s string, max int, allowNewline bool) error {
	if utf8.RuneCountInString(s) > max {
		return fmt.Errorf("%s is longer than %d characters", label, max)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", label)
	}
	for _, r := range s {
		if allowNewline && r == '\n' {
			continue
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", label)
		}
	}
	return nil
}

// ValidateCurve checks every rule of the v1 format. It does not touch disk.
func ValidateCurve(c Curve) error {
	if c.Version != CurveVersion {
		return fmt.Errorf("curve: unsupported version %d (this build reads %d)", c.Version, CurveVersion)
	}
	if !curveIDRe.MatchString(c.ID) {
		return fmt.Errorf("curve: invalid id %q (want %s)", c.ID, curveIDRe)
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("curve: name is empty")
	}
	if err := checkText("curve name", c.Name, curveNameMax, false); err != nil {
		return err
	}
	if err := checkText("curve note", c.Note, curveNoteMax, true); err != nil {
		return err
	}
	if !validSource(c.Source) {
		return fmt.Errorf("curve: unknown source %q", c.Source)
	}
	if c.Slot < -1 || c.Slot > 255 {
		return fmt.Errorf("curve: slot %d out of range (-1..255)", c.Slot)
	}
	if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
		return fmt.Errorf("curve: createdAt: %w", err)
	}
	masterC, err := quantCenti(c.MasterGain, MasterMaxDB)
	if err != nil {
		return fmt.Errorf("curve: masterGain: %w", err)
	}
	if len(c.Bands) > EqMaxBands {
		return fmt.Errorf("curve: %d bands (max %d)", len(c.Bands), EqMaxBands)
	}
	prev := -1
	bandC := make([]int, len(c.Bands))
	for i, b := range c.Bands {
		if b.Index < 0 || b.Index >= EqMaxBands {
			return fmt.Errorf("curve: band %d: index %d out of range (0..%d)", i, b.Index, EqMaxBands-1)
		}
		if b.Index <= prev {
			return fmt.Errorf("curve: band %d: index %d is duplicated or out of order", i, b.Index)
		}
		prev = b.Index
		if b.Freq < minFreqHz || b.Freq > maxFreqHz {
			return fmt.Errorf("curve: band %d: freq %d Hz out of range (%d..%d)", i, b.Freq, minFreqHz, maxFreqHz)
		}
		g, err := quantCenti(b.Gain, MaxDB)
		if err != nil {
			return fmt.Errorf("curve: band %d: %w", i, err)
		}
		bandC[i] = g
		if b.Q < 0 || b.Q > math.MaxUint16 {
			return fmt.Errorf("curve: band %d: q %d out of range", i, b.Q)
		}
		if b.Type < 0 || b.Type > math.MaxUint8 {
			return fmt.Errorf("curve: band %d: type %d out of range", i, b.Type)
		}
	}

	if c.Ears != nil {
		if err := validateEars(c.Ears); err != nil {
			return err
		}
	}

	if c.Payload == "" {
		if c.Source != SourceUser {
			return fmt.Errorf("curve: source %q needs a payload", c.Source)
		}
		return nil
	}
	p, err := hex.DecodeString(c.Payload)
	if err != nil {
		return fmt.Errorf("curve: payload: %w", err)
	}
	if len(p) > EqReadLen {
		return fmt.Errorf("curve: payload is %d bytes (max %d)", len(p), EqReadLen)
	}
	if err := ValidateEq(p); err != nil {
		return fmt.Errorf("curve: payload: %w", err)
	}
	snap, err := DecodeEq(p)
	if err != nil {
		return fmt.Errorf("curve: payload: %w", err)
	}
	if masterC != masterCenti(p) {
		return fmt.Errorf("curve: masterGain %v does not match the payload (%v)", c.MasterGain, snap.MasterGain)
	}
	if len(snap.Bands) != len(c.Bands) {
		return fmt.Errorf("curve: %d bands do not match the payload (%d)", len(c.Bands), len(snap.Bands))
	}
	for i, d := range snap.Bands {
		b := c.Bands[i]
		if b.Index != d.Index || b.Freq != d.Freq || bandC[i] != int(math.Round(d.Gain*100)) || b.Q != d.Q || b.Type != d.Type {
			return fmt.Errorf("curve: band %d does not match the payload", i)
		}
	}
	return nil
}

// validateEars checks the per-ear curves: EarBands bands each, in increasing
// position, with frequency, gain, Q and type in the ranges of a payload.
func validateEars(e *EarCurves) error {
	if e.Baseline != "" && e.Baseline != "factory" && e.Baseline != "zero" {
		return fmt.Errorf("curve: ears.baseline %q is not factory or zero", e.Baseline)
	}
	if _, err := quantCenti(e.MasterGain, MasterMaxDB); err != nil {
		return fmt.Errorf("curve: ears: masterGain: %w", err)
	}
	for _, side := range []struct {
		name  string
		bands []EqBand
	}{{"left", e.Left}, {"right", e.Right}} {
		if len(side.bands) != EarBands {
			return fmt.Errorf("curve: ears.%s has %d bands (want %d)", side.name, len(side.bands), EarBands)
		}
		prev := -1
		for i, b := range side.bands {
			switch {
			case b.Index < 0 || b.Index >= EqMaxBands:
				return fmt.Errorf("curve: ears.%s[%d]: index %d out of range", side.name, i, b.Index)
			case b.Index <= prev:
				return fmt.Errorf("curve: ears.%s[%d]: index %d is duplicated or out of order", side.name, i, b.Index)
			case b.Freq < minFreqHz || b.Freq > maxFreqHz:
				return fmt.Errorf("curve: ears.%s[%d]: freq %d Hz out of range", side.name, i, b.Freq)
			case b.Q < 0 || b.Q > math.MaxUint16:
				return fmt.Errorf("curve: ears.%s[%d]: q %d out of range", side.name, i, b.Q)
			case !BandTypeSupported(b.Type):
				return fmt.Errorf("curve: ears.%s[%d]: type %d is not supported", side.name, i, b.Type)
			}
			prev = b.Index
			if _, err := quantCenti(b.Gain, MaxDB); err != nil {
				return fmt.Errorf("curve: ears.%s[%d]: %w", side.name, i, err)
			}
		}
	}
	return nil
}

// Applyable reports whether the curve can be written to the earbuds: every
// band must have a type the device has shown (BandTypes: 0, 2, 3, 255) and sit
// at a position the payload has (0-19). Bands at positions 10-19 do not make a
// curve unapplyable: slot 255 takes the 143-byte format without a flag, and any
// other target refuses it in ApplyEq (needs --full-length). It is derived and
// never persisted.
func (c Curve) Applyable() (bool, string) {
	for _, b := range c.Bands {
		if b.Index < 0 || b.Index >= EqMaxBands {
			return false, fmt.Sprintf("band at position %d (the payload has positions 0-%d)", b.Index, EqMaxBands-1)
		}
		if !BandTypeSupported(b.Type) {
			return false, fmt.Sprintf("band %d has type %d (supported: 0, 2, 3, 255)", b.Index, b.Type)
		}
	}
	return true, ""
}

func decodeCurve(data []byte) (Curve, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Curve{}, fmt.Errorf("curve: %w", err)
	}
	if head.Version != CurveVersion {
		return Curve{}, fmt.Errorf("curve: unsupported version %d (this build reads %d)", head.Version, CurveVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Curve
	if err := dec.Decode(&c); err != nil {
		return Curve{}, fmt.Errorf("curve: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Curve{}, errors.New("curve: trailing data after the JSON object")
	}
	if c.Bands == nil {
		c.Bands = []EqBand{}
	}
	if err := ValidateCurve(c); err != nil {
		return Curve{}, err
	}
	return c, nil
}

// readCurveFile reads a regular file of at most MaxCurveFileSize bytes without
// following a symlink at path.
func readCurveFile(path string) ([]byte, error) {
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
	data, err := io.ReadAll(io.LimitReader(f, MaxCurveFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCurveFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, MaxCurveFileSize)
	}
	return data, nil
}

func curvePath(id string) (string, error) {
	if !curveIDRe.MatchString(id) {
		return "", fmt.Errorf("curve: invalid id %q", id)
	}
	dir, err := LibraryDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// LoadCurve reads and validates the curve with the given id.
func LoadCurve(id string) (Curve, error) {
	p, err := curvePath(id)
	if err != nil {
		return Curve{}, err
	}
	data, err := readCurveFile(p)
	if err != nil {
		return Curve{}, fmt.Errorf("curve %q: %w", id, err)
	}
	c, err := decodeCurve(data)
	if err != nil {
		return Curve{}, fmt.Errorf("curve %q: %w", id, err)
	}
	if c.ID != id {
		return Curve{}, fmt.Errorf("curve %q: file holds id %q", id, c.ID)
	}
	return c, nil
}

// ListCurves returns every valid curve, sorted by id, plus one error per file
// that could not be used. Temp files, subdirectories and symlinks are skipped.
func ListCurves() ([]Curve, []error) {
	dir, err := LibraryDir()
	if err != nil {
		return nil, []error{err}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []Curve
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || !e.Type().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		c, err := LoadCurve(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errs
}

func ensureLibraryDir() (string, error) {
	dir, err := LibraryDir()
	if err != nil {
		return "", err
	}
	if st, err := os.Lstat(dir); err == nil && !st.IsDir() {
		return "", fmt.Errorf("library dir %s is not a directory", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("library dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("library dir: %w", err)
	}
	return dir, nil
}

func prepareCurve(c Curve) (Curve, []byte, error) {
	if c.Version == 0 {
		c.Version = CurveVersion
	}
	if c.CreatedAt == "" {
		c.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if c.Bands == nil {
		c.Bands = []EqBand{}
	}
	if err := ValidateCurve(c); err != nil {
		return Curve{}, nil, err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return Curve{}, nil, err
	}
	return c, append(data, '\n'), nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// CreateCurve stores a new curve. It fails (wrapping os.ErrExist) when the id
// is already taken and never replaces an existing file.
func CreateCurve(c Curve) error {
	c, data, err := prepareCurve(c)
	if err != nil {
		return err
	}
	dir, err := ensureLibraryDir()
	if err != nil {
		return err
	}
	if err := writeNoReplace(filepath.Join(dir, c.ID+".json"), data); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("curve %q already exists: %w", c.ID, os.ErrExist)
		}
		return fmt.Errorf("curve %q: %w", c.ID, err)
	}
	return nil
}

// UpdateCurve replaces an existing curve (temp file, fsync, rename). It fails
// when the id does not exist or is not a regular file.
func UpdateCurve(c Curve) error {
	c, data, err := prepareCurve(c)
	if err != nil {
		return err
	}
	dir, err := ensureLibraryDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, c.ID+".json")
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("curve %q: %w", c.ID, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("curve %q: not a regular file", c.ID)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-curve-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// UpsertCurve stores c under its id, replacing an existing file ATOMICALLY
// (temp file + rename: the entry never disappears). The version it replaces
// is first linked into <library>/.trash/<id>-<UTC timestamp>.json, so nothing
// is lost. replaced reports whether there was a previous version.
func UpsertCurve(c Curve) (replaced bool, err error) {
	c, _, err = prepareCurve(c)
	if err != nil {
		return false, err
	}
	dir, err := ensureLibraryDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, c.ID+".json")
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, CreateCurve(c)
	}
	if err != nil {
		return false, fmt.Errorf("curve %q: %w", c.ID, err)
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("curve %q: not a regular file", c.ID)
	}
	trash := filepath.Join(dir, ".trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return false, fmt.Errorf("curve trash: %w", err)
	}
	ts := time.Now().UTC().Format("20060102T150405.000000Z")
	linked := false
	for n := 0; n < 100 && !linked; n++ {
		name := fmt.Sprintf("%s-%s.json", c.ID, ts)
		if n > 0 {
			name = fmt.Sprintf("%s-%s-%d.json", c.ID, ts, n)
		}
		err := os.Link(path, filepath.Join(trash, name))
		switch {
		case err == nil:
			linked = true
		case !errors.Is(err, os.ErrExist):
			return false, fmt.Errorf("curve trash: %w", err)
		}
	}
	if !linked {
		return false, errors.New("curve trash: could not find a free file name")
	}
	syncDir(trash)
	return true, UpdateCurve(c)
}

// DeleteCurve moves the curve to <library>/.trash/<id>-<UTC timestamp>.json.
// Nothing is erased.
func DeleteCurve(id string) error {
	path, err := curvePath(id)
	if err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("curve %q: %w", id, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("curve %q: not a regular file", id)
	}
	dir := filepath.Dir(path)
	trash := filepath.Join(dir, ".trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return fmt.Errorf("curve trash: %w", err)
	}
	ts := time.Now().UTC().Format("20060102T150405.000000Z")
	for n := 0; n < 100; n++ {
		name := fmt.Sprintf("%s-%s.json", id, ts)
		if n > 0 {
			name = fmt.Sprintf("%s-%s-%d.json", id, ts, n)
		}
		// Link then unlink: unlike rename, Link refuses to overwrite.
		err := os.Link(path, filepath.Join(trash, name))
		if err == nil {
			if err := os.Remove(path); err != nil {
				return err
			}
			syncDir(trash)
			syncDir(dir)
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("curve trash: %w", err)
		}
	}
	return errors.New("curve trash: could not find a free file name")
}

// ImportCurve reads a curve file from path (at most 64 KiB, symlinks refused)
// and adds it to the library. An id that is already there is an error.
func ImportCurve(path string) (Curve, error) {
	data, err := readCurveFile(path)
	if err != nil {
		return Curve{}, fmt.Errorf("import: %w", err)
	}
	c, err := decodeCurve(data)
	if err != nil {
		return Curve{}, fmt.Errorf("import: %w", err)
	}
	if err := CreateCurve(c); err != nil {
		return Curve{}, fmt.Errorf("import: %w", err)
	}
	return c, nil
}

// ExportCurve writes the curve to path (0600). It refuses to overwrite an
// existing file.
func ExportCurve(id, path string) error {
	c, err := LoadCurve(id)
	if err != nil {
		return err
	}
	_, data, err := prepareCurve(c)
	if err != nil {
		return err
	}
	if err := writeNoReplace(path, data); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("export: %s already exists: %w", path, os.ErrExist)
		}
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// SeedFromPayload builds a curve from a raw 0x22 payload (hex) and stores it in
// the library under Slug(name). source is device-import or factory-snapshot
// (user is accepted too); slot is the slot it came from, or -1.
func SeedFromPayload(source string, slot int, name, hexPayload string) (Curve, error) {
	p, err := hex.DecodeString(strings.TrimSpace(hexPayload))
	if err != nil {
		return Curve{}, fmt.Errorf("seed: payload: %w", err)
	}
	if err := ValidateEq(p); err != nil {
		return Curve{}, fmt.Errorf("seed: %w", err)
	}
	snap, err := DecodeEq(p)
	if err != nil {
		return Curve{}, fmt.Errorf("seed: %w", err)
	}
	c := Curve{
		Version:    CurveVersion,
		ID:         Slug(name),
		Name:       strings.TrimSpace(name),
		Source:     source,
		Slot:       slot,
		MasterGain: snap.MasterGain,
		Payload:    snap.Payload,
		Bands:      snap.Bands,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := CreateCurve(c); err != nil {
		return Curve{}, err
	}
	return c, nil
}
