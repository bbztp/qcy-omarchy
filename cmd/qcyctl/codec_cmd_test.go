package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const testAddr = "AA:BB:CC:DD:EE:FF"

func loadPWFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/pw-dump-bluez.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pwDoc builds a pw-dump document with one bluez5 device (id 92).
func pwDoc(profiles string, active string, nodeCodec string) string {
	node := ""
	if nodeCodec != "" {
		node = `,{"id":95,"type":"PipeWire:Interface:Node","info":{"props":{"api.bluez5.codec":"` + nodeCodec + `","device.id":92,"media.class":"Audio/Sink"}}}`
	}
	return `[{"id":92,"type":"PipeWire:Interface:Device","info":{"props":{"device.api":"bluez5","api.bluez5.address":"` + testAddr + `"},"params":{"EnumProfile":[` + profiles + `],"Profile":[` + active + `]}}}` + node + `]`
}

func prof(idx int, name, desc, avail string) string {
	b, _ := json.Marshal(map[string]interface{}{"index": idx, "name": name, "description": desc, "available": avail})
	return string(b)
}

type fakeCodec struct {
	dumps    []string // successive pw-dump outputs; the last repeats
	dumpErr  error
	wpctlErr error
	missing  map[string]bool
	calls    [][]string
	dumpN    int
}

func (f *fakeCodec) env() codecEnv {
	var clock time.Time
	return codecEnv{
		now:  func() time.Time { return clock },
		have: func(n string) bool { return !f.missing[n] },
		exec: func(name string, args ...string) (string, error) {
			f.calls = append(f.calls, append([]string{name}, args...))
			if name == "pw-dump" {
				if f.dumpErr != nil {
					return "", f.dumpErr
				}
				i := f.dumpN
				if i >= len(f.dumps) {
					i = len(f.dumps) - 1
				}
				f.dumpN++
				return f.dumps[i], nil
			}
			return "", f.wpctlErr
		},
		sleep: func(d time.Duration) { clock = clock.Add(d) },
	}
}

func exitCodeOf(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if err == nil {
		return 0
	}
	return 1
}

func TestParseCodecFixture(t *testing.T) {
	st, err := parseCodecState(loadPWFixture(t), "aa:bb:cc:dd:ee:ff") // case-insensitive
	if err != nil {
		t.Fatal(err)
	}
	if st.Device != 92 {
		t.Errorf("device = %d", st.Device)
	}
	var got []string
	for _, m := range st.Music {
		got = append(got, m.Codec)
	}
	if strings.Join(got, ",") != "SBC,SBC-XQ,AAC,LDAC" {
		t.Errorf("music order = %v", got)
	}
	if st.Active.Kind != "music" || st.Active.Codec != "LDAC" {
		t.Errorf("active = %+v", st.Active)
	}
	for _, m := range st.Music {
		if m.Active != (m.Codec == "LDAC") {
			t.Errorf("%s active = %v", m.Codec, m.Active)
		}
	}
}

func TestParseCodecOtherDevice(t *testing.T) {
	st, err := parseCodecState(loadPWFixture(t), "11:22:33:44:55:66")
	if err != nil || st.Device != 120 || len(st.Music) != 1 || st.Active.Codec != "SBC" {
		t.Errorf("other device: %+v %v", st, err)
	}
	if _, err := parseCodecState(loadPWFixture(t), "00:00:00:00:00:01"); !errors.Is(err, errCodecNotFound) {
		t.Errorf("not found err = %v", err)
	}
}

func TestParseCodecCases(t *testing.T) {
	music := prof(0, "off", "Off", "yes") + "," +
		prof(1, "a2dp-sink-sbc", "High Fidelity Playback (A2DP Sink, codec SBC)", "yes") + "," +
		prof(2, "a2dp-sink-aptx_hd", "x (A2DP Sink, codec aptX HD)", "yes") + "," +
		prof(3, "a2dp-sink-aac", "x (A2DP Sink, codec AAC)", "no") + "," +
		prof(4, "a2dp-source", "A2DP Source", "yes") + "," +
		prof(5, "bap-duplex", "BAP", "yes") + "," +
		prof(6, "a2dp-sink", "x (A2DP Sink, codec LDAC)", "yes") + "," +
		prof(7, "headset-head-unit", "Headset (HSP/HFP, codec MSBC)", "yes")
	cases := []struct {
		name, active, node string
		kind, codec        string
		music              string
	}{
		{"music space name", `{"index":2,"name":"a2dp-sink-aptx_hd"}`, "", "music", "aptX HD", "SBC,aptX HD,LDAC"},
		{"call from description", `{"index":7,"name":"headset-head-unit"}`, "", "call", "MSBC", "SBC,aptX HD,LDAC"},
		{"call from node", `{"index":7,"name":"headset-head-unit"}`, "msbc", "call", "mSBC", "SBC,aptX HD,LDAC"},
		{"off", `{"index":0,"name":"off"}`, "", "off", "", "SBC,aptX HD,LDAC"},
		{"unsuffixed music uses node", `{"index":6,"name":"a2dp-sink"}`, "ldac", "music", "LDAC", "SBC,aptX HD,LDAC"},
	}
	for _, c := range cases {
		st, err := parseCodecState([]byte(pwDoc(music, c.active, c.node)), testAddr)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var names []string
		for _, m := range st.Music {
			names = append(names, m.Codec)
		}
		if st.Active.Kind != c.kind || st.Active.Codec != c.codec || strings.Join(names, ",") != c.music {
			t.Errorf("%s: active %+v music %v", c.name, st.Active, names)
		}
	}
}

func TestParseCodecInvalidJSON(t *testing.T) {
	if _, err := parseCodecState([]byte("not json"), testAddr); err == nil {
		t.Error("invalid JSON accepted")
	}
	f := &fakeCodec{dumps: []string{"{oops"}}
	err := runCodec(f.env(), []string{"--address", testAddr}, &bytes.Buffer{})
	if exitCodeOf(err) != exitUnavailable {
		t.Errorf("invalid JSON exit = %d (%v)", exitCodeOf(err), err)
	}
}

func TestCodecUnavailable(t *testing.T) {
	f := &fakeCodec{missing: map[string]bool{"pw-dump": true}}
	if c := exitCodeOf(runCodec(f.env(), []string{"--address", testAddr}, &bytes.Buffer{})); c != exitUnavailable {
		t.Errorf("pw-dump missing exit = %d", c)
	}
	f = &fakeCodec{dumpErr: errors.New("exit status 1")}
	if c := exitCodeOf(runCodec(f.env(), []string{"--address", testAddr}, &bytes.Buffer{})); c != exitUnavailable {
		t.Errorf("pw-dump failing exit = %d", c)
	}
	f = &fakeCodec{dumps: []string{string(loadPWFixture(t))}}
	if c := exitCodeOf(runCodec(f.env(), []string{"--address", "00:00:00:00:00:01"}, &bytes.Buffer{})); c != exitUnavailable {
		t.Errorf("device not found exit = %d", c)
	}
}

func TestCodecUsage(t *testing.T) {
	f := &fakeCodec{dumps: []string{"[]"}}
	for _, args := range [][]string{
		{}, {"--json"}, {"--address", "nope"}, {"--address"}, {"set", "--address", testAddr},
		{"bogus", "--address", testAddr}, {"--address", testAddr, "--wat"},
	} {
		if c := exitCodeOf(runCodec(f.env(), args, &bytes.Buffer{})); c != exitUsage {
			t.Errorf("%v: exit = %d, want %d", args, c, exitUsage)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("usage errors must not exec: %v", f.calls)
	}
}

func TestCodecJSONShape(t *testing.T) {
	f := &fakeCodec{dumps: []string{string(loadPWFixture(t))}}
	var out bytes.Buffer
	if err := runCodec(f.env(), []string{"--json", "--address", testAddr}, &out); err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	act := m["active"].(map[string]interface{})
	music := m["music"].([]interface{})
	e0 := music[0].(map[string]interface{})
	if m["device"].(float64) != 92 || act["codec"] != "LDAC" || act["kind"] != "music" ||
		e0["codec"] != "SBC" || e0["profile"] != "a2dp-sink-sbc" || e0["index"].(float64) != 131073 || e0["active"] != false {
		t.Errorf("json = %s", out.String())
	}
}

// fixtureOn returns the fixture with the given profile active (and the sink
// node reporting nodeCodec), as PipeWire would after a switch.
func fixtureOn(t *testing.T, index int, name, nodeCodec string) string {
	t.Helper()
	var objs []map[string]interface{}
	if err := json.Unmarshal(loadPWFixture(t), &objs); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		info := o["info"].(map[string]interface{})
		switch o["type"] {
		case "PipeWire:Interface:Device":
			if o["id"].(float64) == 92 {
				info["params"].(map[string]interface{})["Profile"] = []interface{}{map[string]interface{}{"index": index, "name": name}}
			}
		case "PipeWire:Interface:Node":
			info["props"].(map[string]interface{})["api.bluez5.codec"] = nodeCodec
		}
	}
	b, err := json.Marshal(objs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCodecSetCallsWpctlAndConfirms(t *testing.T) {
	// Fixture is on LDAC (131080); switching to AAC (131076). The second read
	// reports AAC active.
	before := string(loadPWFixture(t))
	after := fixtureOn(t, 131076, "a2dp-sink-aac", "aac")
	f := &fakeCodec{dumps: []string{before, after}}
	var out bytes.Buffer
	if err := runCodec(f.env(), []string{"set", "aac", "--json", "--address", testAddr}, &out); err != nil {
		t.Fatalf("set: %v\n%v", err, f.calls)
	}
	var m codecState
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || m.Active.Codec != "AAC" {
		t.Errorf("result = %s (%v)", out.String(), err)
	}
	var wp []string
	for _, c := range f.calls {
		if c[0] == "wpctl" {
			wp = c
		}
	}
	if strings.Join(wp, " ") != "wpctl set-profile 92 131076" {
		t.Errorf("wpctl call = %v", wp)
	}
}

func TestCodecSetNotConfirmed(t *testing.T) {
	f := &fakeCodec{dumps: []string{string(loadPWFixture(t))}} // never changes
	err := runCodec(f.env(), []string{"set", "SBC-XQ", "--address", testAddr}, &bytes.Buffer{})
	if exitCodeOf(err) != exitFailed {
		t.Errorf("exit = %d (%v)", exitCodeOf(err), err)
	}
	n := 0
	for _, c := range f.calls {
		if c[0] == "wpctl" {
			n++
			if strings.Join(c, " ") != "wpctl set-profile 92 131074" {
				t.Errorf("wpctl call = %v", c)
			}
		}
	}
	if n != 1 {
		t.Errorf("wpctl calls = %d", n)
	}
}

func TestCodecSetWpctlFails(t *testing.T) {
	f := &fakeCodec{dumps: []string{string(loadPWFixture(t))}, wpctlErr: errors.New("exit status 1")}
	err := runCodec(f.env(), []string{"set", "SBC", "--address", testAddr}, &bytes.Buffer{})
	if exitCodeOf(err) != exitFailed {
		t.Errorf("exit = %d", exitCodeOf(err))
	}
}

func TestCodecSetUnknownAndAlreadyActive(t *testing.T) {
	f := &fakeCodec{dumps: []string{string(loadPWFixture(t))}}
	err := runCodec(f.env(), []string{"set", "aptx", "--address", testAddr}, &bytes.Buffer{})
	if exitCodeOf(err) != exitFailed || !strings.Contains(err.Error(), "SBC, SBC-XQ, AAC, LDAC") {
		t.Errorf("unknown codec: %v", err)
	}
	if err := runCodec(f.env(), []string{"set", "ldac", "--address", testAddr}, &bytes.Buffer{}); err != nil {
		t.Errorf("already active: %v", err)
	}
	for _, c := range f.calls {
		if c[0] == "wpctl" {
			t.Errorf("no wpctl expected, got %v", c)
		}
	}
}

func TestCodecSetWpctlMissing(t *testing.T) {
	f := &fakeCodec{dumps: []string{string(loadPWFixture(t))}, missing: map[string]bool{"wpctl": true}}
	err := runCodec(f.env(), []string{"set", "sbc", "--address", testAddr}, &bytes.Buffer{})
	if exitCodeOf(err) != exitUnavailable {
		t.Errorf("exit = %d", exitCodeOf(err))
	}
}

func TestCodecLiveExecRefusedInTests(t *testing.T) {
	// The live seam goes through refuseHardware: tests never reach PipeWire.
	if _, err := liveCodecEnv().exec("pw-dump"); err == nil {
		t.Error("live exec ran in a test")
	}
}

func TestCodecAddressNormalised(t *testing.T) {
	f := &fakeCodec{dumps: []string{string(loadPWFixture(t))}}
	var out bytes.Buffer
	if err := runCodec(f.env(), []string{"--json", "--address", "AA-BB-CC-DD-EE-FF"}, &out); err != nil {
		t.Fatalf("dashed address: %v", err)
	}
	if !strings.Contains(out.String(), `"device":92`) {
		t.Errorf("out = %s", out.String())
	}
}

func TestCodecConfirmBudgetBounded(t *testing.T) {
	f := &fakeCodec{dumps: []string{string(loadPWFixture(t))}}
	_ = runCodec(f.env(), []string{"set", "SBC", "--address", testAddr}, &bytes.Buffer{})
	reads := 0
	for _, c := range f.calls {
		if c[0] == "pw-dump" {
			reads++
		}
	}
	if reads < 2 || reads > 10 { // 1 initial + about 3 s / 0.4 s
		t.Errorf("pw-dump reads = %d", reads)
	}
}
