package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/backend/internal/hearing"
	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
)

const pactlSinks = "47\talsa_output.pci-0000_00_1f.3.analog-stereo\tPipeWire\ts32le 2ch 48000Hz\tSUSPENDED\n" +
	"92\tbluez_output.AA_BB_CC_DD_EE_FF.1\tPipeWire\ts16le 2ch 44100Hz\tRUNNING\n"

// ---------- fakes ----------

type fakeExec struct {
	mu     sync.Mutex
	calls  []string
	have   map[string]bool
	out    map[string]string // by full command line
	failOn string            // substring of a command line that fails
	vol    string
}

func newFakeExec() *fakeExec {
	return &fakeExec{
		have: map[string]bool{"pactl": true, "pw-cat": true, "playerctl": true},
		out: map[string]string{
			"pactl list short sinks":                               pactlSinks,
			"playerctl -l":                                         "spotify\nfirefox.instance123\nmpv\n",
			"playerctl --player=spotify status":                    "Playing\n",
			"playerctl --player=firefox.instance123 status":        "Paused\n",
			"playerctl --player=mpv status":                        "Playing\n",
			"pactl get-sink-mute bluez_output.AA_BB_CC_DD_EE_FF.1": "Mute: yes\n",
		},
		vol: "Volume: front-left: 39321 /  60% / -13.30 dB,   front-right: 36044 /  55% / -15.00 dB\n        balance -0.08\n",
	}
}

func (f *fakeExec) run(name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, line)
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		return "", errors.New("boom")
	}
	if strings.HasPrefix(line, "pactl get-sink-volume") {
		return f.vol, nil
	}
	return f.out[line], nil
}

func (f *fakeExec) index(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func (f *fakeExec) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

type fakeLink struct {
	mu       sync.Mutex
	events   []string
	verified bool
	modeErr  error
	writeErr error
}

func (l *fakeLink) rec(s string) {
	l.mu.Lock()
	l.events = append(l.events, s)
	l.mu.Unlock()
}
func (l *fakeLink) status() (map[string]interface{}, error) {
	return map[string]interface{}{"model": map[string]interface{}{"verified": l.verified}, "eq": map[string]interface{}{"slot": 6}}, nil
}
func (l *fakeLink) hearingMode(on bool) (*hearingCall, error) {
	if on {
		l.rec("mode:on")
	} else {
		l.rec("mode:off")
	}
	if on && l.modeErr != nil {
		return nil, l.modeErr
	}
	return &hearingCall{Sent: "ff0345010x"}, nil
}
func (l *fakeLink) hearingWrite(ear hearing.Ear, g, o [hearing.Bands]int) (*hearingCall, error) {
	l.rec("write:" + ear.String())
	if l.writeErr != nil {
		return nil, l.writeErr
	}
	return &hearingCall{Sent: "ff", Ack: false}, nil
}
func (l *fakeLink) eqRead() (*qcycore.EqSnapshot, error) {
	return &qcycore.EqSnapshot{Slot: 11, Effect: "Experiência auditiva", MasterGain: -10}, nil
}
func (l *fakeLink) ping()        {}
func (l *fakeLink) Close() error { return nil }

func (l *fakeLink) list() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.events, ",")
}

type fakePlayer struct {
	mu            sync.Mutex
	started, stop int
}

func (p *fakePlayer) Start(sink string, ear hearing.Ear, freq int, level float64) (func() error, error) {
	p.mu.Lock()
	p.started++
	p.mu.Unlock()
	return func() error { p.mu.Lock(); p.stop++; p.mu.Unlock(); return nil }, nil
}

type fnKeys struct{ next func() (byte, error) }

func (k fnKeys) ReadKey() (byte, error) { return k.next() }

// cycleKeys answers n,n,h,... forever (the staircase always terminates).
func cycleKeys() fnKeys {
	i := 0
	return fnKeys{func() (byte, error) {
		i++
		if i%3 == 0 {
			return 'h', nil
		}
		return 'n', nil
	}}
}

// pacedKeys hands out key i only once i+1 tones have started, as a listener
// would (the runner discards keys typed while no tone is playing).
func pacedKeys(pl *fakePlayer, s string) fnKeys {
	b := []byte(s)
	i := 0
	return fnKeys{func() (byte, error) {
		if i >= len(b) {
			return 0, io.EOF
		}
		for pl.startedCount() < i+1 {
			time.Sleep(time.Millisecond)
		}
		k := b[i]
		i++
		return k, nil
	}}
}

func seqKeys(s string) fnKeys {
	b := []byte(s)
	return fnKeys{func() (byte, error) {
		if len(b) == 0 {
			return 0, io.EOF
		}
		k := b[0]
		b = b[1:]
		return k, nil
	}}
}

type rig struct {
	env         hearingEnv
	ex          *fakeExec
	link        *fakeLink
	pl          *fakePlayer
	out         *bytes.Buffer
	cfg         string
	ttyRestored *int
}

func newRig(t *testing.T, keys keySource) *rig {
	t.Helper()
	ex := newFakeExec()
	lk := &fakeLink{verified: true}
	pl := &fakePlayer{}
	out := &bytes.Buffer{}
	cfg := t.TempDir()
	restored := new(int)
	return &rig{ex: ex, link: lk, pl: pl, out: out, cfg: cfg, ttyRestored: restored, env: hearingEnv{
		out: out, exec: ex.run,
		have:      func(n string) bool { return ex.have[n] },
		link:      func() (hearingLink, error) { return lk, nil },
		player:    pl,
		keys:      func() (keySource, func(), error) { return keys, func() { *restored++ }, nil },
		sigs:      make(chan os.Signal),
		cachedMAC: func() string { return "aa:bb:cc:dd:ee:ff" },
		now:       func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) },
		configDir: func() (string, error) { return cfg, nil },
	}}
}

func (r *rig) assertRestored(t *testing.T) {
	t.Helper()
	// Test mode: last mode event is off whenever on was sent.
	ev := r.link.list()
	if strings.Contains(ev, "mode:on") && !strings.HasSuffix(ev, "mode:off") && !strings.Contains(ev, "mode:off") {
		t.Errorf("test mode left on: %s", ev)
	}
	if r.ex.count("set-sink-volume") > 0 {
		// last volume command restores the original per-channel values.
		var last string
		r.ex.mu.Lock()
		for _, c := range r.ex.calls {
			if strings.Contains(c, "set-sink-volume") {
				last = c
			}
		}
		r.ex.mu.Unlock()
		if last != "pactl set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 60% 55%" {
			t.Errorf("volume not restored: %q", last)
		}
		if r.ex.index("set-sink-mute bluez_output.AA_BB_CC_DD_EE_FF.1 1") < 0 {
			t.Error("mute not restored")
		}
	}
	// Only the players this run paused are resumed.
	if r.ex.count(" pause") != r.ex.count(" play") {
		t.Errorf("pause/play mismatch: %v", r.ex.calls)
	}
	if r.ex.index("--player=firefox.instance123 pause") >= 0 || r.ex.index("--player=firefox.instance123 play") >= 0 {
		t.Error("touched a player that was not playing")
	}
	r.pl.mu.Lock()
	defer r.pl.mu.Unlock()
	if r.pl.started != r.pl.stop {
		t.Errorf("tones started %d stopped %d", r.pl.started, r.pl.stop)
	}
	if *r.ttyRestored != 1 {
		t.Errorf("terminal restored %d times", *r.ttyRestored)
	}
}

func opts() hearingRunOpts {
	return hearingRunOpts{soundDB: hearing.DefaultSoundDB, sinkVolume: 100}
}

// ---------- tests ----------

func TestHearingRunFullTestWritesNothingWithoutApply(t *testing.T) {
	r := newRig(t, cycleKeys())
	if err := hearingRunLive(r.env, opts()); err != nil {
		t.Fatal(err)
	}
	if ev := r.link.list(); ev != "mode:on,mode:off" {
		t.Fatalf("events %s", ev)
	}
	r.assertRestored(t)
	if r.ex.count(" pause") != 2 || r.ex.count(" play") != 2 {
		t.Fatalf("players: %v", r.ex.calls)
	}
	if !strings.Contains(r.out.String(), "nothing was written") {
		t.Fatal(r.out.String())
	}
	if r.ex.index("set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 100%") < 0 {
		t.Fatalf("test volume not set: %v", r.ex.calls)
	}
	// Order: pause before the volume changes, which come before nothing audible.
	if r.ex.index("pause") > r.ex.index("set-sink-volume") {
		t.Fatal("music must be paused first")
	}
}

func TestHearingRunApplyBacksUpThenWritesBothEarsThenReadsBack(t *testing.T) {
	r := newRig(t, cycleKeys())
	outFile := filepath.Join(t.TempDir(), "r.json")
	o := opts()
	o.apply, o.yes, o.outFile = true, true, outFile
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if ev := r.link.list(); ev != "mode:on,write:left,write:right,mode:off" {
		t.Fatalf("events %s", ev)
	}
	bk := filepath.Join(r.cfg, "backups", "hearing-before-20261013-120000.json")
	b, err := os.ReadFile(bk)
	if err != nil || !strings.Contains(string(b), `"verified": true`) {
		t.Fatalf("backup: %v %s", err, b)
	}
	out := r.out.String()
	if !strings.Contains(out, "qcyctl library apply backup-antes-do-teste-auditivo --yes") || !strings.Contains(out, `slot 11 "Experiência auditiva"`) {
		t.Fatal(out)
	}
	if st, err := os.Stat(outFile); err != nil || st.Size() == 0 {
		t.Fatal("--out not written", err)
	}
	r.assertRestored(t)
}

func TestHearingRunApplyWithoutYesWritesNothing(t *testing.T) {
	r := newRig(t, cycleKeys())
	o := opts()
	o.apply = true
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.link.list(), "write") {
		t.Fatal(r.link.list())
	}
	if _, err := os.Stat(filepath.Join(r.cfg, "backups")); err == nil {
		t.Fatal("no backup expected when nothing is written")
	}
}

func TestHearingRunBackupFailureAbortsWrite(t *testing.T) {
	r := newRig(t, cycleKeys())
	f := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(f, nil, 0o600)
	r.env.configDir = func() (string, error) { return f, nil } // MkdirAll under a file fails
	o := opts()
	o.apply, o.yes = true, true
	err := hearingRunLive(r.env, o)
	if err == nil || strings.Contains(r.link.list(), "write") {
		t.Fatalf("err %v events %s", err, r.link.list())
	}
	r.assertRestored(t)
}

func TestHearingRunAbortKeyRestoresEverything(t *testing.T) {
	r := newRig(t, nil)
	r.env.keys = func() (keySource, func(), error) { return pacedKeys(r.pl, "nnq"), func() { *r.ttyRestored++ }, nil }
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatal(err)
	}
	if ev := r.link.list(); ev != "mode:on,mode:off" {
		t.Fatalf("events %s", ev)
	}
	r.assertRestored(t)
}

func TestHearingRunInterruptRestoresEverything(t *testing.T) {
	block := make(chan struct{})
	keys := fnKeys{func() (byte, error) { <-block; return 0, io.EOF }}
	t.Cleanup(func() { close(block) })
	r := newRig(t, keys)
	sigs := make(chan os.Signal, 1)
	r.env.sigs = sigs
	go func() {
		for r.pl.startedCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		sigs <- syscall.SIGINT
	}()
	err := hearingRunLive(r.env, opts())
	if !errors.Is(err, errInterrupted) {
		t.Fatal(err)
	}
	r.assertRestored(t)
}

func (p *fakePlayer) startedCount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.started }

func TestHearingRunClosedInputRestoresEverything(t *testing.T) {
	r := newRig(t, seqKeys("n"))
	if err := hearingRunLive(r.env, opts()); err == nil {
		t.Fatal("expected error on EOF")
	}
	r.assertRestored(t)
}

func TestHearingRunModeOnFailureStillSwitchesOffAndRestores(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.link.modeErr = errors.New("no link")
	if err := hearingRunLive(r.env, opts()); err == nil {
		t.Fatal("expected error")
	}
	if ev := r.link.list(); ev != "mode:on,mode:off" {
		t.Fatalf("events %s", ev)
	}
	r.assertRestored(t)
	if r.pl.startedCount() != 0 {
		t.Fatal("no tone may play")
	}
}

func TestHearingRunWriteFailureStillRestores(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.link.writeErr = errors.New("device said no")
	o := opts()
	o.apply, o.yes = true, true
	if err := hearingRunLive(r.env, o); err == nil {
		t.Fatal("expected error")
	}
	if ev := r.link.list(); ev != "mode:on,write:left,mode:off" {
		t.Fatalf("events %s", ev)
	}
	r.assertRestored(t)
}

func TestHearingRunNoSinkTouchesNothing(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.out["pactl list short sinks"] = "47\talsa_output.pci.analog-stereo\tPipeWire\ts32le\tSUSPENDED\n"
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "no Bluetooth sink") {
		t.Fatal(err)
	}
	if r.link.list() != "" || r.ex.count("set-") != 0 || r.ex.count("pause") != 0 || r.pl.startedCount() != 0 {
		t.Fatalf("side effects: %s %v", r.link.list(), r.ex.calls)
	}
}

func TestHearingRunUnverifiedModelRefused(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.link.verified = false
	if err := hearingRunLive(r.env, opts()); err == nil || r.link.list() != "" || r.ex.count("pause") != 0 {
		t.Fatalf("%v %s", err, r.link.list())
	}
}

func TestHearingRunWithoutPlayerctlWarnsAndContinues(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.have["playerctl"] = false
	if err := hearingRunLive(r.env, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "pause any music manually") || r.ex.count("playerctl") != 0 {
		t.Fatal(r.out.String())
	}
}

func TestHearingRunRepeatReplaysSameTone(t *testing.T) {
	r := newRig(t, nil)
	r.env.keys = func() (keySource, func(), error) { return pacedKeys(r.pl, "rrq"), func() { *r.ttyRestored++ }, nil }
	_ = hearingRunLive(r.env, opts())
	if n := r.pl.startedCount(); n != 3 {
		t.Fatalf("tone started %d times, want 3", n)
	}
}

func TestParseBluezSinksAndPick(t *testing.T) {
	sinks := parseBluezSinks(pactlSinks)
	if len(sinks) != 1 || sinks[0] != "bluez_output.AA_BB_CC_DD_EE_FF.1" {
		t.Fatal(sinks)
	}
	two := append(append([]string{}, sinks...), "bluez_output.11_22_33_44_55_66.1")
	if s, w, err := pickSink(two, "", "aa:bb:cc:dd:ee:ff"); err != nil || s != sinks[0] || w != "" {
		t.Fatal(s, w, err)
	}
	if _, _, err := pickSink(two, "", "00:00:00:00:00:00"); err == nil {
		t.Fatal("ambiguous sinks must fail")
	}
	if s, w, err := pickSink(sinks, "", ""); err != nil || s != sinks[0] || w == "" {
		t.Fatal("single sink is a warned fallback", s, w, err)
	}
	if _, _, err := pickSink(sinks, "bluez_output.nope", ""); err == nil {
		t.Fatal("unknown explicit sink must fail")
	}
	if _, _, err := pickSink(nil, "", "x"); err == nil {
		t.Fatal("no sink must fail")
	}
}

func TestParseSinkVolumeAndMute(t *testing.T) {
	v, err := parseSinkVolume(newFakeExec().vol)
	if err != nil || len(v) != 2 || v[0] != 60 || v[1] != 55 {
		t.Fatal(v, err)
	}
	if _, err := parseSinkVolume("nothing"); err == nil {
		t.Fatal("garbage must fail")
	}
	if m, err := parseMute("Mute: yes\n"); err != nil || !m {
		t.Fatal(m, err)
	}
	if m, err := parseMute("Mute: no\n"); err != nil || m {
		t.Fatal(m, err)
	}
	if _, err := parseMute("?"); err == nil {
		t.Fatal("garbage must fail")
	}
}

func TestHearingRunCmdFlags(t *testing.T) {
	var b bytes.Buffer
	for _, args := range [][]string{
		{"run", "--sink-volume", "150%"},
		{"run", "--sink-volume", "0"},
		{"run", "--answers", "n"},
		{"run", "--json"},
	} {
		if err := hearingMain(args, &b); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// A valid real run in the test sandbox stops at the hardware guard.
	if err := hearingMain([]string{"run"}, &b); err == nil || !strings.Contains(err.Error(), errHardwareDisabled.Error()) {
		t.Fatalf("real run reached outside the sandbox: %v", err)
	}
}
