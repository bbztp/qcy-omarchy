package main

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
)

// payload11 is a 73-byte 0x22 payload (the written form): slot 11, master +1.50 dB, band 1 at
// 40 Hz / -2.5 dB (not the fixed 31 Hz table).
func payload11() []byte {
	p := make([]byte, 73)
	p[0] = 11
	p[1], p[2] = 0x96, 0x00 // +150 centi-dB
	p[3], p[4] = 40, 0      // 40 Hz
	p[5], p[6] = 0x06, 0xff // -250
	return p
}

func TestEqReportFromUnitsAndSlot(t *testing.T) {
	r, err := eqReportFrom(payload11())
	if err != nil {
		t.Fatal(err)
	}
	if r.Preset != 11 || r.MasterGain != 1.5 {
		t.Fatalf("preset=%d masterGain=%v", r.Preset, r.MasterGain)
	}
	// only band 0 has a frequency; empty (freq 0) positions are not reported
	if len(r.Bands) != 1 || r.Bands[0].Freq != 40 || r.Bands[0].Gain != -2.5 || r.Bands[0].Index != 0 {
		t.Fatalf("bands = %+v", r.Bands)
	}
}

func TestStatusFromDaemonEq(t *testing.T) {
	var arr []interface{}
	for _, b := range payload11() {
		arr = append(arr, float64(b))
	}
	rep := statusFromDaemon(map[string]interface{}{"eq": arr})
	if rep.EQ == nil || rep.EQ.Preset != 11 || rep.EQ.MasterGain != 1.5 {
		t.Fatalf("%+v", rep.EQ)
	}
}

func TestStatusFromDaemonStaleFields(t *testing.T) {
	stale := statusFromDaemon(map[string]interface{}{
		"stale":      true,
		"ageSeconds": float64(12),
		"battery":    map[string]interface{}{"left": float64(80), "right": float64(70), "case": float64(50)},
	})
	m, err := structToMap(stale)
	if err != nil {
		t.Fatal(err)
	}
	if m["stale"] != true || m["ageSeconds"] != float64(12) {
		t.Fatalf("stale=%v ageSeconds=%v", m["stale"], m["ageSeconds"])
	}
	if b, _ := m["battery"].(map[string]interface{}); b["left"] != float64(80) {
		t.Fatalf("battery lost: %v", m["battery"])
	}

	fresh, err := structToMap(statusFromDaemon(map[string]interface{}{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh["stale"]; ok {
		t.Fatalf("fresh status carries stale: %v", fresh)
	}
	if _, ok := fresh["ageSeconds"]; ok {
		t.Fatalf("fresh status carries ageSeconds: %v", fresh)
	}
}

func TestMasterGainRejectsBeforeConnecting(t *testing.T) {
	// All of these must fail in validation, never reaching Bluetooth.
	for _, v := range []string{"NaN", "Inf", "-Inf", "12.5", "-13", "1e400", "abc", ""} {
		if err := cmdMasterGain([]string{v}); err == nil {
			t.Errorf("master-gain %q accepted", v)
		}
	}
	if err := cmdMasterGain(nil); err == nil {
		t.Error("master-gain with no value accepted")
	}
}

func TestEqSetValidation(t *testing.T) {
	ten := "0,0,0,0,0,0,0,0,0,0"
	cases := map[string][]string{
		"no --yes":           {ten},
		"short list":         {"1,2,3", "--yes"},
		"gain out of range":  {"0,0,0,0,0,0,0,0,0,9", "--yes"},
		"NaN gain":           {"0,0,0,0,0,0,0,0,0,NaN", "--yes"},
		"bad master":         {ten, "--yes", "--master-gain", "13"},
		"NaN master":         {ten, "--yes", "--master-gain=NaN"},
		"bad preset":         {ten, "--yes", "--preset", "300"},
		"preset needs value": {ten, "--yes", "--preset"},
	}
	for name, args := range cases {
		if err := cmdEqSet(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEqGetPresetUnsupported(t *testing.T) {
	err := cmdEqGet([]string{"--preset", "3"})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("err = %v", err)
	}
}

func TestRawEqWriteRefused(t *testing.T) {
	if _, err := opcodeFor("eq"); err == nil {
		t.Fatal("raw eq action allowed; it would bypass the backup")
	}
}

// Real 0x22 payloads read from the device (same bytes as qcycore's fixtures).
const (
	realCustom11  = "0b50fb320064003c0002c80076fd3c000284039cff460002f00ac800780002401f44fd5e01023f002c014600027d000000460002fa000000460002f4010000460002e8036400460002d0076400460002a00f6400460002401f900146000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	realClassico6 = "0650fb3c002c01320002c8000cfe46000284039cff460002f00ac800780002401f44fd5e0102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"
)

func mustPayload(t *testing.T, s string) []byte {
	t.Helper()
	p, err := hex.DecodeString(s)
	if err != nil || len(p) != qcycore.EqReadLen {
		t.Fatalf("fixture: %v len %d", err, len(p))
	}
	return p
}

func TestParseEqSetArgsFlags(t *testing.T) {
	ten := "0,0,0,0,0,0,0,0,0,0"
	a, err := parseEqSetArgs([]string{ten, "--yes", "--preset", "6"})
	if err != nil || a.op.Slot == nil || *a.op.Slot != 6 || a.op.AllowProtected || a.op.AllowFullLength {
		t.Fatalf("%+v %v", a, err)
	}
	// without --allow-protected the op this produces is refused by ApplyEq, for
	// a protected target and for leaving a protected active slot
	if _, err := qcycore.ApplyEq(mustPayload(t, realClassico6), a.op); !errors.Is(err, qcycore.ErrProtectedSlot) {
		t.Fatalf("--preset 6 on slot 6: %v", err)
	}
	b, err := parseEqSetArgs([]string{ten, "--yes", "--preset=0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qcycore.ApplyEq(mustPayload(t, realClassico6), b.op); !errors.Is(err, qcycore.ErrProtectedSlot) {
		t.Fatalf("--preset 0 away from slot 6: %v", err)
	}
	c, err := parseEqSetArgs([]string{ten, "--yes", "--preset", "0", "--allow-protected", "--full-length", "--json", "5"})
	if err != nil || !c.op.AllowProtected || !c.op.AllowFullLength || !c.jsonOut || !c.confirmed || len(c.secArgs) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := qcycore.ApplyEq(mustPayload(t, realCustom11), c.op); err != nil {
		t.Fatalf("both flags: %v", err)
	}
}

// fakeEqLink stands in for the BLE client: it answers the 0x22 query with
// state, records writes, and answers a write with echo.
type fakeEqLink struct {
	state  []byte
	echo   []byte // returned by setConfirm; nil = the written payload
	ok     bool
	writes [][]byte
}

func (f *fakeEqLink) query(byte, time.Duration) ([]byte, error) { return f.state, nil }
func (f *fakeEqLink) setConfirm(_ byte, params []byte, _ time.Duration, match func([]byte) bool) ([]byte, bool, error) {
	f.writes = append(f.writes, append([]byte(nil), params...))
	got := f.echo
	if got == nil {
		got = params
	}
	return got, match(got), nil
}

func backupFiles(t *testing.T, data string) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(data, "qcy-omarchy", "backups"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

func TestDirectEqApplyRefusalsHappenBeforeAnyWrite(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	zero := 0
	m := -100
	custom := mustPayload(t, realCustom11)
	classico := mustPayload(t, realClassico6)

	for name, tc := range map[string]struct {
		state []byte
		op    qcycore.EqOp
		want  *qcycore.EqError
	}{
		"protected target":       {classico, qcycore.EqOp{Slot: &zero}, qcycore.ErrProtectedSlot},
		"143 bytes without flag": {custom, qcycore.EqOp{MasterCenti: &m}, qcycore.ErrFullLengthRequired},
	} {
		l := &fakeEqLink{state: tc.state}
		_, err := eqApplyVia(l, tc.op)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if len(l.writes) != 0 || backupFiles(t, data) != 0 {
			t.Errorf("%s: wrote %d frames, %d backups", name, len(l.writes), backupFiles(t, data))
		}
	}

	// the proven 73-byte master write of the protected active slot still works
	l := &fakeEqLink{state: classico}
	res, err := eqApplyVia(l, qcycore.EqOp{MasterCenti: &m})
	if err != nil || len(l.writes) != 1 || len(l.writes[0]) != qcycore.EqLen || res.FullLength {
		t.Fatalf("%v %d", err, len(l.writes))
	}
	// and the explicit 143-byte write goes out whole
	l = &fakeEqLink{state: custom}
	res, err = eqApplyVia(l, qcycore.EqOp{MasterCenti: &m, AllowFullLength: true})
	if err != nil || len(l.writes) != 1 || len(l.writes[0]) != qcycore.EqReadLen || !res.FullLength {
		t.Fatalf("%v", err)
	}
}

// The device answers a cross-slot write with the old active slot: the CLI says
// the write was sent and unconfirmed, with what was written and the backup.
func TestDirectEqApplyUnconfirmedReportsWrittenAndBackup(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	zero := 0
	custom := mustPayload(t, realCustom11)
	l := &fakeEqLink{state: custom, echo: custom}
	_, err := eqApplyVia(l, qcycore.EqOp{
		Slot: &zero, MasterCenti: &zero, AllowProtected: true,
		Bands: []qcycore.BandSpec{{Index: 0, Freq: 100, Gain: 1, Q: 100, Type: 2}},
	})
	var ee *qcycore.EqError
	if !errors.Is(err, qcycore.ErrUnconfirmed) || !errors.As(err, &ee) {
		t.Fatalf("err = %v", err)
	}
	if len(l.writes) != 1 || !strings.Contains(err.Error(), hex.EncodeToString(l.writes[0])) {
		t.Fatalf("message lacks the written payload: %v", err)
	}
	if _, serr := os.Stat(ee.Backup); serr != nil || !strings.Contains(err.Error(), ee.Backup) {
		t.Fatalf("backup %q: %v / %v", ee.Backup, serr, err)
	}
}
