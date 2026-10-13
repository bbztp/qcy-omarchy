package main

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/qcycore/models"
)

func intp(n int) *int { return &n }

// fnDaemon is a scripted daemon whose connected unit is the given vendorId,
// served by that vendorId's profile handler (the N70 keeps its own handler).
func fnDaemon(t *testing.T, vendor uint16, onWrite func(*scriptSession, byte, []byte)) (*daemon, *scriptSession) {
	t.Helper()
	fnSettleSaved, fnReadSaved, fnQSaved := fnSettle, fnReadbackTimeout, fnQueryTimeout
	fnSettle, fnReadbackTimeout, fnQueryTimeout = time.Millisecond, 600*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { fnSettle, fnReadbackTimeout, fnQueryTimeout = fnSettleSaved, fnReadSaved, fnQSaved })
	d, s := scriptDaemon(t, onWrite)
	d.mu.Lock()
	d.vendorID, d.haveVendor = vendor, true
	if vendor != 23872 && vendor != 23877 {
		ph, ok := newProfileHandler(vendor)
		if !ok {
			t.Fatalf("no profile for %d", vendor)
		}
		d.handler = ph
	}
	d.mu.Unlock()
	return d, s
}

func queriesOf(s *scriptSession, op byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.packets {
		if len(p) >= 5 && p[2] == 0xFE && p[4] == op {
			n++
		}
	}
	return n
}

func writesWithParams(s *scriptSession, op byte) [][]byte {
	var out [][]byte
	for _, p := range s.writesOf(op) {
		if p[2] == op && !(len(p) >= 5 && op == 0xFE) {
			out = append(out, p)
		}
	}
	return out
}

// echo makes the unit store what is written to op (the parameters).
func echo(ops ...byte) func(*scriptSession, byte, []byte) {
	return func(s *scriptSession, op byte, p []byte) {
		for _, o := range ops {
			if o == op && len(p) > 0 {
				s.set(op, append([]byte(nil), p...))
			}
		}
	}
}

func fnReq(args map[string]interface{}) request { return request{Cmd: "fnSet", Args: args} }

func listIDs(t *testing.T, d *daemon) map[int]fnItem {
	t.Helper()
	v, err := d.handleRequest(request{Cmd: "fnList"})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := v.([]fnItem)
	if !ok {
		t.Fatalf("fnList returned %T", v)
	}
	m := map[int]fnItem{}
	for _, it := range items {
		m[it.CmdID] = it
	}
	return m
}

func TestFnListN70DoesNotDuplicateSpecialisedControls(t *testing.T) {
	d, _ := fnDaemon(t, 23872, nil)
	m := listIDs(t, d)
	for _, covered := range []int{9, 16, 35, 36, 42, 44, 29, 20, 31, 17, 34, 23, 22, 72} {
		if _, ok := m[covered]; ok {
			t.Errorf("cmdId %d is covered by a specialised handler but listed", covered)
		}
	}
	for _, want := range []int{25, 1, 3} {
		if _, ok := m[want]; !ok {
			t.Errorf("cmdId %d missing from %v", want, m)
		}
	}
	if len(m) != 3 {
		t.Fatalf("N70 generic list has %d entries: %v", len(m), m)
	}
	if it := m[25]; it.Type != "list" || len(it.Options) != 2 || it.Options[0].Value != "57513030" {
		t.Errorf("tone item %+v", it)
	}
	if it := m[3]; it.Type != "action" || !it.Danger || !it.Confirm || it.Text == "" {
		t.Errorf("factory item %+v", it)
	}
}

func TestFnListEmptyWhenModelDeclaresNothingGeneric(t *testing.T) {
	d, s := scriptDaemon(t, nil)
	// A vendorId with no panel, and a unit whose vendorId is unknown.
	d.mu.Lock()
	d.vendorID, d.haveVendor = 1, true
	d.mu.Unlock()
	if m := listIDs(t, d); len(m) != 0 {
		t.Fatalf("unknown vendor lists %v", m)
	}
	d.mu.Lock()
	d.haveVendor = false
	d.mu.Unlock()
	if m := listIDs(t, d); len(m) != 0 {
		t.Fatalf("no vendor lists %v", m)
	}
	rep := mustStatus(t, d)
	if _, ok := rep["functions"]; ok {
		t.Fatalf("status carries functions: %v", rep["functions"])
	}
	if n := queriesOf(s, 0x19); n != 0 {
		t.Fatalf("%d function queries sent for a model without generic functions", n)
	}
}

func TestFnSetToggleSendsThePanelBytesAndConfirms(t *testing.T) {
	d, s := fnDaemon(t, 19797, echo(0x40))
	m := listIDs(t, d)
	if it := m[64]; it.Type != "toggle" || it.Label != "AdaptiveVolume" {
		t.Fatalf("toggle item %+v", it)
	}
	for _, c := range []struct {
		arg  string
		want []byte
		st   string
	}{{"on", []byte{0xFF, 0x03, 0x40, 0x01, 0x01}, "on"}, {"off", []byte{0xFF, 0x03, 0x40, 0x01, 0x00}, "off"}} {
		got, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(64), c.arg: true}))
		if err != nil {
			t.Fatal(err)
		}
		r := got.(*fnResult)
		if !r.Confirmed || r.State != c.st || r.Sent != hex.EncodeToString(c.want) {
			t.Errorf("%s: %+v", c.arg, r)
		}
		w := writesWithParams(s, 0x40)
		if !bytes.Equal(w[len(w)-1], c.want) {
			t.Errorf("%s wrote %x", c.arg, w[len(w)-1])
		}
	}
	// "value" accepts on/off words too.
	if _, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": "64", "value": "on"})); err != nil {
		t.Fatal(err)
	}
}

func TestFnSetListOptionByLabelAndByValue(t *testing.T) {
	d, s := fnDaemon(t, 19797, echo(0x33, 0x19))
	m := listIDs(t, d)
	opts := m[51].Options
	if len(opts) < 2 {
		t.Fatalf("choices item %+v", m[51])
	}
	for _, key := range []string{opts[1].Label, opts[1].Value, strings.ToLower(opts[1].Value)} {
		got, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(51), "value": key}))
		if err != nil {
			t.Fatalf("%q: %v", key, err)
		}
		r := got.(*fnResult)
		want := "FF0333" + "01" + opts[1].Value
		if !r.Confirmed || strings.ToUpper(r.Sent) != want || r.State != opts[1].Value {
			t.Errorf("%q: %+v want sent %s", key, r, want)
		}
	}
	if len(writesWithParams(s, 0x33)) != 3 {
		t.Errorf("writes: %d", len(writesWithParams(s, 0x33)))
	}
	if _, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(51), "value": "nope"})); err == nil {
		t.Error("an undeclared option was accepted")
	}
	// Tone language: 4 ASCII bytes.
	if _, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(25), "value": "4C583031"})); err != nil {
		t.Fatal(err)
	}
	if w := writesWithParams(s, 0x19); len(w) != 1 || !bytes.Equal(w[0], []byte{0xFF, 0x06, 0x19, 0x04, 0x4C, 0x58, 0x30, 0x31}) {
		t.Errorf("language frame %x", w)
	}
}

func TestFnSetRefusesUndeclaredAndSpecialisedFunctions(t *testing.T) {
	d, s := fnDaemon(t, 23872, nil)
	before := len(s.packets)
	for _, id := range []float64{9, 35, 42, 29, 200, 64} {
		if _, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": id, "on": true, "confirm": true})); err == nil {
			t.Errorf("cmdId %v accepted", id)
		}
	}
	if len(s.packets) != before {
		t.Errorf("%d frames reached the link", len(s.packets)-before)
	}
	for _, bad := range []interface{}{nil, -1.0, 300.0, "x", 1.5} {
		if _, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": bad})); err == nil {
			t.Errorf("cmdId %v accepted", bad)
		}
	}
}

func TestParsePanelFrameRefusesInvalidFrames(t *testing.T) {
	ok := []struct {
		h    string
		cmd  *int
		bare bool
		op   byte
		par  string
	}{
		{"FF03400101", intp(0x40), false, 0x40, "01"},
		{"400101", intp(0x40), false, 0x40, "01"},
		{"FF020100", nil, false, 0x01, ""},
		{"02", nil, true, 0x02, ""},
	}
	for _, c := range ok {
		op, p, err := parsePanelFrame(c.h, c.cmd, c.bare)
		if err != nil || op != c.op || hex.EncodeToString(p) != c.par {
			t.Errorf("%s: op %x par %x err %v", c.h, op, p, err)
		}
	}
	bad := []struct {
		h   string
		cmd *int
	}{
		{"FF03410101", intp(0x40)},   // opcode differs from the cmdId
		{"FF04400101", intp(0x40)},   // body longer than declared
		{"FF0340010101", intp(0x40)}, // extra bytes after the block
		{"FF03400201", intp(0x40)},   // paramLen disagrees
		{"FF", nil},
		{"FF03", nil},
		{"zz", nil},
		{"", nil},
		{"FF03FE0101", intp(0xFE)}, // reserved opcode
		{"02", nil},                // bare form only for commands
	}
	for _, c := range bad {
		if _, _, err := parsePanelFrame(c.h, c.cmd, false); err == nil {
			t.Errorf("%q accepted", c.h)
		}
	}
}

func TestFnRefusesInvalidPanelDeclarations(t *testing.T) {
	saved := fnFunctionsFor
	defer func() { fnFunctionsFor = saved }()
	tog := func(cmd int, on, off string) models.Function {
		return models.Function{CmdID: intp(cmd), Name: "x", ToggleBlock: &models.Toggle{On: []string{on}, Off: []string{off}}}
	}
	fnFunctionsFor = func(uint16) []models.Function {
		return []models.Function{
			tog(64, "FF03400101", "FF03400100"), // good
			tog(65, "FF03410101", "FF03400100"), // opcode != cmdId
			tog(66, "FF03420101", "FF04420100"), // inconsistent length
			tog(67, "FF03430101", "FF03430101"), // on == off
			tog(9, "FF03090101", "FF03090102"),  // specialised (game mode)
			tog(68, "FF03440101", "FF03440100"), // fine
			{CmdID: intp(70), Name: "Firmware update", ToggleBlock: &models.Toggle{On: []string{"FF03460101"}, Off: []string{"FF03460100"}}},
			{CmdID: intp(71), Name: "x", Character: "0000000F", ToggleBlock: &models.Toggle{On: []string{"FF03470101"}, Off: []string{"FF03470100"}}},
			{CmdID: intp(72), Name: "levels", ChoiceList: []models.Choice{{Label: "a", Value: intp(1)}}}, // no bytes: no frame declared
			{Name: "no id", ToggleBlock: &models.Toggle{On: []string{"01"}, Off: []string{"02"}}},
		}
	}
	got := genericFunctionsFor(1, false)
	if len(got) != 2 || got[0].CmdID != 64 || got[1].CmdID != 68 {
		t.Fatalf("kept %+v", got)
	}
}

func TestFnResetNeedsDoubleConfirm(t *testing.T) {
	d, s := fnDaemon(t, 23872, nil)
	for _, args := range []map[string]interface{}{
		{"cmdId": float64(1)},
		{"cmdId": float64(1), "confirm": true},
		{"cmdId": float64(3), "confirmAgain": true},
		{"cmdId": float64(3), "confirm": false, "confirmAgain": true},
	} {
		if _, err := d.handleRequest(fnReq(args)); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if w := writesWithParams(s, 0x01); len(w) != 0 {
		t.Fatalf("reset reached the link: %x", w)
	}
	got, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(1), "confirm": true, "confirmAgain": true}))
	if err != nil {
		t.Fatal(err)
	}
	if r := got.(*fnResult); r.Sent != "ff020100" {
		t.Errorf("%+v", r)
	}
	if w := writesWithParams(s, 0x01); len(w) != 1 || !bytes.Equal(w[0], []byte{0xFF, 0x02, 0x01, 0x00}) {
		t.Fatalf("reset frame %x", w)
	}
	if n := queriesOf(s, 0x01); n != 0 {
		t.Errorf("a command was queried %d times", n)
	}
}

func TestFnRestartDeclaredToleratesSilenceAndNeverRepeats(t *testing.T) {
	saved := fnFunctionsFor
	defer func() { fnFunctionsFor = saved }()
	fnFunctionsFor = func(uint16) []models.Function {
		return []models.Function{{CmdID: intp(77), Name: "Restarting thing",
			ToggleBlock:  &models.Toggle{On: []string{"FF034D0101"}, Off: []string{"FF034D0100"}},
			MessageBlock: &models.Messages{RebootTip: "The earbuds restart."}}}
	}
	d, s := fnDaemon(t, 19797, nil)
	// The unit keeps reporting the old value (off).
	s.set(0x4D, []byte{0x00})
	if _, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(77), "on": true})); err == nil {
		t.Fatal("restart function written without confirm")
	}
	if len(writesWithParams(s, 0x4D)) != 0 {
		t.Fatal("written without confirm")
	}
	got, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(77), "on": true, "confirm": true}))
	if err != nil {
		t.Fatalf("restart tolerated as error: %v", err)
	}
	r := got.(*fnResult)
	if !r.Restart || r.Confirmed {
		t.Errorf("%+v", r)
	}
	if n := len(writesWithParams(s, 0x4D)); n != 1 {
		t.Fatalf("the write was sent %d times", n)
	}
}

func TestFnSetMismatchWithoutRestartIsAnErrorAndNotRepeated(t *testing.T) {
	d, s := fnDaemon(t, 19797, func(s *scriptSession, op byte, p []byte) {
		if op == 0x40 {
			s.set(0x40, []byte{0x02}) // the unit refuses and reports off
		}
	})
	if _, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(64), "on": true})); err == nil {
		t.Fatal("a refused write was reported as done")
	}
	if n := len(writesWithParams(s, 0x40)); n != 1 {
		t.Fatalf("written %d times", n)
	}
}

func TestFnStatusReadsOnceAndCachesSilence(t *testing.T) {
	d, s := fnDaemon(t, 19797, nil)
	s.set(0x40, []byte{0x01})
	rep := mustStatus(t, d)
	fs, _ := rep["functions"].(map[string]string)
	if fs["64"] != "on" {
		t.Fatalf("functions %v", rep["functions"])
	}
	n := queriesOf(s, 0x40)
	d.invalidateCache()
	mustStatus(t, d)
	mustStatus(t, d)
	if got := queriesOf(s, 0x40); got != n {
		t.Fatalf("function queried again: %d -> %d", n, got)
	}
	// A write refreshes the reading of that function.
	if _, err := d.handleRequest(fnReq(map[string]interface{}{"cmdId": float64(64), "off": true})); err != nil {
		t.Logf("write: %v", err)
	}
}
