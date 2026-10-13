package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHearingPlan(t *testing.T) {
	var b bytes.Buffer
	if err := hearingMain([]string{"plan"}, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "-55.3 dBFS") || strings.Count(b.String(), "\n") < 17 {
		t.Fatal(b.String())
	}
}

func TestHearingResultZero(t *testing.T) {
	var b bytes.Buffer
	z := "0,0,0,0,0,0,0,0"
	if err := hearingMain([]string{"result", "--left", z, "--right", z}, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "master gain: -10.00 dB") || !strings.Contains(out, "ff3d463b0b18fc") || !strings.Contains(out, "ff03450101") {
		t.Fatal(out)
	}
	if err := hearingMain([]string{"result", "--left", "1,2", "--right", z}, &b); err == nil {
		t.Fatal("expected error")
	}
}

func TestHearingDryRun(t *testing.T) {
	var b bytes.Buffer
	if err := hearingMain([]string{"run", "--dry-run", "--answers", "n,n,n,n,n,n"}, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "incomplete after 6 answers") {
		t.Fatal(b.String())
	}
	if err := hearingMain([]string{"run", "--answers", "n"}, &b); err == nil {
		t.Fatal("real run must not exist")
	}
	full := strings.TrimSuffix(strings.Repeat("n,n,n,n,n,n,", 16), ",")
	b.Reset()
	if err := hearingMain([]string{"run", "--dry-run", "--answers", full, "--json"}, &b); err != nil || !strings.Contains(b.String(), `"complete": true`) {
		t.Fatal(err, b.String())
	}
}
