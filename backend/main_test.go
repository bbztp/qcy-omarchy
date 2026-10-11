package main

import (
	"strings"
	"testing"
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
		"bad master":         {ten, "--yes", "--master-gain", "12"},
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
