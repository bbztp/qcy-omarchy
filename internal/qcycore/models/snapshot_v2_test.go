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
			for _, ok := range []bool{f.ToggleBlock != nil, f.CommandBlock != nil, len(f.ChoiceList) > 0, f.StandbyBlock != nil,
				f.LanguageBlock != nil, f.ANC != nil, f.EQ != nil, f.Keys != nil} {
				if ok {
					blocks++
				}
			}
			if blocks > 1 {
				t.Errorf("model %s function %d has %d blocks", k, i, blocks)
			}
			if f.ToggleBlock != nil && (len(f.ToggleBlock.On) == 0 || len(f.ToggleBlock.Off) == 0) {
				t.Errorf("model %s function %d toggle %+v", k, i, f.ToggleBlock)
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
			if f.CmdID != nil && f.ToggleBlock != nil {
				byCmd[*f.CmdID] = f
			}
			switch {
			case f.CommandBlock != nil && f.CommandBlock.Bytes == "FF020100":
				reset = true
			case f.CommandBlock != nil && f.CommandBlock.Bytes == "FF020300":
				factory = true
			case f.LanguageBlock != nil:
				tone = len(f.LanguageBlock.Options) == 2 && f.LanguageBlock.Options[0].Bytes == "57513030"
			case f.StandbyBlock != nil:
				if len(f.StandbyBlock.Options) == 5 && f.StandbyBlock.Options[4].Never && *f.StandbyBlock.Options[0].Minutes == 5 {
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
			if !ok || len(f.ToggleBlock.On) != 1 || f.Character != "00001001" {
				t.Errorf("%d toggle cmd %d: %+v", v, c, f)
			}
		}
		if f := byCmd[42]; len(f.ToggleBlock.Off) == 1 && f.ToggleBlock.Off[0] != "FF032A0100" {
			t.Errorf("%d wind noise off %v", v, f.ToggleBlock.Off)
		}
		if f := byCmd[35]; f.MessageBlock == nil || f.MessageBlock.Msg == "" {
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

func TestFunctionsForAndAccessors(t *testing.T) {
	if FunctionsFor(1) != nil {
		t.Fatal("unknown vendorId returned functions")
	}
	fs := FunctionsFor(23872)
	if len(fs) == 0 {
		t.Fatal("N70 has no functions")
	}
	// The result is a copy: reordering it must not touch the snapshot.
	fs[0], fs[1] = fs[1], fs[0]
	if again := FunctionsFor(23872); again[0].Name != "Customizing button function" {
		t.Fatalf("snapshot mutated: %q", again[0].Name)
	}
	var tog, cmd, lang, sb, ch, msg bool
	for _, f := range FunctionsFor(23872) {
		if t, ok := f.Toggle(); ok && f.CmdID != nil && *f.CmdID == 9 {
			tog = len(t.On) == 1 && t.On[0] == "FF03090101" && t.Off[0] == "FF03090102"
		}
		if c, ok := f.Command(); ok && c.Bytes == "FF020100" {
			cmd = true
		}
		if l, ok := f.Language(); ok && len(l.Options) == 2 {
			lang = true
		}
		if s, ok := f.Standby(); ok && len(s.Options) == 5 {
			sb = true
		}
		if len(f.Choices()) == 4 {
			ch = true
		}
		if m, ok := f.Messages(); ok && m.Msg != "" {
			msg = true
		}
	}
	for n, ok := range map[string]bool{"toggle": tog, "command": cmd, "language": lang, "standby": sb, "choices": ch, "messages": msg} {
		if !ok {
			t.Errorf("accessor %s found nothing", n)
		}
	}
	var none Function
	if _, ok := none.Toggle(); ok {
		t.Error("empty function has a toggle")
	}
	if len(none.Choices()) != 0 {
		t.Error("empty function has choices")
	}
}
