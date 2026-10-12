package qcycore

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func libEnv(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("XDG_DATA_HOME", d)
	dir, err := LibraryDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(d, "qcy-omarchy", "library") {
		t.Fatalf("LibraryDir = %s", dir)
	}
	return dir
}

func userCurve(id string) Curve {
	return Curve{
		Version: 1, ID: id, Name: "Mine", Source: SourceUser, Slot: -1,
		MasterGain: 1.5,
		Bands:      []EqBand{{Index: 0, Freq: 100, Gain: 1.5, Q: 50, Type: 2}, {Index: 3, Freq: 1000, Gain: -2.25, Q: 70, Type: 0}},
		CreatedAt:  "2026-10-11T10:00:00Z",
	}
}

func writeRaw(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDataDirAndBackupDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/x/data")
	d, _ := DataDir()
	b, _ := BackupDir()
	if d != "/x/data/qcy-omarchy" || b != "/x/data/qcy-omarchy/backups" {
		t.Fatalf("DataDir=%s BackupDir=%s", d, b)
	}
	t.Setenv("XDG_DATA_HOME", "relative")
	t.Setenv("HOME", "/h")
	d, _ = DataDir()
	if d != "/h/.local/share/qcy-omarchy" {
		t.Fatalf("DataDir=%s", d)
	}
}

func TestSlug(t *testing.T) {
	long := strings.Repeat("ab ", 30)
	for in, want := range map[string]string{
		"Clássico 6":            "classico-6",
		"  My  Curve!! ":        "my-curve",
		"":                      "curve",
		"!!!":                   "curve",
		"日本語":                   "curve",
		"a_b/../c":              "a-b-c",
		strings.TrimSpace(long): "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab",
	} {
		got := Slug(in)
		if len(got) > 40 || !curveIDRe.MatchString(got) {
			t.Errorf("Slug(%q)=%q invalid", in, got)
		}
		if in != strings.TrimSpace(long) && got != want {
			t.Errorf("Slug(%q)=%q want %q", in, got, want)
		}
	}
}

func TestCurveRoundtripLossless(t *testing.T) {
	for _, tc := range []struct {
		name, hex, source string
		slot              int
		applyable         bool
	}{
		{"Classico 6", realClassico6, SourceFactorySnapshot, 6, true},
		{"Custom 11", realCustom11, SourceDeviceImport, 11, true}, // positions 10-12 are fine now: slot 255 takes 143 bytes
	} {
		t.Run(tc.name, func(t *testing.T) {
			libEnv(t)
			c, err := SeedFromPayload(tc.source, tc.slot, tc.name, tc.hex)
			if err != nil {
				t.Fatal(err)
			}
			got, err := LoadCurve(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c) {
				t.Fatalf("load != seeded:\n%+v\n%+v", got, c)
			}
			if got.Payload != tc.hex {
				t.Fatal("payload changed")
			}
			if ok, why := got.Applyable(); ok != tc.applyable {
				t.Fatalf("Applyable=%v (%s)", ok, why)
			}
			if !tc.applyable {
				if _, why := got.Applyable(); why == "" {
					t.Fatal("no reason given")
				}
			}
			// Persisted bytes carry no derived field.
			dir, _ := LibraryDir()
			raw, _ := os.ReadFile(filepath.Join(dir, c.ID+".json"))
			if strings.Contains(string(raw), "applyable") {
				t.Fatal("applyable persisted")
			}
			// Perms.
			if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
				t.Fatalf("dir mode %v", st.Mode().Perm())
			}
			if st, _ := os.Stat(filepath.Join(dir, c.ID+".json")); st.Mode().Perm() != 0o600 {
				t.Fatalf("file mode %v", st.Mode().Perm())
			}
			// Re-encode from the decoded fields is identical to the payload's decode.
			snap, _ := DecodeEq(mustHex(t, tc.hex))
			if !reflect.DeepEqual(snap.Bands, got.Bands) || snap.MasterGain != got.MasterGain {
				t.Fatal("bands/master drifted from payload")
			}
		})
	}
}

func TestCreateNoReplace(t *testing.T) {
	dir := libEnv(t)
	if err := CreateCurve(userCurve("mine")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "mine.json"))
	other := userCurve("mine")
	other.Name = "Other"
	err := CreateCurve(other)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("err=%v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "mine.json"))
	if string(before) != string(after) {
		t.Fatal("existing file was modified")
	}
	if _, err := SeedFromPayload(SourceDeviceImport, 6, "Mine", realClassico6); !errors.Is(err, os.ErrExist) {
		t.Fatalf("seed over existing: %v", err)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	dir := libEnv(t)
	if err := UpdateCurve(userCurve("ghost")); err == nil {
		t.Fatal("update of missing curve succeeded")
	}
	c := userCurve("mine")
	if err := CreateCurve(c); err != nil {
		t.Fatal(err)
	}
	c.Name = "Renamed"
	c.Bands[0].Gain = 3
	if err := UpdateCurve(c); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCurve("mine")
	if err != nil || got.Name != "Renamed" || got.Bands[0].Gain != 3 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if err := DeleteCurve("mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCurve("mine"); err == nil {
		t.Fatal("deleted curve still loads")
	}
	ents, err := os.ReadDir(filepath.Join(dir, ".trash"))
	if err != nil || len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "mine-") {
		t.Fatalf("trash=%v err=%v", ents, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".trash", ents[0].Name()))
	var back Curve
	if json.Unmarshal(data, &back) != nil || back.Name != "Renamed" {
		t.Fatal("trash content lost")
	}
	// The id is free again, and a second delete lands in its own trash file.
	if err := CreateCurve(userCurve("mine")); err != nil {
		t.Fatal(err)
	}
	if err := DeleteCurve("mine"); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, ".trash")); len(ents) != 2 {
		t.Fatalf("trash has %d entries", len(ents))
	}
	if err := DeleteCurve("mine"); err == nil {
		t.Fatal("delete of missing curve succeeded")
	}
	if err := DeleteCurve("../etc"); err == nil {
		t.Fatal("traversal id accepted")
	}
}

func TestListSkipsAndReportsBadFiles(t *testing.T) {
	dir := libEnv(t)
	if cs, errs := ListCurves(); len(cs) != 0 || len(errs) != 0 {
		t.Fatal("missing dir should list empty")
	}
	if err := CreateCurve(userCurve("b-ok")); err != nil {
		t.Fatal(err)
	}
	if err := CreateCurve(userCurve("a-ok")); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(filepath.Join(dir, "a-ok.json"))
	writeRaw(t, dir, "broken.json", []byte("{not json"))
	writeRaw(t, dir, ".tmp-backup-123", good)
	writeRaw(t, dir, ".tmp-curve-9.json", good)
	writeRaw(t, dir, "notes.txt", []byte("x"))
	writeRaw(t, dir, "wrong-id.json", good) // id inside is a-ok
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a-ok.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	cs, errs := ListCurves()
	if len(cs) != 2 || cs[0].ID != "a-ok" || cs[1].ID != "b-ok" {
		t.Fatalf("curves=%+v", cs)
	}
	if len(errs) != 2 {
		t.Fatalf("errs=%v", errs)
	}
}

func TestSymlinkNotFollowed(t *testing.T) {
	dir := libEnv(t)
	if err := CreateCurve(userCurve("real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real.json"), filepath.Join(dir, "via-link.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCurve("via-link"); err == nil {
		t.Fatal("symlink followed by Load")
	}
	if err := DeleteCurve("via-link"); err == nil {
		t.Fatal("symlink deleted/moved")
	}
	if err := UpdateCurve(userCurve("via-link")); err == nil {
		t.Fatal("symlink updated")
	}
	// Create over a dangling symlink must not write through it.
	target := filepath.Join(t.TempDir(), "victim.json")
	if err := os.Symlink(target, filepath.Join(dir, "dangling.json")); err != nil {
		t.Fatal(err)
	}
	if err := CreateCurve(userCurve("dangling")); err == nil {
		t.Fatal("create over dangling symlink succeeded")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("wrote through the symlink")
	}
	// Import of a symlink.
	src := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "real.json"), filepath.Join(src, "l.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportCurve(filepath.Join(src, "l.json")); err == nil {
		t.Fatal("import followed a symlink")
	}
}

func TestValidateCurveTable(t *testing.T) {
	mod := func(f func(c *Curve)) Curve {
		c := userCurve("mine")
		f(&c)
		return c
	}
	cls := mustHex(t, realClassico6)
	withPayload := func(f func(c *Curve)) Curve {
		s, _ := DecodeEq(cls)
		c := Curve{Version: 1, ID: "cls", Name: "Cls", Source: SourceDeviceImport, Slot: 6,
			MasterGain: s.MasterGain, Payload: s.Payload, Bands: s.Bands, CreatedAt: "2026-10-11T10:00:00Z"}
		f(&c)
		return c
	}
	manyBands := func(n int) []EqBand {
		var b []EqBand
		for i := 0; i < n; i++ {
			b = append(b, EqBand{Index: i, Freq: 100 + i, Gain: 0, Q: 50, Type: 2})
		}
		return b
	}
	cases := []struct {
		name string
		c    Curve
		ok   bool
	}{
		{"valid user", userCurve("mine"), true},
		{"valid payload", withPayload(func(*Curve) {}), true},
		{"20 bands", mod(func(c *Curve) { c.Bands = manyBands(20) }), true},
		{"21 bands", mod(func(c *Curve) { c.Bands = manyBands(21) }), false},
		{"no bands", mod(func(c *Curve) { c.Bands = nil }), true},
		{"master NaN", mod(func(c *Curve) { c.MasterGain = math.NaN() }), false},
		{"master Inf", mod(func(c *Curve) { c.MasterGain = math.Inf(1) }), false},
		{"master 12", mod(func(c *Curve) { c.MasterGain = 12 }), true},
		{"master 12.01", mod(func(c *Curve) { c.MasterGain = 12.01 }), false},
		{"master -12.01", mod(func(c *Curve) { c.MasterGain = -12.01 }), false},
		{"master 0.005", mod(func(c *Curve) { c.MasterGain = 0.005 }), false},
		{"gain NaN", mod(func(c *Curve) { c.Bands[0].Gain = math.NaN() }), false},
		{"gain -Inf", mod(func(c *Curve) { c.Bands[0].Gain = math.Inf(-1) }), false},
		{"gain 8", mod(func(c *Curve) { c.Bands[0].Gain = 8 }), true},
		{"gain 12", mod(func(c *Curve) { c.Bands[0].Gain = 12 }), true},
		{"gain -12", mod(func(c *Curve) { c.Bands[0].Gain = -12 }), true},
		{"gain 12.01", mod(func(c *Curve) { c.Bands[0].Gain = 12.01 }), false},
		{"gain -12.01", mod(func(c *Curve) { c.Bands[0].Gain = -12.01 }), false},
		{"gain 0.001", mod(func(c *Curve) { c.Bands[0].Gain = 0.001 }), false},
		{"freq 19", mod(func(c *Curve) { c.Bands[0].Freq = 19 }), false},
		{"freq 20", mod(func(c *Curve) { c.Bands[0].Freq = 20 }), true},
		{"freq 20000", mod(func(c *Curve) { c.Bands[0].Freq = 20000 }), true},
		{"freq 20001", mod(func(c *Curve) { c.Bands[0].Freq = 20001 }), false},
		{"q 65536", mod(func(c *Curve) { c.Bands[0].Q = 65536 }), false},
		{"q -1", mod(func(c *Curve) { c.Bands[0].Q = -1 }), false},
		{"type 256", mod(func(c *Curve) { c.Bands[0].Type = 256 }), false},
		{"index 20", mod(func(c *Curve) { c.Bands[1].Index = 20 }), false},
		{"index -1", mod(func(c *Curve) { c.Bands[0].Index = -1 }), false},
		{"index dup", mod(func(c *Curve) { c.Bands[1].Index = 0 }), false},
		{"index order", mod(func(c *Curve) { c.Bands[0].Index, c.Bands[1].Index = 5, 2 }), false},
		{"id empty", mod(func(c *Curve) { c.ID = "" }), false},
		{"id upper", mod(func(c *Curve) { c.ID = "Mine" }), false},
		{"id dash first", mod(func(c *Curve) { c.ID = "-mine" }), false},
		{"id underscore", mod(func(c *Curve) { c.ID = "a_b" }), false},
		{"id traversal", mod(func(c *Curve) { c.ID = "../x" }), false},
		{"id 40", mod(func(c *Curve) { c.ID = strings.Repeat("a", 40) }), true},
		{"id 41", mod(func(c *Curve) { c.ID = strings.Repeat("a", 41) }), false},
		{"name empty", mod(func(c *Curve) { c.Name = " " }), false},
		{"name control", mod(func(c *Curve) { c.Name = "a\x00b" }), false},
		{"note newline", mod(func(c *Curve) { c.Note = "a\nb" }), true},
		{"note control", mod(func(c *Curve) { c.Note = "a\x1bb" }), false},
		{"source bad", mod(func(c *Curve) { c.Source = "preset" }), false},
		{"import w/o payload", mod(func(c *Curve) { c.Source = SourceDeviceImport }), false},
		{"slot 256", mod(func(c *Curve) { c.Slot = 256 }), false},
		{"slot -2", mod(func(c *Curve) { c.Slot = -2 }), false},
		{"createdAt bad", mod(func(c *Curve) { c.CreatedAt = "yesterday" }), false},
		{"version 2", mod(func(c *Curve) { c.Version = 2 }), false},
		{"payload master mismatch", withPayload(func(c *Curve) { c.MasterGain = 1 }), false},
		{"payload band gain mismatch", withPayload(func(c *Curve) { c.Bands[0].Gain += 0.01 }), false},
		{"payload band missing", withPayload(func(c *Curve) { c.Bands = c.Bands[1:] }), false},
		{"payload band index mismatch", withPayload(func(c *Curve) { c.Bands[0].Index = 9 }), false},
		{"payload bad hex", withPayload(func(c *Curve) { c.Payload = "zz" }), false},
		{"payload short", withPayload(func(c *Curve) { c.Payload = c.Payload[:20] }), false},
		{"payload all FF", withPayload(func(c *Curve) { c.Payload = strings.Repeat("ff", EqReadLen) }), false},
		{"payload too long", withPayload(func(c *Curve) { c.Payload += "00" }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCurve(tc.c)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
	}
}

func TestApplyableTable(t *testing.T) {
	mod := func(f func(c *Curve)) Curve {
		c := userCurve("mine")
		f(&c)
		return c
	}
	for name, tc := range map[string]struct {
		c  Curve
		ok bool
	}{
		"plain":       {userCurve("mine"), true},
		"position 10": {mod(func(c *Curve) { c.Bands = append(c.Bands, EqBand{Index: 10, Freq: 900, Q: 50, Type: 2}) }), true},
		"position 19": {mod(func(c *Curve) { c.Bands = append(c.Bands, EqBand{Index: 19, Freq: 900, Q: 50, Type: 2}) }), true},
		"type 1":      {mod(func(c *Curve) { c.Bands[0].Type = 1 }), false},
		"type 7":      {mod(func(c *Curve) { c.Bands[0].Type = 7 }), false},
		"type 0":      {mod(func(c *Curve) { c.Bands[0].Type = 0 }), true},
		"type 2":      {mod(func(c *Curve) { c.Bands[0].Type = 2 }), true},
		"type 3":      {mod(func(c *Curve) { c.Bands[0].Type = 3 }), true},
		"type 255":    {mod(func(c *Curve) { c.Bands[0].Type = 255 }), true},
	} {
		if ok, why := tc.c.Applyable(); ok != tc.ok || (!ok && why == "") {
			t.Errorf("%s: ok=%v why=%q", name, ok, why)
		}
	}
}

func TestDecodeStrict(t *testing.T) {
	dir := libEnv(t)
	valid := func() map[string]any {
		b, _ := json.Marshal(userCurve("mine"))
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	enc := func(m map[string]any) []byte { b, _ := json.Marshal(m); return b }

	writeRaw(t, dir, "mine.json", enc(valid()))
	if _, err := LoadCurve("mine"); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	v2 := valid()
	v2["version"] = 2
	v2["futureField"] = true
	unknown := valid()
	unknown["applyable"] = true
	missingVersion := valid()
	delete(missingVersion, "version")
	wrongType := valid()
	wrongType["slot"] = "six"
	idMismatch := valid()
	idMismatch["id"] = "other"

	big := enc(valid())
	big = append(big, []byte(strings.Repeat(" ", MaxCurveFileSize))...)
	exact := enc(valid())
	exact = append(exact, []byte(strings.Repeat(" ", MaxCurveFileSize-len(exact)))...)

	for name, tc := range map[string]struct {
		data []byte
		ok   bool
	}{
		"version 2":       {enc(v2), false},
		"missing version": {enc(missingVersion), false},
		"unknown field":   {enc(unknown), false},
		"wrong type":      {enc(wrongType), false},
		"id mismatch":     {enc(idMismatch), false},
		"trailing data":   {append(enc(valid()), []byte(`{"x":1}`)...), false},
		"not json":        {[]byte("nope"), false},
		"empty":           {nil, false},
		"over 64 KiB":     {big, false},
		"exactly 64 KiB":  {exact, true},
	} {
		t.Run(name, func(t *testing.T) {
			writeRaw(t, dir, "mine.json", tc.data)
			_, err := LoadCurve("mine")
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
			if len(tc.data) > MaxCurveFileSize {
				out := filepath.Join(t.TempDir(), "x.json")
				if err := os.WriteFile(out, tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := ImportCurve(out); err == nil {
					t.Fatal("oversized import accepted")
				}
			}
		})
	}
}

func TestImportExport(t *testing.T) {
	libEnv(t)
	c, err := SeedFromPayload(SourceDeviceImport, 11, "Custom 11", realCustom11)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.json")
	if err := ExportCurve(c.ID, out); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("export mode %v", st.Mode().Perm())
	}
	if err := ExportCurve(c.ID, out); !errors.Is(err, os.ErrExist) {
		t.Fatalf("export over existing: %v", err)
	}
	if err := ExportCurve("nope", filepath.Join(t.TempDir(), "n.json")); err == nil {
		t.Fatal("export of missing id succeeded")
	}
	// Collision with the same library.
	if _, err := ImportCurve(out); !errors.Is(err, os.ErrExist) {
		t.Fatalf("import collision: %v", err)
	}
	// Import into a fresh library: identical curve.
	libEnv(t)
	got, err := ImportCurve(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("imported differs:\n%+v\n%+v", got, c)
	}
	if _, err := ImportCurve(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("import of missing file succeeded")
	}
}

func TestSeedFromPayloadRejects(t *testing.T) {
	libEnv(t)
	for name, tc := range map[string]struct{ source, hex string }{
		"bad hex":     {SourceDeviceImport, "xyz"},
		"all FF":      {SourceDeviceImport, strings.Repeat("ff", EqReadLen)},
		"short":       {SourceDeviceImport, realClassico6[:100]},
		"bad source":  {"preset", realClassico6},
		"over length": {SourceDeviceImport, realClassico6 + "00"},
	} {
		if _, err := SeedFromPayload(tc.source, 1, "X "+name, tc.hex); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if cs, _ := ListCurves(); len(cs) != 0 {
		t.Fatalf("rejected seeds left %d curves", len(cs))
	}
}

// Every embedded factory payload decodes, round-trips through the library
// format and is applyable (types 3 and 255 included).
func TestFactoryEffectsEmbedded(t *testing.T) {
	libEnv(t)
	wantOrder := []string{"Predefinido", "Popular", "Bass pesados", "Rock", "Suave", "Clássico", "Agudos suaves", "Som espacial"}
	wantSlot := []int{1, 2, 3, 4, 5, 6, 14, 10}
	if len(FactoryEffects) != len(wantOrder) {
		t.Fatalf("%d factory effects", len(FactoryEffects))
	}
	for i, e := range FactoryEffects {
		if e.Name != wantOrder[i] || e.Slot != wantSlot[i] {
			t.Errorf("entry %d = %d %q", i, e.Slot, e.Name)
		}
		if n, ok := EffectName(e.Slot); !ok || n != e.Name {
			t.Errorf("EffectName(%d) = %q", e.Slot, n)
		}
		p := mustHex(t, e.Payload)
		if int(p[0]) != e.Slot || masterCenti(p) != -1200 {
			t.Errorf("%s: slot byte %d master %d", e.Name, p[0], masterCenti(p))
		}
		c, err := SeedFromPayload(SourceFactorySnapshot, e.Slot, e.Name, e.Payload)
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		if c.ID != FactoryCurveID(e) || c.Payload != e.Payload {
			t.Errorf("%s: id %q", e.Name, c.ID)
		}
		if ok, why := c.Applyable(); !ok {
			t.Errorf("%s not applyable: %s", e.Name, why)
		}
	}
	// slot 10 is slot 1 with another byte 0, as measured
	if FactoryEffects[7].Payload[2:] != FactoryEffects[0].Payload[2:] {
		t.Error("Som espacial differs from Predefinido beyond byte 0")
	}
	// slot 1's band 4 is type 3, the one band type the old rule refused
	if s := mustDecode(t, mustHex(t, FactoryEffects[0].Payload)); s.Bands[4].Type != 3 {
		t.Errorf("Predefinido band 4 type = %d", s.Bands[4].Type)
	}
}

func TestSupersededCurveIDs(t *testing.T) {
	m := SupersededCurveIDs()
	for old, want := range map[string]string{
		"classico-slot-6":              "classico",
		"slot-11-primeira-leitura":     "experiencia-auditiva",
		"experiencia-auditiva-slot-11": "experiencia-auditiva",
		"predefinido-slot-1":           "predefinido",
		"som-espacial-slot-10":         "som-espacial",
	} {
		if m[old] != want {
			t.Errorf("%s -> %q, want %q", old, m[old], want)
		}
	}
	if _, ok := m["slot-0-primeira-leitura"]; ok {
		t.Error("slot 0 has no replacement")
	}
}
