package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeBeeper struct {
	mu      sync.Mutex
	started int
	stopped int
	dead    bool
	onStop  func()
}

func (b *fakeBeeper) Start(sink string) (func() error, <-chan struct{}, error) {
	b.mu.Lock()
	b.started++
	b.mu.Unlock()
	d := make(chan struct{})
	if b.dead {
		close(d)
	}
	return func() error {
		b.mu.Lock()
		b.stopped++
		hook := b.onStop
		b.mu.Unlock()
		if hook != nil {
			hook()
		}
		return nil
	}, d, nil
}

func (b *fakeBeeper) startedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.started
}

// fakeClock advances only when something sleeps.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
	n  int // sleeps
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleepFn(step time.Duration, real time.Duration) func(time.Duration) {
	return func(d time.Duration) {
		c.mu.Lock()
		if step > 0 {
			d = step
		}
		c.t = c.t.Add(d)
		c.n++
		c.mu.Unlock()
		if real > 0 {
			time.Sleep(real)
		}
	}
}

type findRig struct {
	*rig
	clock  *fakeClock
	env    findEnv
	beep   *fakeBeeper
	events *bytes.Buffer
}

// never blocks forever: the run ends by timeout, error or signal.
func neverKeys() fnKeys {
	return fnKeys{func() (byte, error) { select {} }}
}

func newFindRig(t *testing.T, keys keySource) *findRig {
	t.Helper()
	rg := newRig(t, keys)
	b := &fakeBeeper{}
	b.onStop = func() {
		rg.ex.mu.Lock()
		rg.ex.calls = append(rg.ex.calls, "<tone stopped>")
		rg.ex.mu.Unlock()
	}
	ev := &bytes.Buffer{}
	clk := &fakeClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	rg.env.now, rg.env.sleep = clk.now, clk.sleepFn(0, 0)
	return &findRig{rig: rg, clock: clk, env: findEnv{hearingEnv: rg.env, beep: b, events: ev}, beep: b, events: ev}
}

func (r *findRig) run(o findOpts) error {
	if o.maxPct == 0 {
		o.maxPct = 70
	}
	if o.startPct == 0 {
		o.startPct = 20 // the tests exercise the ramp; the product default starts at 100%
	}
	if o.timeout == 0 {
		o.timeout = 60 * time.Second
	}
	return findRunLive(r.env, o)
}

// setVolumes lists the percentages written with `pactl set-sink-volume`.
func (r *findRig) setVolumes() []int {
	r.ex.mu.Lock()
	defer r.ex.mu.Unlock()
	var out []int
	for _, c := range r.ex.calls {
		if strings.HasPrefix(c, "pactl set-sink-volume ") && strings.HasSuffix(c, "%") {
			f := strings.Fields(c)
			n, _ := strconv.Atoi(strings.TrimSuffix(f[len(f)-1], "%"))
			out = append(out, n)
		}
	}
	return out
}

func (r *findRig) eventList(t *testing.T) []map[string]interface{} {
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

func (r *findRig) assertRestored(t *testing.T) {
	t.Helper()
	// Raw values restored (the fake sink was at 39321/36044), mute put back
	// (it was muted), the players that were Playing resumed.
	if r.ex.index("pactl set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 39321 36044") < 0 {
		t.Errorf("volume not restored: %v", r.ex.calls)
	}
	if r.ex.index("pactl set-sink-mute bluez_output.AA_BB_CC_DD_EE_FF.1 1") < 0 {
		t.Errorf("mute not restored")
	}
	for _, p := range []string{"spotify", "mpv"} {
		if r.ex.index("playerctl --player="+p+" play") < 0 {
			t.Errorf("%s not resumed", p)
		}
	}
	if r.ex.index("playerctl --player=firefox.instance123 play") >= 0 {
		t.Error("a player that was not playing was resumed")
	}
	st, vol := r.ex.index("<tone stopped>"), r.ex.index("39321 36044")
	if r.beep.started > 0 && (st < 0 || st > vol) {
		t.Errorf("tone must stop before the volume is restored: %d %d", st, vol)
	}
}

func TestFindRampsToCeilingAndStopsAtTimeout(t *testing.T) {
	r := newFindRig(t, neverKeys())
	if err := r.run(findOpts{yes: true, timeout: 60 * time.Second}); err != nil {
		t.Fatal(err)
	}
	vols := r.setVolumes()
	want := []int{20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70}
	if len(vols) != len(want) {
		t.Fatalf("volumes %v, want %v", vols, want)
	}
	for i, v := range want {
		if vols[i] != v {
			t.Fatalf("volumes %v, want %v", vols, want)
		}
	}
	if r.beep.startedCount() != 1 || r.beep.stopped != 1 {
		t.Fatalf("beeper started %d stopped %d", r.beep.started, r.beep.stopped)
	}
	r.assertRestored(t)
	ev := r.eventList(t)
	if ev[0]["event"] != "started" || ev[0]["volume"] != float64(20) || ev[0]["maxVolume"] != float64(70) {
		t.Fatalf("started event %v", ev[0])
	}
	last := ev[len(ev)-1]
	if last["event"] != "stopped" || last["reason"] != "timeout" {
		t.Fatalf("last event %v", last)
	}
	nvol := 0
	for _, e := range ev {
		if e["event"] == "volume" {
			nvol++
		}
	}
	if nvol != 10 {
		t.Fatalf("%d volume events", nvol)
	}
}

func TestFindTimeoutIsHonoured(t *testing.T) {
	r := newFindRig(t, neverKeys())
	if err := r.run(findOpts{yes: true, timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	// 5 s: the steps at 2 s and 4 s only.
	if v := r.setVolumes(); len(v) != 3 || v[2] != 30 {
		t.Fatalf("volumes %v", v)
	}
}

func TestFindNeverCrossesTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		max  int
		last int
	}{{30, 30}, {10, 10}, {100, 100}, {42, 42}} {
		r := newFindRig(t, neverKeys())
		if err := r.run(findOpts{yes: true, maxPct: tc.max}); err != nil {
			t.Fatal(err)
		}
		vols := r.setVolumes()
		for _, v := range vols {
			if v > tc.max {
				t.Fatalf("max %d: wrote %d (%v)", tc.max, v, vols)
			}
		}
		if vols[len(vols)-1] != tc.last {
			t.Fatalf("max %d: %v", tc.max, vols)
		}
		if tc.max < 20 && vols[0] != tc.max {
			t.Fatalf("start above the ceiling: %v", vols)
		}
	}
}

func TestFindNothingLoudBeforeSilence(t *testing.T) {
	r := newFindRig(t, neverKeys())
	// The sink stays RUNNING with a stream whose peak cannot be measured.
	r.ex.out["pactl list sinks"] = strings.Replace(r.ex.out["pactl list sinks"], "State: IDLE", "State: RUNNING", 1)
	r.ex.out["pactl list sink-inputs"] = "Sink Input #7\n\tSink: 92\n\tCorked: no\n\tMute: no\n\tVolume: front-left: 65536 / 100% / 0.00 dB\n\tProperties:\n\t\tapplication.name = \"Brave\"\n"
	err := r.run(findOpts{yes: true})
	if err == nil || !strings.Contains(err.Error(), "still playing") {
		t.Fatalf("err = %v", err)
	}
	if v := r.setVolumes(); len(v) != 0 || r.ex.count("set-sink-mute") != 0 {
		t.Fatalf("volume or mute touched before silence: %v", r.ex.calls)
	}
	if r.beep.startedCount() != 0 {
		t.Fatal("tone started")
	}
	if r.ex.index("playerctl --player=spotify play") < 0 {
		t.Error("music not resumed after the refusal")
	}
	ev := r.eventList(t)
	if ev[len(ev)-2]["event"] != "error" || ev[len(ev)-1]["reason"] != "error" {
		t.Fatalf("events %v", ev)
	}
}

func TestFindFirstVolumeComesAfterEverySilenceCheck(t *testing.T) {
	r := newFindRig(t, neverKeys())
	if err := r.run(findOpts{yes: true, timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	r.ex.mu.Lock()
	defer r.ex.mu.Unlock()
	firstVol, lastCheck, pause := -1, -1, -1
	for i, c := range r.ex.calls {
		switch {
		case strings.HasPrefix(c, "pactl set-sink-volume") && firstVol < 0:
			firstVol = i
		case c == "pactl list sink-inputs":
			if firstVol < 0 {
				lastCheck = i
			}
		case strings.HasPrefix(c, "playerctl --player=spotify pause"):
			pause = i
		}
	}
	if pause < 0 || lastCheck < pause || firstVol < lastCheck {
		t.Fatalf("pause %d, last silence check %d, first volume %d", pause, lastCheck, firstVol)
	}
}

func TestFindStopKeyRestores(t *testing.T) {
	var rg *findRig
	keys := fnKeys{func() (byte, error) {
		// a key typed before the tone runs is discarded: wait for the ramp
		for rg.ex.index("set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 25%") < 0 {
			time.Sleep(time.Millisecond)
		}
		return 'q', nil
	}}
	rg = newFindRig(t, keys)
	// Pace the loop (the fake sleep is instant) so the key can win the race.
	rg.env.sleep = rg.clock.sleepFn(0, 200*time.Microsecond)
	if err := rg.run(findOpts{yes: true}); err != nil {
		t.Fatal(err)
	}
	rg.assertRestored(t)
	ev := rg.eventList(t)
	if ev[len(ev)-1]["reason"] != "stop" {
		t.Fatalf("events %v", ev)
	}
}

func TestFindSignalRestores(t *testing.T) {
	r := newFindRig(t, neverKeys())
	sigs := make(chan os.Signal, 1)
	r.env.sigs = sigs
	r.env.sleep = r.clock.sleepFn(0, 200*time.Microsecond) // let the watcher run
	r.ex.onCall = func(line string) {
		if line == "pactl set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 30%" {
			sigs <- syscallTERM()
		}
	}
	err := r.run(findOpts{yes: true})
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v", err)
	}
	r.assertRestored(t)
	if v := r.setVolumes(); v[len(v)-1] > 40 {
		t.Fatalf("kept ramping after the signal: %v", v)
	}
	ev := r.eventList(t)
	if ev[len(ev)-1]["reason"] != "signal" {
		t.Fatalf("events %v", ev)
	}
}

func TestFindErrorRestores(t *testing.T) {
	r := newFindRig(t, neverKeys())
	r.ex.failOn = "set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 35%"
	err := r.run(findOpts{yes: true})
	if err == nil {
		t.Fatal("no error")
	}
	r.assertRestored(t)
	if r.beep.stopped != 1 {
		t.Fatalf("tone stopped %d times", r.beep.stopped)
	}
}

func TestFindPlayerDeathAbortsAndRestores(t *testing.T) {
	r := newFindRig(t, neverKeys())
	r.beep.dead = true
	err := r.run(findOpts{yes: true})
	if err == nil || !strings.Contains(err.Error(), "player died") {
		t.Fatalf("err = %v", err)
	}
	r.assertRestored(t)
}

func TestFindSinkDisconnectStops(t *testing.T) {
	r := newFindRig(t, neverKeys())
	r.ex.dropSinkAfter = 3
	err := r.run(findOpts{yes: true})
	if err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("err = %v", err)
	}
	if r.beep.stopped != 1 {
		t.Fatal("tone not stopped")
	}
}

func TestFindRefusesNonBluezSinks(t *testing.T) {
	for _, name := range []string{"alsa_output.pci-0000_00_1f.3.analog-stereo", "nonexistent"} {
		r := newFindRig(t, neverKeys())
		err := r.run(findOpts{yes: true, sinkName: name})
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if len(r.setVolumes()) != 0 || r.ex.count("playerctl --player=spotify pause") != 0 || r.beep.startedCount() != 0 {
			t.Fatalf("side effects before refusing %s: %v", name, r.ex.calls)
		}
	}
	// A bluez-named sink without api.bluez5.address is refused too.
	r := newFindRig(t, neverKeys())
	r.ex.out["pactl list sinks"] = strings.Replace(r.ex.out["pactl list sinks"], "\t\tapi.bluez5.address = \"AA:BB:CC:DD:EE:FF\"\n", "", 1)
	if err := r.run(findOpts{yes: true}); err == nil || !strings.Contains(err.Error(), "not identified") {
		t.Fatalf("err = %v", err)
	}
}

func TestFindConfirmation(t *testing.T) {
	r := newFindRig(t, seqKeys("n"))
	r.env.events = nil // interactive: the typed key answers the question
	err := r.run(findOpts{})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(r.out.String(), "Remove the earbuds from your ears before starting") {
		t.Errorf("no warning in %q", r.out.String())
	}
	if r.ex.count("playerctl --player=spotify pause") != 0 || len(r.setVolumes()) != 0 {
		t.Fatalf("side effects after declining: %v", r.ex.calls)
	}
	// y proceeds.
	first := true
	r = newFindRig(t, fnKeys{func() (byte, error) {
		if first {
			first = false
			return 'y', nil
		}
		select {}
	}})
	r.env.events = nil
	if err := r.run(findOpts{timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if len(r.setVolumes()) == 0 {
		t.Fatal("did not run after y")
	}
}

func TestFindEventsAreJSONLines(t *testing.T) {
	r := newFindRig(t, neverKeys())
	if err := r.run(findOpts{yes: true, timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range r.eventList(t) {
		names = append(names, e["event"].(string))
	}
	if strings.Join(names, ",") != "started,volume,volume,stopped" {
		t.Fatalf("events %v", names)
	}
	// Nothing else may reach the events stream.
	for _, ln := range strings.Split(strings.TrimSpace(r.events.String()), "\n") {
		if !strings.HasPrefix(ln, "{") {
			t.Fatalf("non-JSON line %q", ln)
		}
	}
}

func TestLineKeysStopAndEOF(t *testing.T) {
	k := newLineKeys(strings.NewReader("hello\nSTOP\n"))
	if b, err := k.ReadKey(); err != nil || b != 'q' {
		t.Fatalf("%v %v", b, err)
	}
	if _, err := k.ReadKey(); !errors.Is(err, io.EOF) {
		t.Fatalf("err %v", err)
	}
}

func TestParseFindArgs(t *testing.T) {
	o, ev, err := parseFindArgs([]string{"--yes"})
	if err != nil || ev || o.maxPct != 100 || o.startPct != 0 || o.timeout != 60*time.Second || !o.yes {
		t.Fatalf("%+v %v %v", o, ev, err)
	}
	o, ev, err = parseFindArgs([]string{"--yes", "--events", "--max-volume", "90%", "--timeout=30", "--sink", "x"})
	if err != nil || !ev || o.maxPct != 90 || o.timeout != 30*time.Second || o.sinkName != "x" {
		t.Fatalf("%+v %v %v", o, ev, err)
	}
	for _, bad := range [][]string{{"--events"}, {"--yes", "--max-volume", "0"}, {"--yes", "--max-volume", "101%"}, {"--yes", "--timeout", "0"}, {"extra"}} {
		if _, _, err := parseFindArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestBeepToneLevelAndShape(t *testing.T) {
	var g beepGen
	n := beepPatternFrames()
	buf := g.Next(n)
	if len(buf) != n*4 {
		t.Fatalf("len %d", len(buf))
	}
	peak, first := 0, int16(1)
	for i := 0; i+1 < len(buf); i += 2 {
		v := int(int16(uint16(buf[i]) | uint16(buf[i+1])<<8))
		if i == 0 {
			first = int16(v)
		}
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	if db := 20 * math.Log10(float64(peak)/32768); db > -2.9 || db < -3.1 {
		t.Fatalf("peak %.2f dBFS", db)
	}
	if first != 0 {
		t.Fatalf("first sample %d: no ramp", first)
	}
	// Two bursts of 150 ms separated by 100 ms of silence, then the rest.
	active := func(from, to int) bool {
		for i := from * 4; i < to*4; i += 4 {
			if buf[i] != 0 || buf[i+1] != 0 {
				return true
			}
		}
		return false
	}
	b, gap := msFrames(findBeepMs), msFrames(findGapMs)
	if !active(0, b) || active(b+10, b+gap-10) || !active(b+gap, 2*b+gap) || active(2*b+gap+10, n) {
		t.Fatal("pattern is not di-di + rest")
	}
	// The ramp: the first 10 ms rise, the fade-out ends at zero.
	f := g.Fade()
	if l := f[len(f)-4:]; l[0] != 0 || l[1] != 0 {
		t.Fatalf("fade does not end at 0: %v", l)
	}
}

func syscallTERM() os.Signal { return syscall.SIGTERM }

func TestFindElapsedComesFromTheClock(t *testing.T) {
	r := newFindRig(t, neverKeys())
	// Every sleep takes 500 ms of wall clock although the loop asks for 100.
	r.env.sleep = r.clock.sleepFn(500*time.Millisecond, 0)
	if err := r.run(findOpts{yes: true, timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if r.clock.n > 20 {
		t.Fatalf("%d sleeps for a 5 s timeout at 500 ms each", r.clock.n)
	}
	if v := r.setVolumes(); len(v) != 3 {
		t.Fatalf("volumes %v", v)
	}
	ev := r.eventList(t)
	if ev[len(ev)-1]["reason"] != "timeout" {
		t.Fatalf("events %v", ev)
	}
}

func TestFindStopBeforeTheToneNeverRaisesTheVolume(t *testing.T) {
	r := newFindRig(t, seqKeys("q")) // "stop", then EOF
	// Give the stdin reader time to deliver before the run reaches the volume.
	r.ex.onCall = func(line string) {
		if line == "playerctl -l" {
			time.Sleep(30 * time.Millisecond)
		}
	}
	if err := r.run(findOpts{yes: true}); err != nil {
		t.Fatal(err)
	}
	if v := r.setVolumes(); len(v) != 0 || r.ex.count("set-sink-mute") != 0 {
		t.Fatalf("volume touched: %v", r.ex.calls)
	}
	if r.beep.startedCount() != 0 {
		t.Fatal("tone started")
	}
	if r.ex.index("playerctl --player=spotify play") < 0 {
		t.Error("music not resumed")
	}
	ev := r.eventList(t)
	if len(ev) != 1 || ev[0]["event"] != "stopped" || ev[0]["reason"] != "stop" {
		t.Fatalf("events %v", ev)
	}
}

func TestFindStopDuringTheSilenceWait(t *testing.T) {
	var r *findRig
	keys := fnKeys{func() (byte, error) {
		for r.ex.count("pactl list sink-inputs") < 3 {
			time.Sleep(50 * time.Microsecond)
		}
		return 'q', nil
	}}
	r = newFindRig(t, keys)
	r.env.sleep = r.clock.sleepFn(0, time.Millisecond)
	r.ex.out["pactl list sinks"] = strings.Replace(r.ex.out["pactl list sinks"], "State: IDLE", "State: RUNNING", 1)
	r.ex.out["pactl list sink-inputs"] = "Sink Input #7\n\tSink: 92\n\tCorked: no\n\tMute: no\n\tVolume: front-left: 65536 / 100% / 0.00 dB\n"
	if err := r.run(findOpts{yes: true}); err != nil {
		t.Fatalf("stop reported as failure: %v", err)
	}
	if v := r.setVolumes(); len(v) != 0 {
		t.Fatalf("volume touched: %v", v)
	}
	if r.ex.index("playerctl --player=spotify play") < 0 {
		t.Error("music not resumed")
	}
	ev := r.eventList(t)
	if ev[len(ev)-1]["reason"] != "stop" {
		t.Fatalf("events %v", ev)
	}
}

func TestFindNewStreamAfterStartStopsAndRestores(t *testing.T) {
	r := newFindRig(t, neverKeys())
	r.ex.onCall = func(line string) {
		if line == "pactl set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 30%" {
			r.ex.out["pactl list sink-inputs"] += "\nSink Input #9\n\tSink: 92\n\tCorked: no\n"
		}
	}
	err := r.run(findOpts{yes: true})
	if err == nil || !strings.Contains(err.Error(), "new audio stream") {
		t.Fatalf("err = %v", err)
	}
	if v := r.setVolumes(); v[len(v)-1] != 30 && v[len(v)-1] != 35 {
		t.Fatalf("kept ramping: %v", v)
	}
	r.assertRestored(t)
	ev := r.eventList(t)
	if ev[len(ev)-1]["reason"] != "audio-started" {
		t.Fatalf("events %v", ev)
	}
}

func TestFindSinkAddressMayDifferByOneInLastOctet(t *testing.T) {
	set := func(addr string) *findRig {
		r := newFindRig(t, neverKeys())
		r.ex.out["pactl list sinks"] = strings.Replace(r.ex.out["pactl list sinks"], "AA:BB:CC:DD:EE:FF\"\n", addr+"\"\n", 1)
		return r
	}
	if err := set("AA:BB:CC:DD:EE:FE").run(findOpts{yes: true}); err != nil && strings.Contains(err.Error(), "reports address") {
		t.Fatalf("adjacent address refused: %v", err)
	}
	if err := set("AA:BB:CC:DD:EE:10").run(findOpts{yes: true}); err == nil || !strings.Contains(err.Error(), "reports address") {
		t.Fatalf("distant address accepted: %v", err)
	}
}

func TestParseFindArgsStartVolumeAndToneLevel(t *testing.T) {
	defer func(a float64) { findBeepAmp = a }(findBeepAmp)
	o, _, err := parseFindArgs([]string{"--yes", "--start-volume", "40%", "--tone-level", "-12"})
	if err != nil || o.startPct != 40 {
		t.Fatalf("%+v %v", o, err)
	}
	if db := 20 * math.Log10(findBeepAmp); db < -12.1 || db > -11.9 {
		t.Fatalf("tone level %.2f dBFS", db)
	}
	for _, bad := range [][]string{{"--yes", "--tone-level", "-2"}, {"--yes", "--tone-level", "-40"}, {"--yes", "--start-volume", "0"}} {
		if _, _, err := parseFindArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
