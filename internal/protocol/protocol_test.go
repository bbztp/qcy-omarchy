package protocol

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBlockVectors(t *testing.T) {
	cases := []struct {
		name   string
		cmd    byte
		params []byte
		want   string
	}{
		{"anc", OpANC, []byte{3, 1, 0}, "ff051703030100"},
		{"game on", OpGame, []byte{1}, "ff03090101"},
		{"sleep off", OpSleep, []byte{2}, "ff03100102"},
		{"fit start", OpFit, []byte{1, 1, 1}, "ff051103010101"},
		{"fit stop", OpFit, []byte{0, 0, 0}, "ff051103000000"},
		{"auto-off", OpAutoOff, []byte{4, 0x3c, 0, 0, 0}, "ff071405043c000000"},
		{"balance", OpBalance, []byte{50}, "ff03160132"},
		{"prompt volume", OpPromptVolume, []byte{8, 0}, "ff041d020800"},
		{"ldac", OpLDAC, []byte{1}, "ff03230101"},
		{"multipoint", OpMultipoint, []byte{2}, "ff03240102"},
		{"wind off", OpWind, []byte{0}, "ff032a0100"},
		{"wear", OpWear, []byte{1, 0, 0}, "ff052c03010000"},
		{"spatial", OpSpatial, []byte{1}, "ff032d0101"},
		{"hearing mode on", OpHearingMode, []byte{1}, "ff03450101"},
		{"hearing mode off", OpHearingMode, []byte{0}, "ff03450100"},
		{"wear sensitivity", OpWearSensitivity, []byte{2}, "ff03480102"},
		{"find", OpFind, []byte{1, 1}, "ff0405020101"},
		{"empty params", OpANC, nil, "ff021700"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(Block(c.cmd, c.params...))
		want := c.want
		if got != want {
			t.Errorf("%s: got %s want %s", c.name, got, want)
		}
	}
}

func TestQueryVectors(t *testing.T) {
	for _, op := range []byte{OpANC, OpGame, OpSleep, OpFit, OpAutoOff, OpBalance, OpPromptVolume, OpEQ,
		OpLDAC, OpMultipoint, OpWind, OpWear, OpSpatial, OpWearSensitivity, OpANCStrength} {
		want := []byte{0xff, 0x03, 0xfe, 0x01, op}
		if got := Query(op); !bytes.Equal(got, want) {
			t.Errorf("Query(0x%02x) = %x want %x", op, got, want)
		}
	}
}

func TestParseBlocksRoundTripAndMulti(t *testing.T) {
	fr, err := ParseBlocks(append(Block(OpANC, 3, 1, 0), Block(OpBalance, 50)...))
	if err != nil || len(fr) != 2 || fr[0].Cmd != OpANC || fr[1].Params[0] != 50 {
		t.Fatalf("two frames: %+v %v", fr, err)
	}
	// Several blocks inside one FF frame.
	multi := []byte{0xff, 0x07, 0x17, 0x03, 3, 1, 0, 0x16, 0x01, 50}
	multi[1] = byte(len(multi) - 2)
	fr, err = ParseBlocks(multi)
	if err != nil || len(fr) != 2 || fr[1].Cmd != OpBalance || fr[1].Params[0] != 50 {
		t.Fatalf("multi-block: %+v %v", fr, err)
	}
	for name, in := range map[string][]byte{
		"empty":      nil,
		"bad sof":    {0x00, 0x01},
		"trunc hdr":  {0xff},
		"trunc body": {0xff, 0x05, 0x17},
		"trunc blk":  {0xff, 0x01, 0x17},
		"trunc par":  {0xff, 0x03, 0x17, 0x05, 0x01},
	} {
		if _, err := ParseBlocks(in); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestBadSOFMessage(t *testing.T) {
	_, err := ParseBlocks([]byte{0xab})
	if err == nil || err.Error() != "bad SOF 0xab at offset 0" {
		t.Fatalf("got %v", err)
	}
}

func TestRegistryLoads(t *testing.T) {
	all := All()
	if len(all) < 80 {
		t.Fatalf("only %d entries", len(all))
	}
	e, ok := Lookup(OpANC)
	if !ok || e.Direction != "write" || e.Name == "" {
		t.Fatalf("ANC: %+v %v", e, ok)
	}
	if e, ok := Lookup(OpQuery); !ok || e.Direction != "query" {
		t.Fatalf("query row: %+v %v", e, ok)
	}
	if _, ok := Lookup(0x7E); ok {
		t.Fatal("unknown opcode found")
	}
	if e, ok := ByName(e.Name); !ok || e.Opcode != OpANC {
		t.Fatalf("ByName: %+v", e)
	}
	// Every row with an opcode carries consistent hex text.
	for _, e := range all {
		if e.HasOpcode && !strings.EqualFold(e.OpcodeHex, "0x"+strings.ToUpper(hex.EncodeToString([]byte{e.Opcode}))) {
			t.Errorf("%s: hex %s", e.Name, e.OpcodeHex)
		}
	}
}

func TestParseValidation(t *testing.T) {
	bad := map[string]string{
		"malformed":  `[{`,
		"empty":      `[]`,
		"no name":    `[{"opcode":1,"direction":"write"}]`,
		"direction":  `[{"name":"a","opcode":1,"direction":"sideways"}]`,
		"range":      `[{"name":"a","opcode":300,"direction":"write"}]`,
		"hex":        `[{"name":"a","opcode":1,"opcodeHex":"0x02","direction":"write"}]`,
		"dup opcode": `[{"name":"a","opcode":1,"direction":"write"},{"name":"b","opcode":1,"direction":"write"}]`,
		"dup name":   `[{"name":"a","opcode":1,"direction":"write"},{"name":"a","opcode":2,"direction":"write"}]`,
	}
	for n, in := range bad {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: want error", n)
		}
	}
	// Same opcode with a different direction is allowed.
	if _, err := Parse([]byte(`[{"name":"a","opcode":1,"direction":"write"},{"name":"b","opcode":1,"direction":"query"}]`)); err != nil {
		t.Fatal(err)
	}
}

func TestActionOpcodesAreRegistered(t *testing.T) {
	for action, op := range ActionOpcodes {
		e, ok := Lookup(op)
		if !ok {
			t.Errorf("action %s: opcode 0x%02x not in the registry", action, op)
			continue
		}
		if e.Direction != "write" {
			t.Errorf("action %s: 0x%02x is a %s row, not write", action, op, e.Direction)
		}
	}
}

// Every opcode constant exists in the table. Constants that the table lists
// only as a query or notify row are marked here so a direction change shows up.
func TestConstantsInRegistry(t *testing.T) {
	want := map[byte]string{
		OpFind: "write", OpANCStrength: "write", OpGame: "write", OpSleep: "write", OpFit: "write",
		OpAutoOff: "write", OpBalance: "write", OpANC: "write", OpPromptVolume: "write", OpEQ: "write",
		OpLDAC: "write", OpMultipoint: "write", OpWind: "write", OpWear: "write", OpSpatial: "write",
		OpBattery: "notify", OpVersion: "query", OpHearingMode: "write", OpHearingLeft: "write",
		OpHearingRight: "write", OpWearSensitivity: "write", OpQuery: "query",
	}
	for op, dir := range want {
		if _, ok := Default().LookupDir(op, dir); !ok {
			t.Errorf("0x%02x %s missing from the registry", op, dir)
		}
	}
}
