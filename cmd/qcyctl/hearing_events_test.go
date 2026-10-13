package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// eventsRig is the terminal rig with the events stream and stdin commands.
type eventsRig struct {
	*rig
	events  *bytes.Buffer
	stdinW  *io.PipeWriter
	outFile string
	opts    hearingRunOpts
	// atStopped: what the fakes had seen when "stopped" was written.
	atStopped struct {
		seen      bool
		plays     int
		pauses    int
		lastVol   string
		linkEvs   string
		tonesLeft int
	}
	stopFeed chan struct{}
	feedOnce sync.Once
}

type stoppedHook struct {
	r *eventsRig
}

func (h stoppedHook) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"event":"stopped"`)) {
		r := h.r
		r.atStopped.seen = true
		r.atStopped.plays = r.ex.count(" play")
		r.atStopped.pauses = r.ex.count(" pause")
		r.ex.mu.Lock()
		for _, c := range r.ex.calls {
			if strings.Contains(c, "set-sink-volume") {
				r.atStopped.lastVol = c
			}
		}
		r.ex.mu.Unlock()
		r.atStopped.linkEvs = r.link.list()
		r.pl.mu.Lock()
		r.atStopped.tonesLeft = r.pl.started - r.pl.stop
		r.pl.mu.Unlock()
	}
	return h.r.events.Write(p)
}

func newEventsRig(t *testing.T) *eventsRig {
	t.Helper()
	pr, pw := io.Pipe()
	rg := newRig(t, newEventKeys(pr))
	er := &eventsRig{rig: rg, events: &bytes.Buffer{}, stdinW: pw, stopFeed: make(chan struct{})}
	er.outFile = filepath.Join(t.TempDir(), "hearing.json")
	er.opts = opts()
	er.opts.events = stoppedHook{er}
	er.opts.outFile = er.outFile
	er.opts.acceptFloor = false
	t.Cleanup(func() { er.feedOnce.Do(func() { close(er.stopFeed) }); pw.CloseWithError(io.EOF) })
	return er
}

// feed sends cmds[i] once i+1 tones have started (a listener answers a tone
// that is playing); then, when eof is set, it closes stdin.
func (r *eventsRig) feed(cmds []string, eof bool) {
	go func() {
		for i, c := range cmds {
			for r.pl.startedCount() < i+1 {
				select {
				case <-r.stopFeed:
					return
				case <-time.After(time.Millisecond):
				}
			}
			if _, err := io.WriteString(r.stdinW, c+"\n"); err != nil {
				return
			}
		}
		if eof {
			r.stdinW.Close()
		}
	}()
}

// cycleCmds is not-heard, not-heard, heard, ... (the staircase terminates).
func cycleCmds(n int) []string {
	var c []string
	for i := 1; i <= n; i++ {
		if i%3 == 0 {
			c = append(c, "heard")
		} else {
			c = append(c, "not-heard")
		}
	}
	return c
}

func (r *eventsRig) list(t *testing.T) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, ln := range strings.Split(strings.TrimSpace(r.events.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("event line %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

func (r *eventsRig) assertStoppedAfterCleanup(t *testing.T) {
	t.Helper()
	if !r.atStopped.seen {
		t.Fatal("no stopped event")
	}
	if r.atStopped.tonesLeft != 0 {
		t.Errorf("a tone was still playing at stopped: %d", r.atStopped.tonesLeft)
	}
	if r.atStopped.pauses != r.atStopped.plays {
		t.Errorf("music not resumed at stopped: %d pauses, %d plays", r.atStopped.pauses, r.atStopped.plays)
	}
	if strings.Contains(r.atStopped.linkEvs, "mode:on") && !strings.HasSuffix(r.atStopped.linkEvs, "mode:off") {
		t.Errorf("test mode still on at stopped: %s", r.atStopped.linkEvs)
	}
	if r.atStopped.lastVol != "" && r.atStopped.lastVol != "pactl set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 39321 36044" {
		t.Errorf("volume not restored at stopped: %q", r.atStopped.lastVol)
	}
	if strings.Contains(r.link.list(), "write:") {
		t.Errorf("--events wrote to the earbuds: %s", r.link.list())
	}
}

func evNames(evs []map[string]interface{}) []string {
	var n []string
	for _, e := range evs {
		s := e["event"].(string)
		if s == "phase" {
			s += ":" + e["name"].(string)
		}
		n = append(n, s)
	}
	return n
}

func TestEventsFullTestOrderAndResultMatchesFile(t *testing.T) {
	r := newEventsRig(t)
	r.opts.baseline = "zero"
	r.feed(cycleCmds(2000), false)
	if err := hearingRunLive(r.env, r.opts); err != nil {
		t.Fatal(err)
	}
	r.assertRestored(t)
	r.assertStoppedAfterCleanup(t)
	evs := r.list(t)
	names := evNames(evs)
	head := []string{"phase:pausing-music", "phase:checking-earbuds", "phase:waiting-silence", "phase:starting", "phase:testing"}
	if !reflect.DeepEqual(names[:len(head)], head) {
		t.Fatalf("head %v", names)
	}
	n := len(names)
	if names[n-1] != "stopped" || names[n-2] != "phase:done" || names[n-3] != "result" || names[n-4] != "phase:finishing" {
		t.Fatalf("tail %v", names[n-5:])
	}
	// tone, answer, tone, answer ...
	body := names[len(head) : n-4]
	for i, s := range body {
		want := "tone"
		if i%2 == 1 {
			want = "answer"
		}
		if s != want {
			t.Fatalf("body[%d] = %s, want %s (%v)", i, s, want, body)
		}
	}
	first := evs[len(head)]
	for _, k := range []string{"index", "ear", "band", "bands", "freq", "step", "levelDb", "progress"} {
		if _, ok := first[k]; !ok {
			t.Errorf("tone without %q: %v", k, first)
		}
	}
	if first["ear"] != "left" || first["index"].(float64) != 1 || first["bands"].(float64) != 8 {
		t.Errorf("first tone %v", first)
	}
	if evs[n-1]["reason"] != "done" {
		t.Errorf("stopped %v", evs[n-1])
	}

	res := evs[n-3]
	b, err := os.ReadFile(r.outFile)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(r.outFile); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	var f struct {
		Left, Right []int
		Floor       map[string][]bool
		Result      struct{ MasterGain float64 }
		Baseline    string
		Applied     bool
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	toInts := func(v interface{}) []int {
		var o []int
		for _, x := range v.([]interface{}) {
			o = append(o, int(x.(float64)))
		}
		return o
	}
	if !reflect.DeepEqual(toInts(res["left"]), f.Left) || !reflect.DeepEqual(toInts(res["right"]), f.Right) {
		t.Errorf("result %v vs file %v %v", res, f.Left, f.Right)
	}
	if len(f.Left) != 8 || len(f.Right) != 8 {
		t.Errorf("bands %d %d", len(f.Left), len(f.Right))
	}
	fl := res["floor"].(map[string]interface{})
	if len(fl["left"].([]interface{})) != 8 || len(fl["right"].([]interface{})) != 8 {
		t.Errorf("floor %v", fl)
	}
	if res["masterGain"].(float64) != f.Result.MasterGain || res["outFile"] != r.outFile {
		t.Errorf("result %v file master %v", res, f.Result.MasterGain)
	}
	if f.Baseline != "zero" || f.Applied {
		t.Errorf("baseline %q applied %v", f.Baseline, f.Applied)
	}
	// What `hearing apply` will read.
	_, _, rf, _, err := loadHearingResult(r.outFile)
	if err != nil {
		t.Fatalf("apply cannot read the file: %v", err)
	}
	if rf.Baseline != "zero" {
		t.Errorf("baseline lost: %q", rf.Baseline)
	}
	// Events only on stdout side: progress text is in the other writer.
	if strings.Contains(r.events.String(), "sink:") || !strings.Contains(r.out.String(), "sink:") {
		t.Error("progress text and events are mixed")
	}
}

func TestEventsAbortCommandRestoresAndStops(t *testing.T) {
	r := newEventsRig(t)
	r.feed([]string{"not-heard", "not-heard", "abort"}, false)
	if err := hearingRunLive(r.env, r.opts); err != nil {
		t.Fatalf("an abort is a normal end: %v", err)
	}
	r.assertRestored(t)
	r.assertStoppedAfterCleanup(t)
	names := evNames(r.list(t))
	n := len(names)
	if names[n-1] != "stopped" || names[n-2] != "phase:aborted" {
		t.Fatalf("%v", names)
	}
	if r.list(t)[n-1]["reason"] != "abort" {
		t.Errorf("reason %v", r.list(t)[n-1])
	}
	if _, err := os.Stat(r.outFile); err == nil {
		t.Error("an aborted test left a result file")
	}
	for _, s := range names {
		if s == "result" {
			t.Error("result after abort")
		}
	}
}

func TestEventsEOFIsAbort(t *testing.T) {
	r := newEventsRig(t)
	r.feed([]string{"not-heard"}, true)
	if err := hearingRunLive(r.env, r.opts); err != nil {
		t.Fatal(err)
	}
	r.assertRestored(t)
	r.assertStoppedAfterCleanup(t)
	evs := r.list(t)
	if last := evs[len(evs)-1]; last["event"] != "stopped" || last["reason"] != "abort" {
		t.Fatalf("%v", last)
	}
}

func TestEventsSignalRestoresAndStops(t *testing.T) {
	r := newEventsRig(t)
	sigs := make(chan os.Signal, 1)
	r.env.sigs = sigs
	go func() {
		for r.pl.startedCount() < 2 {
			time.Sleep(time.Millisecond)
		}
		sigs <- syscall.SIGTERM
	}()
	r.feed([]string{"not-heard"}, false)
	err := hearingRunLive(r.env, r.opts)
	if err == nil {
		t.Fatal("a signal must be reported")
	}
	r.assertRestored(t)
	r.assertStoppedAfterCleanup(t)
	evs := r.list(t)
	if last := evs[len(evs)-1]; last["event"] != "stopped" || last["reason"] != "signal" {
		t.Fatalf("%v", last)
	}
}

func TestEventsRepeatReplaysTheTone(t *testing.T) {
	r := newEventsRig(t)
	r.feed([]string{"repeat", "not-heard", "abort"}, false)
	if err := hearingRunLive(r.env, r.opts); err != nil {
		t.Fatal(err)
	}
	r.assertRestored(t)
	var tones []map[string]interface{}
	for _, e := range r.list(t) {
		if e["event"] == "tone" {
			tones = append(tones, e)
		}
	}
	if len(tones) < 3 {
		t.Fatalf("tones %v", tones)
	}
	if tones[0]["index"] != tones[1]["index"] || tones[0]["freq"] != tones[1]["freq"] || tones[0]["step"] != tones[1]["step"] {
		t.Errorf("repeat changed the tone: %v %v", tones[0], tones[1])
	}
	if tones[2]["index"].(float64) != 2 {
		t.Errorf("after the answer the index is %v", tones[2]["index"])
	}
	for _, e := range r.list(t) {
		if e["event"] == "answer" && e["band"].(float64) == 0 && e["ear"] == "left" {
			return
		}
	}
	t.Error("no answer for the first tone")
}

func TestEventsDeadToneRepeatsAndWarns(t *testing.T) {
	r := newEventsRig(t)
	r.pl.deadAtStart = map[int]bool{1: true}
	r.feed([]string{"not-heard", "abort"}, false)
	// the first start is dead: the feed counts starts, so the first command
	// answers the replay.
	if err := hearingRunLive(r.env, r.opts); err != nil {
		t.Fatal(err)
	}
	r.assertRestored(t)
	var warn bool
	for _, e := range r.list(t) {
		if e["event"] == "warning" && strings.Contains(e["message"].(string), "tone interrupted") {
			warn = true
		}
	}
	if !warn {
		t.Errorf("no warning for the dead tone: %v", evNames(r.list(t)))
	}
}

func TestEventsSinkErrorEmitsErrorAndStopped(t *testing.T) {
	r := newEventsRig(t)
	r.ex.out["pactl list short sinks"] = "47\talsa_output.pci-0000_00_1f.3.analog-stereo\tPipeWire\ts32le\tSUSPENDED\n"
	err := hearingRunLive(r.env, r.opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	evs := r.list(t)
	names := evNames(evs)
	want := []string{"error", "phase:error", "stopped"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("%v", names)
	}
	if !strings.Contains(evs[0]["message"].(string), "no Bluetooth sink") || evs[2]["reason"] != "error" {
		t.Errorf("%v", evs)
	}
	if r.ex.count("set-sink-volume") != 0 {
		t.Error("volume touched")
	}
}

func TestEventsAbortDuringSilenceWaitNeverRaisesVolume(t *testing.T) {
	r := newEventsRig(t)
	var once sync.Once
	r.ex.onCall = func(line string) {
		if line == "pactl list sink-inputs" {
			once.Do(func() {
				go io.WriteString(r.stdinW, "abort\n")
				time.Sleep(50 * time.Millisecond)
			})
		}
	}
	if err := hearingRunLive(r.env, r.opts); err != nil {
		t.Fatal(err)
	}
	if r.ex.count("set-sink-volume") != 0 {
		t.Errorf("volume raised after abort: %v", r.ex.calls)
	}
	r.assertRestored(t)
	r.assertStoppedAfterCleanup(t)
	evs := r.list(t)
	if last := evs[len(evs)-1]; last["reason"] != "abort" {
		t.Fatalf("%v", last)
	}
}

func TestEventKeysCommands(t *testing.T) {
	k := newEventKeys(strings.NewReader("heard\nfoo\n NOT-HEARD \nrepeat\nabort\n"))
	var got []byte
	for {
		b, err := k.ReadKey()
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
		got = append(got, b)
	}
	if string(got) != "hnrq" {
		t.Fatalf("%q", got)
	}
}

func TestHearingRunEventsRejectsWriteFlags(t *testing.T) {
	for _, a := range [][]string{
		{"--events", "--apply", "--yes"}, {"--events", "--yes"}, {"--events", "--dry-run"},
		{"--events", "--accept-floor"}, {"--events", "--baseline", "weird"},
	} {
		if err := hearingRunCmd(a, io.Discard); err == nil {
			t.Errorf("%v accepted", a)
		}
	}
}
