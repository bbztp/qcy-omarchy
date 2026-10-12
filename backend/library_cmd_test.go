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
	if strings.Join(ids, ",") != "alpha,zeta,beta,classico" {
		t.Fatalf("order = %v", ids)
	}
	if !got[0].Applyable || got[0].NBands != 10 {
		t.Fatalf("%+v", got[0])
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
	w("eq-slot0-first.hex", slot0Flat+"\n")
	w("eq-slot11-first.hex", realCustom11)
	w("eq-slot0-20261011T205722.870031Z.hex", slot0Flat) // history: ignored
	w("preset-map/v3-182503-slot6.json", `{"slot":6,"junk":{"a":[1,2]},"payload":"`+realClassico6+`"}`)
	w("preset-map/log-v3.txt", "ignored")
	w("preset-map/v3-999999-slot6.json", "not json") // reported, never fatal

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
	if len(first.Created) != 3 || len(first.Errors) != 1 {
		t.Fatalf("%+v", first)
	}
	second := run()
	if len(second.Created) != 0 || len(second.Skipped) != 3 {
		t.Fatalf("not idempotent: %+v", second)
	}
	cs, errs := qcycore.ListCurves()
	if len(cs) != 3 || len(errs) != 0 {
		t.Fatalf("%d curves, %v", len(cs), errs)
	}
	c, err := qcycore.LoadCurve("classico-slot-6")
	if err != nil || c.Source != qcycore.SourceFactorySnapshot || c.Slot != 6 || c.Name != "Clássico (slot 6)" {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = qcycore.LoadCurve("experiencia-auditiva-slot-11")
	if err != nil || c.Name != "Experiência auditiva (slot 11)" || c.Source != qcycore.SourceDeviceImport || c.Slot != 11 {
		t.Fatalf("%+v %v", c, err)
	}
	if ok, _ := c.Applyable(); ok {
		t.Fatal("slot 11 has a tail and must not be applyable")
	}
}

// A library seeded by an older version keeps its old id and gets no duplicate.
func TestLibrarySeedKeepsLegacyID(t *testing.T) {
	data := libEnv(t)
	bk := filepath.Join(data, "qcy-omarchy", "backups")
	if err := os.MkdirAll(bk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bk, "eq-slot11-first.hex"), []byte(realCustom11), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, "Slot 11 (primeira leitura)", realCustom11); err != nil {
		t.Fatal(err)
	}
	rep, err := seedLibrary()
	if err != nil || len(rep.Created) != 0 || len(rep.Skipped) != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := qcycore.LoadCurve("slot-11-primeira-leitura"); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryApplyRefusesNonApplyableBeforeAnyLink(t *testing.T) {
	libEnv(t)
	l, opened := useFakeLink(t, hexBytes(t, slot0Flat))
	if _, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, 11, "Tail", realCustom11); err != nil {
		t.Fatal(err)
	}
	err := qcycore.CreateCurve(qcycore.Curve{
		ID: "odd", Name: "Odd", Source: qcycore.SourceUser, Slot: -1,
		Bands: []qcycore.EqBand{{Index: 0, Freq: 100, Gain: 1, Q: 100, Type: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 10-19 without --full-length: refused, and says why
	err = cmdLibrary([]string{"apply", "tail", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "143") || !strings.Contains(err.Error(), "NOT been proven") {
		t.Fatalf("tail without --full-length: %v", err)
	}
	// unsupported band type: refused even with --full-length
	err = cmdLibrary([]string{"apply", "odd", "--yes", "--full-length"})
	if err == nil || !strings.Contains(err.Error(), "type 1") {
		t.Fatalf("odd: %v", err)
	}
	if err := cmdLibrary([]string{"apply", "odd"}); err == nil {
		t.Fatal("apply without --yes accepted")
	}
	if err := cmdLibrary([]string{"apply", "missing", "--yes"}); err == nil {
		t.Fatal("unknown id accepted")
	}
	if *opened != 0 || len(l.writes) != 0 {
		t.Fatalf("link opened %d times, %d writes", *opened, len(l.writes))
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
