package models

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestSchemaVersion(t *testing.T) {
	if SchemaVersion() != 2 {
		t.Fatalf("schemaVersion %d", SchemaVersion())
	}
}

func TestEveryV1VendorStillPresent(t *testing.T) {
	b, err := os.ReadFile("testdata/vendor_ids_v1.txt")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Fields(string(b)) {
		v, err := strconv.ParseUint(l, 10, 16)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := Lookup(uint16(v)); !ok {
			t.Errorf("vendorId %d missing", v)
		}
		n++
	}
	if n < 221 {
		t.Fatalf("only %d ids listed", n)
	}
}

// The embedded file must decode strictly into the Go types: a field the
// generator writes and the types do not know is a schema drift.
func TestSnapshotMatchesSchema(t *testing.T) {
	var env struct {
		SchemaVersion int                        `json:"schemaVersion"`
		Models        map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.SchemaVersion != 2 {
		t.Fatalf("envelope: %v v=%d", err, env.SchemaVersion)
	}
	for k, r := range env.Models {
		dec := json.NewDecoder(bytes.NewReader(r))
		dec.DisallowUnknownFields()
		var p Product
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("model %s: %v", k, err)
		}
		if p.Panel == nil {
			continue
		}
		for i, f := range p.Panel.Functions {
			blocks := 0
			for _, ok := range []bool{f.Toggle != nil, f.Command != nil, len(f.Choices) > 0, f.Standby != nil,
				f.Language != nil, f.ANC != nil, f.EQ != nil, f.Keys != nil} {
				if ok {
					blocks++
				}
			}
			if blocks > 1 {
				t.Errorf("model %s function %d has %d blocks", k, i, blocks)
			}
			if f.Toggle != nil && (len(f.Toggle.On) == 0 || len(f.Toggle.Off) == 0) {
				t.Errorf("model %s function %d toggle %+v", k, i, f.Toggle)
			}
			if f.Type == nil && f.Raw == nil {
				t.Errorf("model %s function %d: no type and no raw", k, i)
			}
		}
	}
}

func TestN70Functions(t *testing.T) {
	for _, v := range []uint16{23872, 23877} {
		p, _ := Lookup(v)
		byCmd := map[int]Function{}
		var reset, factory, tone, standby, keys, anc, eq bool
		for _, f := range p.Panel.Functions {
			if f.CmdID != nil && f.Toggle != nil {
				byCmd[*f.CmdID] = f
			}
			switch {
			case f.Command != nil && f.Command.Bytes == "FF020100":
				reset = true
			case f.Command != nil && f.Command.Bytes == "FF020300":
				factory = true
			case f.Language != nil:
				tone = len(f.Language.Options) == 2 && f.Language.Options[0].Bytes == "57513030"
			case f.Standby != nil:
				if len(f.Standby.Options) == 5 && f.Standby.Options[4].Never && *f.Standby.Options[0].Minutes == 5 {
					standby = true
				}
			case f.Keys != nil:
				keys = len(f.Keys.Events) >= 8 && f.Keys.Events[0].DefaultFun != nil
			case f.ANC != nil:
				for _, m := range f.ANC.Modes {
					if m.VoiceCmdID != nil && *m.VoiceCmdID == 196864 {
						anc = true
					}
				}
			case f.EQ != nil:
				eq = f.EQ.Individuation != nil && len(f.EQ.Individuation.SoundDB) == 8 && f.Character == "0000000B" &&
					f.EQ.VolumeLevel != nil && *f.EQ.VolumeLevel == 4
			}
		}
		for _, c := range []int{9, 16, 35, 36, 42} {
			f, ok := byCmd[c]
			if !ok || len(f.Toggle.On) != 1 || f.Character != "00001001" {
				t.Errorf("%d toggle cmd %d: %+v", v, c, f)
			}
		}
		if f := byCmd[42]; len(f.Toggle.Off) == 1 && f.Toggle.Off[0] != "FF032A0100" {
			t.Errorf("%d wind noise off %v", v, f.Toggle.Off)
		}
		if f := byCmd[35]; f.Messages == nil || f.Messages.Msg == "" {
			t.Errorf("%d LDAC restart text missing", v)
		}
		for name, ok := range map[string]bool{"reset": reset, "factory": factory, "tone": tone,
			"standby": standby, "keys": keys, "anc": anc, "eq": eq} {
			if !ok {
				t.Errorf("%d: %s block missing or wrong", v, name)
			}
		}
	}
}

// The terms are assembled so that this file does not itself contain them.
var forbidden = regexp.MustCompile(`(?i)\b` + "ap" + `k\b|` + "de" + `compil|` + "ja" + `dx|` + "qcy-" + `reference|\.` + "ja" + `va\b|com\.` + "qcy" + `mall|` +
	"CMD" + `ID_|reverse ` + "engi" + `neering`)

func TestNoForbiddenTermsOrForeignURLs(t *testing.T) {
	if loc := forbidden.FindIndex(raw); loc != nil {
		t.Fatalf("forbidden term in snapshot: %q", raw[loc[0]:loc[1]])
	}
	urls := regexp.MustCompile(`(?i)https?://[^"\\ ]+`).FindAll(raw, -1)
	for _, u := range urls {
		if !strings.HasPrefix(strings.ToLower(string(u)), "http://img.qcymall.com/") &&
			!strings.HasPrefix(strings.ToLower(string(u)), "https://img.qcymall.com/") {
			t.Errorf("foreign URL %s", u)
		}
	}
	if regexp.MustCompile(`<[A-Za-z/][^>]*>`).Match(raw) {
		t.Error("HTML in snapshot")
	}
	if len(raw) > 4<<20 {
		t.Errorf("snapshot %d bytes", len(raw))
	}
}

func TestFrequencyKSuffixParsed(t *testing.T) {
	p, _ := Lookup(16737)
	for _, f := range p.Panel.EQ.Freq {
		if f <= 0 {
			t.Fatalf("zero frequency in %v", p.Panel.EQ.Freq)
		}
	}
}
