package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
)

// slot0Flat is built in init: 143 bytes, slot 0, master 0, bands at the flat
// table (31..16000 Hz, q 100, type 0) with band 0 at -8 dB, tail zero.
var slot0Flat = func() string {
	p := make([]byte, qcycore.EqReadLen)
	for i, f := range defaultBandTable {
		o := 3 + i*7
		p[o], p[o+1] = byte(f), byte(f>>8)
		p[o+4] = 100
	}
	p[5], p[6] = 0xe0, 0xfc
	return hex.EncodeToString(p)
}()

const tenFlat = "0,0,0,0,0,0,0,0,0,0"

// captureStdout runs fn and returns what it printed to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

func libEnv(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	return data
}

// fakeLibAPI is the daemonAPI the apply/import-device paths see; only eqRead
// and eqApply are implemented (the embedded nil interface panics on the rest).
type fakeLibAPI struct {
	daemonAPI
	link *fakeEqLink
}

func (f *fakeLibAPI) eqRead() (*qcycore.EqSnapshot, error) {
	s, err := qcycore.DecodeEq(f.link.state)
	return &s, err
}
func (f *fakeLibAPI) eqApply(op qcycore.EqOp) (*qcycore.EqResult, error) {
	return eqApplyVia(f.link, op)
}

// useFakeLink replaces the runner; the returned counter is how many times a
// link was opened.
func useFakeLink(t *testing.T, state []byte) (*fakeEqLink, *int) {
	t.Helper()
	l := &fakeEqLink{state: state}
	opened := 0
	old := libraryRunner
	libraryRunner = func(_ int, fn func(daemonAPI) error) error {
		opened++
		return fn(&fakeLibAPI{link: l})
	}
	t.Cleanup(func() { libraryRunner = old })
	return l, &opened
}

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	return mustPayload(t, s)
}

func mustSave(t *testing.T, args ...string) {
	t.Helper()
	if err := cmdLibrary(append([]string{"save"}, args...)); err != nil {
		t.Fatalf("save %v: %v", args, err)
	}
}

func TestLibraryListOrderAndCorruptFile(t *testing.T) {
	libEnv(t)
	mustSave(t, "Zeta", "--gains", tenFlat)
	mustSave(t, "alpha", "--gains", tenFlat)
	if _, err := qcycore.SeedFromPayload(qcycore.SourceFactorySnapshot, 6, "Clássico", realClassico6); err != nil {
		t.Fatal(err)
	}
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 0, "Beta", slot0Flat); err != nil {
		t.Fatal(err)
	}
	dir, _ := qcycore.LibraryDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureStdout(t, func() { err = cmdLibrary([]string{"list", "--json"}) })
	if err != nil {
		t.Fatalf("list failed on a corrupt file: %v", err)
	}
	var got []struct {
		ID        string `json:"id"`
		Applyable bool   `json:"applyable"`
		NBands    int    `json:"nBands"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	// factory first, then yours (by name), then the other device imports
	if strings.Join(ids, ",") != "classico,alpha,zeta,beta" {
		t.Fatalf("order = %v", ids)
	}
	if !got[1].Applyable || got[1].NBands != 10 {
		t.Fatalf("%+v", got[1])
	}
}

func listIDs(t *testing.T, extra ...string) []string {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = cmdLibrary(append([]string{"list", "--json"}, extra...)) })
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	ids := []string{}
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	return ids
}

// The app's order: Predefinido, Popular, Bass pesados, Rock, Suave, Clássico,
// Agudos suaves, Som espacial; then Experiência auditiva; then the user's own;
// the old test curve (slot 0) after them. The older seeds a new curve replaces
// are hidden, and --all shows them.
func TestLibraryListFactoryOrderHidesSuperseded(t *testing.T) {
	data := libEnv(t)
	bk := filepath.Join(data, "qcy-omarchy", "backups")
	if err := os.MkdirAll(bk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bk, "eq-slot11-first.hex"), []byte(realCustom11), 0o600); err != nil {
		t.Fatal(err)
	}
	mustSave(t, "Zz meu", "--gains", tenFlat)
	mustSave(t, "Aa meu", "--gains", tenFlat)
	// what the previous seed left in the user's library
	for _, o := range []struct {
		src  string
		slot int
		name string
		hex  string
	}{
		{qcycore.SourceFactorySnapshot, 6, "Clássico (slot 6)", realClassico6},
		{qcycore.SourceDeviceImport, 11, "Slot 11 (primeira leitura)", realCustom11},
		{qcycore.SourceDeviceImport, 0, "Slot 0 (primeira leitura)", slot0Flat},
	} {
		if _, err := qcycore.SeedFromPayload(o.src, o.slot, o.name, o.hex); err != nil {
			t.Fatal(err)
		}
	}
	// before the new curves exist nothing is hidden
	if ids := listIDs(t); strings.Join(ids, ",") != "aa-meu,zz-meu,classico-slot-6,slot-0-primeira-leitura,slot-11-primeira-leitura" {
		t.Fatalf("before seed: %v", ids)
	}
	if _, err := seedLibrary(); err != nil {
		t.Fatal(err)
	}
	want := "predefinido,popular,bass-pesados,rock,suave,classico,agudos-suaves,som-espacial,experiencia-auditiva,aa-meu,zz-meu,slot-0-primeira-leitura"
	if ids := listIDs(t); strings.Join(ids, ",") != want {
		t.Fatalf("default list = %v", ids)
	}
	all := listIDs(t, "--all")
	if len(all) != 14 || !strings.Contains(strings.Join(all, ","), "classico-slot-6") || !strings.Contains(strings.Join(all, ","), "slot-11-primeira-leitura") {
		t.Fatalf("--all = %v", all)
	}
	// a hidden curve stays usable by id
	if err := cmdLibrary([]string{"show", "classico-slot-6"}); err != nil {
		t.Fatal(err)
	}
}

func TestLibrarySaveNoReplace(t *testing.T) {
	libEnv(t)
	mustSave(t, "Meu Som", "--gains", "1,2,3,4,5,6,7,-1,-2,-3", "--master", "-1.5", "--note", "n")
	c, err := qcycore.LoadCurve("meu-som")
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != qcycore.SourceUser || len(c.Bands) != 10 || c.Bands[0].Freq != 31 || c.Bands[9].Freq != 16000 ||
		c.Bands[0].Q != 100 || c.Bands[2].Gain != 3 || c.MasterGain != -1.5 || c.Slot != -1 {
		t.Fatalf("%+v", c)
	}
	err = cmdLibrary([]string{"save", "Meu Som", "--gains", tenFlat})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("second save: %v", err)
	}
	if again, _ := qcycore.LoadCurve("meu-som"); again.Bands[2].Gain != 3 {
		t.Fatal("no-replace violated")
	}
	if err := cmdLibrary([]string{"save", "Meu Som", "--gains", tenFlat, "--replace"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := qcycore.LoadCurve("meu-som"); again.Bands[2].Gain != 0 || again.CreatedAt != c.CreatedAt {
		t.Fatalf("%+v", again)
	}
	// --replace only touches source=user
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 0, "Meu Som 2", slot0Flat); err != nil {
		t.Fatal(err)
	}
	if err := cmdLibrary([]string{"save", "Meu Som 2", "--gains", tenFlat, "--replace"}); err == nil {
		t.Fatal("replaced a device-import curve")
	}
	// bad input
	for _, args := range [][]string{
		{"save", "x"},
		{"save", "x", "--gains", "1,2"},
		{"save", "x", "--gains", "0,0,0,0,0,0,0,0,0,12.01"},
		{"save", "x", "--gains", tenFlat, "--master", "13"},
		{"save", "!!!", "--gains", tenFlat, "--bogus"},
	} {
		if err := cmdLibrary(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestLibraryDeleteGoesToTrash(t *testing.T) {
	libEnv(t)
	mustSave(t, "Gone", "--gains", tenFlat)
	if err := cmdLibrary([]string{"delete", "gone"}); err == nil {
		t.Fatal("delete without --yes accepted")
	}
	if _, err := qcycore.LoadCurve("gone"); err != nil {
		t.Fatal(err)
	}
	if err := cmdLibrary([]string{"delete", "gone", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := qcycore.LoadCurve("gone"); err == nil {
		t.Fatal("still loadable")
	}
	dir, _ := qcycore.LibraryDir()
	ents, err := os.ReadDir(filepath.Join(dir, ".trash"))
	if err != nil || len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "gone-") {
		t.Fatalf("%v %v", ents, err)
	}
}

func TestLibraryExportImportRoundTrip(t *testing.T) {
	libEnv(t)
	mustSave(t, "Rt", "--gains", "1,0,0,0,0,0,0,0,0,-1")
	f := filepath.Join(t.TempDir(), "rt.json")
	if err := cmdLibrary([]string{"export", "rt", f}); err != nil {
		t.Fatal(err)
	}
	if err := cmdLibrary([]string{"export", "rt", f}); err == nil {
		t.Fatal("export overwrote a file")
	}
	if err := cmdLibrary([]string{"import", f}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("import over an existing id: %v", err)
	}
	if err := cmdLibrary([]string{"delete", "rt", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdLibrary([]string{"import", f}); err != nil {
		t.Fatal(err)
	}
}

func TestLibrarySeedIdempotent(t *testing.T) {
	data := libEnv(t)
	bk := filepath.Join(data, "qcy-omarchy", "backups")
	if err := os.MkdirAll(filepath.Join(bk, "preset-map"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := func(rel, s string) {
		if err := os.WriteFile(filepath.Join(bk, rel), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w("eq-slot11-first.hex", realCustom11+"\n")
	w("eq-slot11-20261011T205722.870031Z.hex", slot0Flat) // history: ignored
	w("preset-map/v3-182503-slot6.json", `{"slot":6,"junk":{"a":[1,2]},"payload":"`+realClassico6+`"}`)
	w("preset-map/log-v3.txt", "ignored")
	w("preset-map/v3-999999-slot6.json", "not json") // the old seed read these; now ignored

	run := func() seedReport {
		var rep seedReport
		var err error
		out := captureStdout(t, func() { err = cmdLibrary([]string{"seed", "--json"}) })
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return rep
	}
	first := run()
	var created []string
	for _, c := range first.Created {
		created = append(created, c.ID)
	}
	wantIDs := "predefinido,popular,bass-pesados,rock,suave,classico,agudos-suaves,som-espacial,experiencia-auditiva"
	if strings.Join(created, ",") != wantIDs || len(first.Errors) != 0 || len(first.Skipped) != 0 {
		t.Fatalf("%+v", first)
	}
	second := run()
	if len(second.Created) != 0 || len(second.Skipped) != 9 || len(second.Errors) != 0 {
		t.Fatalf("not idempotent: %+v", second)
	}
	cs, errs := qcycore.ListCurves()
	if len(cs) != 9 || len(errs) != 0 {
		t.Fatalf("%d curves, %v", len(cs), errs)
	}
	for _, c := range cs {
		if ok, why := c.Applyable(); !ok {
			t.Errorf("%s not applyable: %s", c.ID, why)
		}
		if c.ID == "experiencia-auditiva" {
			if c.Name != "Experiência auditiva" || c.Source != qcycore.SourceDeviceImport || c.Slot != 11 || c.Payload != realCustom11 {
				t.Errorf("%+v", c)
			}
			continue
		}
		if c.Source != qcycore.SourceFactorySnapshot {
			t.Errorf("%s source %s", c.ID, c.Source)
		}
	}
	c, err := qcycore.LoadCurve("classico")
	if err != nil || c.Name != "Clássico" || c.Slot != 6 || c.Payload != realClassico6 {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = qcycore.LoadCurve("som-espacial")
	if err != nil || c.Name != "Som espacial" || c.Slot != 10 {
		t.Fatalf("%+v %v", c, err)
	}
}

// The factory curves are embedded: they are seeded with no backup directory
// at all, and the hearing-test curve is reported as skipped with the reason.
func TestLibrarySeedWithoutBackups(t *testing.T) {
	libEnv(t)
	rep, err := seedLibrary()
	if err != nil || len(rep.Created) != 8 || len(rep.Errors) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].ID != "experiencia-auditiva" || !strings.Contains(rep.Skipped[0].Reason, "eq-slot11-first.hex") {
		t.Fatalf("%+v", rep.Skipped)
	}
}

// With no eq-slot11-first.hex the hearing-test curve comes from the preset-map
// JSON; other first-read hex files keep the older names, and the slots that the
// embedded data already covers are not seeded under the old "(slot N)" names.
func TestLibrarySeedHearingTestFromJSONAndOtherSlots(t *testing.T) {
	data := libEnv(t)
	bk := filepath.Join(data, "qcy-omarchy", "backups")
	if err := os.MkdirAll(filepath.Join(bk, "preset-map"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := func(rel, s string) {
		if err := os.WriteFile(filepath.Join(bk, rel), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w("preset-map/experiencia-auditiva-slot11-135311.json", `{"slot":11,"payload":"`+realCustom11+`"}`)
	w("eq-slot0-first.hex", slot0Flat)
	w("eq-slot1-first.hex", realClassico6) // factory slot: covered by the embedded data
	rep, err := seedLibrary()
	if err != nil || len(rep.Errors) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	ids := map[string]bool{}
	for _, c := range rep.Created {
		ids[c.ID] = true
	}
	if len(rep.Created) != 10 || !ids["experiencia-auditiva"] || !ids["slot-0-primeira-leitura"] || ids["predefinido-slot-1"] {
		t.Fatalf("%+v", rep.Created)
	}
}

// A library seeded by an older version keeps its old id and gets no duplicate
// for the slots the legacy path still covers (here slot 0).
func TestLibrarySeedKeepsLegacyID(t *testing.T) {
	data := libEnv(t)
	bk := filepath.Join(data, "qcy-omarchy", "backups")
	if err := os.MkdirAll(bk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bk, "eq-slot0-first.hex"), []byte(slot0Flat), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 0, "Slot 0 (primeira leitura)", slot0Flat); err != nil {
		t.Fatal(err)
	}
	rep, err := seedLibrary()
	if err != nil || len(rep.Created) != 8 {
		t.Fatalf("%+v %v", rep, err)
	}
	for _, c := range rep.Created {
		if c.ID == "slot-0-primeira-leitura" {
			t.Fatal("legacy curve duplicated")
		}
	}
	cs, _ := qcycore.ListCurves()
	if len(cs) != 9 {
		t.Fatalf("%d curves", len(cs))
	}
}

// Seeding never renames or replaces an existing curve, not even one of the
// user's with the id of a factory effect.
func TestLibrarySeedKeepsExisting(t *testing.T) {
	libEnv(t)
	mustSave(t, "Rock", "--gains", "1,0,0,0,0,0,0,0,0,0")
	rep, err := seedLibrary()
	if err != nil || len(rep.Created) != 7 {
		t.Fatalf("%+v %v", rep, err)
	}
	c, err := qcycore.LoadCurve("rock")
	if err != nil || c.Source != qcycore.SourceUser || c.Bands[0].Gain != 1 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLibraryApplyRefusesNonApplyableBeforeAnyLink(t *testing.T) {
	libEnv(t)
	l, opened := useFakeLink(t, hexBytes(t, slot0Flat))
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, "Tail", realCustom11); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []int{1, 7} {
		id := "odd" + strconv.Itoa(typ)
		err := qcycore.CreateCurve(qcycore.Curve{
			ID: id, Name: "Odd", Source: qcycore.SourceUser, Slot: -1,
			Bands: []qcycore.EqBand{{Index: 0, Freq: 100, Gain: 1, Q: 100, Type: typ}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// unsupported band type: refused for either slot, even with --full-length
		for _, args := range [][]string{{"apply", id, "--yes"}, {"apply", id, "--yes", "--full-length", "--slot", "0"}} {
			err = cmdLibrary(args)
			if err == nil || !strings.Contains(err.Error(), "type "+strconv.Itoa(typ)) {
				t.Fatalf("%v: %v", args, err)
			}
		}
	}
	// positions 10-19 to slot 0 without --full-length: refused, and says why
	err := cmdLibrary([]string{"apply", "tail", "--yes", "--slot", "0"})
	if err == nil || !errors.Is(err, qcycore.ErrFullLengthRequired) ||
		!strings.Contains(err.Error(), "143") || !strings.Contains(err.Error(), "NOT been proven") || !strings.Contains(err.Error(), "--full-length") {
		t.Fatalf("tail to slot 0 without --full-length: %v", err)
	}
	if err := cmdLibrary([]string{"apply", "odd1"}); err == nil {
		t.Fatal("apply without --yes accepted")
	}
	if err := cmdLibrary([]string{"apply", "missing", "--yes"}); err == nil {
		t.Fatal("unknown id accepted")
	}
	if *opened != 0 || len(l.writes) != 0 {
		t.Fatalf("link opened %d times, %d writes", *opened, len(l.writes))
	}
}

// Slot 255 takes the 143-byte format with no flag, as the phone app writes it,
// with the device's 0xFF type marker in every empty position.
func TestLibraryApplyTailToSlot255Writes143Automatically(t *testing.T) {
	libEnv(t)
	l, opened := useFakeLink(t, hexBytes(t, slot0Flat))
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, "Tail", realCustom11); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureStdout(t, func() { err = cmdLibrary([]string{"apply", "tail", "--yes", "--json"}) })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if *opened != 1 || len(l.writes) != 1 || len(l.writes[0]) != qcycore.EqReadLen || l.writes[0][0] != 0xff {
		t.Fatalf("writes = %x", l.writes)
	}
	w := l.writes[0]
	for i := 13; i < qcycore.EqMaxBands; i++ { // slot 11 has bands at positions 0-12
		if pos := w[3+i*7 : 3+(i+1)*7]; hex.EncodeToString(pos) != "000000000000ff" {
			t.Errorf("position %d = %x", i, pos)
		}
	}
	var res struct {
		Confirmed bool `json:"confirmed"`
		Wrote     struct {
			Slot       int  `json:"slot"`
			FullLength bool `json:"fullLength"`
		} `json:"wrote"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Confirmed || res.Wrote.Slot != 255 || !res.Wrote.FullLength {
		t.Fatalf("%v %+v: %s", err, res, out)
	}
}

func TestLibraryApplyProtectedActiveSlotRefusedByApplyEq(t *testing.T) {
	data := libEnv(t)
	l, opened := useFakeLink(t, hexBytes(t, realClassico6)) // active slot 6
	mustSave(t, "Ten", "--gains", tenFlat)
	err := cmdLibrary([]string{"apply", "ten", "--yes", "--slot", "0"})
	if !errors.Is(err, qcycore.ErrProtectedSlot) {
		t.Fatalf("err = %v", err)
	}
	if *opened != 1 || len(l.writes) != 0 || backupFiles(t, data) != 0 {
		t.Fatalf("opened %d, writes %d, backups %d", *opened, len(l.writes), backupFiles(t, data))
	}
}

func TestLibraryApplyDefaultsToSlot255EvenWithProtectedActive(t *testing.T) {
	libEnv(t)
	l, _ := useFakeLink(t, hexBytes(t, realClassico6)) // active slot 6
	mustSave(t, "Ten", "--gains", tenFlat)
	if err := cmdLibrary([]string{"apply", "ten", "--yes"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(l.writes) != 1 || len(l.writes[0]) != qcycore.EqLen || l.writes[0][0] != 0xFF {
		t.Fatalf("writes = %x", l.writes)
	}
}

func TestLibraryApplyTenBandsWrites73BytesToSlot0(t *testing.T) {
	data := libEnv(t)
	state := hexBytes(t, slot0Flat)
	state[0] = 3 // active slot 3, not protected
	l, _ := useFakeLink(t, state)
	mustSave(t, "Ten", "--gains", "1,2,3,4,5,6,7,8,-8,0", "--master", "2")
	var err error
	out := captureStdout(t, func() { err = cmdLibrary([]string{"apply", "ten", "--yes", "--slot", "0", "--json"}) })
	if len(l.writes) != 1 || len(l.writes[0]) != qcycore.EqLen || l.writes[0][0] != 0 {
		t.Fatalf("writes = %x (err %v)", l.writes, err)
	}
	w := l.writes[0]
	if w[1] != 0xc8 || w[2] != 0x00 { // +2.00 dB master = 200 centi-dB
		t.Fatalf("master bytes %x", w[1:3])
	}
	if backupFiles(t, data) == 0 {
		t.Fatal("no backup before the write")
	}
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var res struct {
		ID        string `json:"id"`
		Confirmed bool   `json:"confirmed"`
		Backup    string `json:"backup"`
		Wrote     struct {
			Slot       int                         `json:"slot"`
			FullLength bool                        `json:"fullLength"`
			Bands      []struct{ Index, Freq int } `json:"bands"`
		} `json:"wrote"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !res.Confirmed || res.Backup == "" || res.Wrote.Slot != 0 || res.Wrote.FullLength || len(res.Wrote.Bands) != 10 {
		t.Fatalf("%+v", res)
	}
}

func TestLibraryApplyUnconfirmedReportsFalse(t *testing.T) {
	libEnv(t)
	state := hexBytes(t, slot0Flat)
	state[0] = 3
	l, _ := useFakeLink(t, state)
	l.echo = state // device keeps reporting slot 3
	mustSave(t, "Ten", "--gains", tenFlat)
	var err error
	out := captureStdout(t, func() { err = cmdLibrary([]string{"apply", "ten", "--yes", "--slot", "0", "--json"}) })
	if !errors.Is(err, qcycore.ErrUnconfirmed) {
		t.Fatalf("err = %v", err)
	}
	var res struct {
		Confirmed bool   `json:"confirmed"`
		Code      string `json:"code"`
		Backup    string `json:"backup"`
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil || res.Confirmed || res.Code != "unconfirmed" || res.Backup == "" {
		t.Fatalf("%v %+v %s", jerr, res, out)
	}
}

func TestLibraryApplyFullLengthWritesWholePayload(t *testing.T) {
	libEnv(t)
	l, _ := useFakeLink(t, hexBytes(t, slot0Flat))
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, "Tail", realCustom11); err != nil {
		t.Fatal(err)
	}
	if err := cmdLibrary([]string{"apply", "tail", "--yes", "--full-length", "--slot", "0"}); err != nil {
		t.Fatal(err)
	}
	if len(l.writes) != 1 || len(l.writes[0]) != qcycore.EqReadLen || l.writes[0][0] != 0 {
		t.Fatalf("writes = %x", l.writes)
	}
}

func TestLibraryImportDevice(t *testing.T) {
	libEnv(t)
	useFakeLink(t, hexBytes(t, slot0Flat))
	if err := cmdLibrary([]string{"import-device", "Do Fone"}); err != nil {
		t.Fatal(err)
	}
	c, err := qcycore.LoadCurve("do-fone")
	if err != nil || c.Source != qcycore.SourceDeviceImport || c.Slot != 0 || c.Payload != slot0Flat {
		t.Fatalf("%+v %v", c, err)
	}
	if err := cmdLibrary([]string{"import-device", "Do Fone"}); err == nil {
		t.Fatal("second import replaced")
	}
}

func TestLibraryUsageErrors(t *testing.T) {
	libEnv(t)
	for _, args := range [][]string{nil, {"nope"}, {"show"}, {"list", "extra"}, {"export", "a"}, {"import"}} {
		if err := cmdLibrary(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

// useWireDaemon makes libraryRunner hand out a real *daemonClient whose peer
// answers every request with reply (one JSON line, the shape qcyctld sends).
func useWireDaemon(t *testing.T, reply string) {
	t.Helper()
	old := libraryRunner
	libraryRunner = func(_ int, fn func(daemonAPI) error) error {
		c, s := net.Pipe()
		go func() {
			defer s.Close()
			r := bufio.NewReader(s)
			for {
				if _, err := r.ReadString('\n'); err != nil {
					return
				}
				if _, err := s.Write([]byte(reply + "\n")); err != nil {
					return
				}
			}
		}()
		dc := &daemonClient{conn: c, enc: json.NewEncoder(c), dec: json.NewDecoder(c)}
		defer dc.Close()
		return fn(dc)
	}
	t.Cleanup(func() { libraryRunner = old })
}

// Through the daemon the typed EqError survives: code, sent, deviceReported
// and backup reach errors.Is/As and the --json output.
func TestLibraryApplyViaDaemonUnconfirmedKeepsTypedError(t *testing.T) {
	libEnv(t)
	mustSave(t, "Ten", "--gains", tenFlat)
	useWireDaemon(t, `{"ok":false,"error":"eq: written but unconfirmed: x","code":"unconfirmed","sent":"00c800","deviceReported":"0b50","backup":"/b/history.hex"}`)
	var err error
	out := captureStdout(t, func() { err = cmdLibrary([]string{"apply", "ten", "--yes", "--slot", "0", "--json"}) })
	var ee *qcycore.EqError
	if !errors.Is(err, qcycore.ErrUnconfirmed) || !errors.As(err, &ee) {
		t.Fatalf("err = %v", err)
	}
	if hex.EncodeToString(ee.Sent) != "00c800" || hex.EncodeToString(ee.Got) != "0b50" || ee.Backup != "/b/history.hex" {
		t.Fatalf("fields lost: %+v", ee)
	}
	var got map[string]interface{}
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("output %q: %v", out, jerr)
	}
	if got["code"] != "unconfirmed" || got["sent"] != "00c800" || got["deviceReported"] != "0b50" || got["backup"] != "/b/history.hex" {
		t.Fatalf("json = %v", got)
	}
}

func TestDaemonClientRequestKeepsEqCodes(t *testing.T) {
	cases := map[string]error{
		"protected_slot":       qcycore.ErrProtectedSlot,
		"full_length_required": qcycore.ErrFullLengthRequired,
	}
	for code, want := range cases {
		c, s := net.Pipe()
		go func() {
			r := bufio.NewReader(s)
			_, _ = r.ReadString('\n')
			_, _ = s.Write([]byte(`{"ok":false,"error":"refused","code":"` + code + `"}` + "\n"))
		}()
		dc := &daemonClient{conn: c, enc: json.NewEncoder(c), dec: json.NewDecoder(c)}
		_, err := dc.request("eqApply", nil)
		dc.Close()
		s.Close()
		if !errors.Is(err, want) || err.Error() != "refused" {
			t.Errorf("%s: err = %v", code, err)
		}
	}
}

func TestLibraryApplySlot255Writes73BytesWithExplicitMaster(t *testing.T) {
	libEnv(t)
	state := hexBytes(t, slot0Flat)
	state[0] = 3
	l, _ := useFakeLink(t, state)
	mustSave(t, "Ten", "--gains", "1,2,3,4,5,6,7,8,-8,12", "--master", "2")
	var err error
	out := captureStdout(t, func() { err = cmdLibrary([]string{"apply", "ten", "--yes", "--slot", "255", "--json"}) })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(l.writes) != 1 || len(l.writes[0]) != qcycore.EqLen || l.writes[0][0] != 0xFF {
		t.Fatalf("writes = %x", l.writes)
	}
	if w := l.writes[0]; w[1] != 0xc8 || w[2] != 0x00 {
		t.Fatalf("master bytes %x", w[1:3])
	}
	var res struct {
		Confirmed bool `json:"confirmed"`
		Wrote     struct {
			Slot int `json:"slot"`
		} `json:"wrote"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Confirmed || res.Wrote.Slot != 255 {
		t.Fatalf("%v %+v: %s", err, res, out)
	}
}

func TestLibraryApplyRefusesOtherSlotsBeforeConnecting(t *testing.T) {
	libEnv(t)
	l, opened := useFakeLink(t, hexBytes(t, slot0Flat))
	mustSave(t, "Ten", "--gains", tenFlat)
	for _, s := range []string{"7", "6", "11", "1", "-1", "256", "x"} {
		if err := cmdLibrary([]string{"apply", "ten", "--yes", "--slot", s}); err == nil {
			t.Errorf("--slot %s accepted", s)
		}
	}
	if *opened != 0 || len(l.writes) != 0 {
		t.Fatalf("opened %d, writes %d", *opened, len(l.writes))
	}
}

// ---------- compose ----------

type composeDry struct {
	Base       string  `json:"base"`
	Slot       int     `json:"slot"`
	MasterGain float64 `json:"masterGain"`
	NBands     int     `json:"nBands"`
	FullLength bool    `json:"fullLength"`
	Payload    string  `json:"payload"`
	Bands      []struct {
		Index int     `json:"index"`
		Freq  int     `json:"freq"`
		Gain  float64 `json:"gain"`
		Q     int     `json:"q"`
		Type  int     `json:"type"`
	} `json:"bands"`
}

func composeDryRun(t *testing.T, args ...string) (composeDry, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		err = cmdLibrary(append([]string{"compose", "--dry-run", "--json"}, args...))
	})
	var d composeDry
	if err == nil {
		if jerr := json.Unmarshal([]byte(out), &d); jerr != nil {
			t.Fatalf("%v: %s", jerr, out)
		}
	}
	return d, err
}

func seeded(t *testing.T) (l *fakeEqLink, opened *int) {
	t.Helper()
	libEnv(t)
	l, opened = useFakeLink(t, hexBytes(t, realClassico6)) // active slot 6, protected
	if rep, err := seedLibrary(); err != nil || len(rep.Created) != 8 {
		t.Fatalf("%+v %v", rep, err)
	}
	return l, opened
}

// The app's layout: the non-zero bars first (ISO frequency, Q 100, type 255),
// then the base effect's bands in their original order, 73 bytes while that
// fits in positions 0-9. Predefinido has 6 bands, three bars are moved.
func TestLibraryComposeDryRunExactLayout(t *testing.T) {
	l, opened := seeded(t)
	d, err := composeDryRun(t, "--base", "predefinido", "--gains", "0,0,3,0,0,-2,0,0,0,4.5")
	if err != nil {
		t.Fatal(err)
	}
	const want = "ff50fb7d002c016400ffe80338ff6400ff803ec2016400ffc8007cfc46000284039cff460002f00ac800780002342144fd5e0102581b9cff780003983a38ff90010200000000000000"
	if d.Payload != want {
		t.Fatalf("payload\n got %s\nwant %s", d.Payload, want)
	}
	if d.Base != "predefinido" || d.Slot != 255 || d.MasterGain != -12 || d.NBands != 9 || d.FullLength || len(d.Bands) != 9 {
		t.Fatalf("%+v", d)
	}
	wantBands := []struct {
		freq int
		gain float64
		q, t int
	}{
		{125, 3, 100, 255}, {1000, -2, 100, 255}, {16000, 4.5, 100, 255}, // the bars, in bar order
		{200, -9, 70, 2}, {900, -1, 70, 2}, {2800, 2, 120, 2}, {8500, -7, 350, 2}, {7000, -1, 120, 3}, {15000, -2, 400, 2}, // base, original order
	}
	for i, w := range wantBands {
		b := d.Bands[i]
		if b.Index != i || b.Freq != w.freq || b.Gain != w.gain || b.Q != w.q || b.Type != w.t {
			t.Errorf("band %d = %+v, want %+v", i, b, w)
		}
	}
	// a dry run connects to nothing and writes nothing
	if *opened != 0 || len(l.writes) != 0 {
		t.Fatalf("opened %d, writes %d", *opened, len(l.writes))
	}
}

// 3 bars + the 13-band hearing test = 16 bands: 143 bytes, the marker in the
// empty positions, and the base's master gain by default.
func TestLibraryComposeDryRunOverTenBands(t *testing.T) {
	seeded(t)
	// the hearing test is not in this sandbox's backups: import it by hand
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, qcycore.HearingTestName, realCustom11); err != nil {
		t.Fatal(err)
	}
	d, err := composeDryRun(t, "--base", "experiencia-auditiva", "--gains", "-4,0,0,6,0,0,0,8,0,0")
	if err != nil {
		t.Fatal(err)
	}
	const want = "ff50fb1f0070fe6400fffa0058026400ffa00f20036400ff28006400370002c800d4fe410002840332004b0002f00afa006e0002401f38ff2c01023f0096005000027d00ceff500002fa006400500002f4016aff500002e8033200500002d007c800500002a00f9cff500002401f2c015a0002000000000000ff000000000000ff000000000000ff000000000000ff"
	if d.Payload != want || d.NBands != 16 || !d.FullLength || d.MasterGain != -12 {
		t.Fatalf("%+v\n got %s\nwant %s", d, d.Payload, want)
	}
	if d.Bands[0].Type != 255 || d.Bands[3].Freq != 40 || d.Bands[15].Index != 15 {
		t.Fatalf("%+v", d.Bands)
	}
}

func TestLibraryComposeBaseNoneAndMaster(t *testing.T) {
	seeded(t)
	d, err := composeDryRun(t, "--base", "none", "--gains", "0,0,0,0,0,0,0,0,0,1", "--master", "-3.5")
	if err != nil {
		t.Fatal(err)
	}
	// one bar at 16 kHz, +1 dB, Q 100, type 255, master -3.5 dB
	want := "ff" + "a2fe" + "803e" + "6400" + "6400" + "ff" + strings.Repeat("00", 73-10)
	if d.Payload != want || d.NBands != 1 || d.MasterGain != -3.5 || d.Base != "none" {
		t.Fatalf("%+v\n got %s\nwant %s", d, d.Payload, want)
	}
	// no --master and no base: -12 dB like every factory effect
	d, err = composeDryRun(t, "--base", "none", "--gains", "1,0,0,0,0,0,0,0,0,0")
	if err != nil || d.MasterGain != -12 {
		t.Fatalf("%+v %v", d, err)
	}
	// --master wins over the base's
	d, err = composeDryRun(t, "--base", "rock", "--gains", "1,0,0,0,0,0,0,0,0,0", "--master", "0")
	if err != nil || d.MasterGain != 0 {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestLibraryComposeRefusals(t *testing.T) {
	l, opened := seeded(t)
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, qcycore.HearingTestName, realCustom11); err != nil {
		t.Fatal(err)
	}
	odd := qcycore.Curve{ID: "odd", Name: "Odd", Source: qcycore.SourceUser, Slot: -1,
		Bands: []qcycore.EqBand{{Index: 0, Freq: 100, Gain: 1, Q: 100, Type: 1}}}
	if err := qcycore.CreateCurve(odd); err != nil {
		t.Fatal(err)
	}
	// 8 bars + 13 base bands = 21 > 20
	_, err := composeDryRun(t, "--base", "experiencia-auditiva", "--gains", "1,1,1,1,1,1,1,1,0,0")
	if err == nil || !strings.Contains(err.Error(), "21 bands") || !strings.Contains(err.Error(), "20") {
		t.Fatalf("21 bands: %v", err)
	}
	// 7 bars + 13 = 20 is exactly full
	d, err := composeDryRun(t, "--base", "experiencia-auditiva", "--gains", "1,1,1,1,1,1,1,0,0,0")
	if err != nil || d.NBands != 20 || len(d.Payload) != qcycore.EqReadLen*2 {
		t.Fatalf("20 bands: %+v %v", d, err)
	}
	for name, args := range map[string][]string{
		"nothing to write": {"--base", "none", "--gains", tenFlat},
		"gain above 12":    {"--base", "none", "--gains", "0,0,0,0,0,0,0,0,0,12.5"},
		"nine gains":       {"--base", "none", "--gains", "1,2,3,4,5,6,7,8,9"},
		"master above 12":  {"--base", "rock", "--gains", tenFlat, "--master", "13"},
		"unknown base":     {"--base", "nope", "--gains", "1,0,0,0,0,0,0,0,0,0"},
		"unsupported type": {"--base", "odd", "--gains", "1,0,0,0,0,0,0,0,0,0"},
		"no base":          {"--gains", "1,0,0,0,0,0,0,0,0,0"},
		"no gains":         {"--base", "rock"},
		"unknown flag":     {"--base", "rock", "--gains", tenFlat, "--slot", "3"},
	} {
		if _, err := composeDryRun(t, args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// a real write needs --yes and the checks happen before any link
	if err := cmdLibrary([]string{"compose", "--base", "rock", "--gains", "1,0,0,0,0,0,0,0,0,0"}); err == nil {
		t.Error("compose without --yes or --dry-run accepted")
	}
	if err := cmdLibrary([]string{"compose", "--base", "experiencia-auditiva", "--gains", "1,1,1,1,1,1,1,1,0,0", "--yes"}); err == nil {
		t.Error("21 bands written")
	}
	if *opened != 0 || len(l.writes) != 0 {
		t.Fatalf("opened %d, writes %d", *opened, len(l.writes))
	}
}

// compose writes through the same path as apply: the active slot (6, protected)
// is backed up, slot 255 is the target, the echo is compared, and a device that
// keeps reporting another slot is "unconfirmed".
func TestLibraryComposeWritesSlot255ViaApplyPath(t *testing.T) {
	data := libEnv(t)
	l, opened := useFakeLink(t, hexBytes(t, realClassico6))
	if rep, err := seedLibrary(); err != nil || len(rep.Created) != 8 {
		t.Fatalf("%+v %v", rep, err)
	}
	var err error
	out := captureStdout(t, func() {
		err = cmdLibrary([]string{"compose", "--base", "predefinido", "--gains", "0,0,3,0,0,-2,0,0,0,4.5", "--yes", "--json"})
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if *opened != 1 || len(l.writes) != 1 {
		t.Fatalf("opened %d writes %d", *opened, len(l.writes))
	}
	const want = "ff50fb7d002c016400ffe80338ff6400ff803ec2016400ffc8007cfc46000284039cff460002f00ac800780002342144fd5e0102581b9cff780003983a38ff90010200000000000000"
	if got := hex.EncodeToString(l.writes[0]); got != want {
		t.Fatalf("wrote %s", got)
	}
	if backupFiles(t, data) == 0 {
		t.Fatal("no backup before the write")
	}
	var res struct {
		ID        string `json:"id"`
		Base      string `json:"base"`
		Confirmed bool   `json:"confirmed"`
		Backup    string `json:"backup"`
		Wrote     struct {
			Slot       int  `json:"slot"`
			FullLength bool `json:"fullLength"`
			Bands      []struct {
				Index, Freq, Type int
			} `json:"bands"`
		} `json:"wrote"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !res.Confirmed || res.Backup == "" || res.Base != "predefinido" || res.Wrote.Slot != 255 || res.Wrote.FullLength || len(res.Wrote.Bands) != 9 || res.Wrote.Bands[0].Type != 255 {
		t.Fatalf("%+v", res)
	}

	// the device keeps reporting slot 6: written, unconfirmed
	l.echo = hexBytes(t, realClassico6)
	err = cmdLibrary([]string{"compose", "--base", "predefinido", "--gains", "1,0,0,0,0,0,0,0,0,0", "--yes"})
	if !errors.Is(err, qcycore.ErrUnconfirmed) {
		t.Fatalf("err = %v", err)
	}
}

// 143 bytes to slot 255 through compose, confirmed with the device's marker
// echo; never AllowProtected: a --slot flag does not exist.
func TestLibraryComposeOverTenBandsWrites143(t *testing.T) {
	libEnv(t)
	l, _ := useFakeLink(t, hexBytes(t, realCustom11)) // active slot 11, protected, populated tail
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, qcycore.HearingTestName, realCustom11); err != nil {
		t.Fatal(err)
	}
	if err := cmdLibrary([]string{"compose", "--base", "experiencia-auditiva", "--gains", "-4,0,0,6,0,0,0,8,0,0", "--yes"}); err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(l.writes) != 1 || len(l.writes[0]) != qcycore.EqReadLen || l.writes[0][0] != 0xff {
		t.Fatalf("writes = %x", l.writes)
	}
	if err := cmdLibrary([]string{"compose", "--base", "experiencia-auditiva", "--gains", "-4,0,0,6,0,0,0,8,0,0", "--yes", "--slot", "0"}); err == nil {
		t.Fatal("compose accepted --slot")
	}
}

// save --base stores the bars on top of the base the way compose writes them:
// the non-zero bars first (type 255), then the base's own bands, so applying
// the saved curve plays what compose played.
func TestLibrarySaveWithBaseKeepsBaseBands(t *testing.T) {
	libEnv(t)
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 0, "Base", slot0Flat); err != nil {
		t.Fatal(err)
	}
	base, err := qcycore.LoadCurve("base")
	if err != nil {
		t.Fatal(err)
	}
	mustSave(t, "Ajustado", "--base", "base", "--gains", "0,0,0,0,0,0,0,3,0,0", "--master", "-6")
	c, err := qcycore.LoadCurve("ajustado")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Bands) != 1+len(base.Bands) || c.MasterGain != -6 {
		t.Fatalf("%d bands (base %d), master %v", len(c.Bands), len(base.Bands), c.MasterGain)
	}
	if b := c.Bands[0]; b.Freq != 4000 || b.Gain != 3 || b.Type != composeUserBandType {
		t.Fatalf("bar band: %+v", b)
	}
	if c.Bands[1].Freq != base.Bands[0].Freq {
		t.Fatalf("base band lost: %+v", c.Bands[1])
	}
}
