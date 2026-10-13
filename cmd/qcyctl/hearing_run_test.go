package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/hearing"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
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
	// dropSinkAfter > 0: from the Nth `pactl list short sinks` on, the
	// earbuds' sink is gone.
	dropSinkAfter int
	sinkListCalls int
	onCall        func(line string)
	ignorePause   map[string]bool
}

func newFakeExec() *fakeExec {
	return &fakeExec{
		have: map[string]bool{"pactl": true, "pw-cat": true, "playerctl": true, "parec": true},
		out: map[string]string{
			"pactl list short sinks":                               pactlSinks,
			"playerctl -l":                                         "spotify\nfirefox.instance123\nmpv\n",
			"playerctl --player=spotify status":                    "Playing\n",
			"playerctl --player=firefox.instance123 status":        "Paused\n",
			"playerctl --player=mpv status":                        "Playing\n",
			"pactl get-sink-mute bluez_output.AA_BB_CC_DD_EE_FF.1": "Mute: yes\n",
			"pactl list sinks":                                     "Sink #47\n\tName: alsa_output.pci.analog-stereo\n\tDescription: Built-in\n\nSink #92\n\tState: IDLE\n\tName: bluez_output.AA_BB_CC_DD_EE_FF.1\n\tDescription: QCY MeloBuds N70\n\tMute: no\n\tVolume: front-left: 65536 / 100% / 0.00 dB,   front-right: 65536 / 100% / 0.00 dB\n\tProperties:\n\t\tapi.bluez5.address = \"AA:BB:CC:DD:EE:FF\"\n\t\tdevice.description = \"QCY MeloBuds N70\"\n",
			"pactl list sink-inputs":                               "Sink Input #5\n\tSink: 47\n\tCorked: no\n\nSink Input #6\n\tSink: 92\n\tCorked: yes\n",
		},
		vol: "Volume: front-left: 39321 /  60% / -13.30 dB,   front-right: 36044 /  55% / -15.00 dB\n        balance -0.08\n",
	}
}

func (f *fakeExec) run(name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, line)
	if f.onCall != nil {
		f.onCall(line)
	}
	if strings.HasPrefix(line, "playerctl --player=") {
		f2 := strings.Fields(line)
		if len(f2) == 3 && (f2[2] == "pause" || f2[2] == "play") && !f.ignorePause[f2[1]] {
			st := "Paused\n"
			if f2[2] == "play" {
				st = "Playing\n"
			}
			f.out["playerctl "+f2[1]+" status"] = st
		}
	}
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		return "", errors.New("boom")
	}
	if line == "pactl list short sinks" {
		f.sinkListCalls++
		if f.dropSinkAfter > 0 && f.sinkListCalls >= f.dropSinkAfter {
			return "47\talsa_output.pci-0000_00_1f.3.analog-stereo\tPipeWire\ts32le\tSUSPENDED\n", nil
		}
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
	eqErr    error
	// failWriteOn: the Nth write (1-based) fails; 0 = writeErr applies to all.
	failWriteOn int
	nWrites     int
	snapPayload string
	pingPanic   bool
	onMode      func(on bool)
	statusCalls int
	eqCalls     int
	closed      bool
	// usedAfterClose: some call hit the link after Close.
	usedAfterClose bool
	modeOffErr     error
	ack            bool
	noReadback     bool
	badReadback    bool
}

func (l *fakeLink) rec(s string) {
	l.mu.Lock()
	l.events = append(l.events, s)
	l.mu.Unlock()
}

// useClosed reports (and records) a call made after Close, which fails like a
// real closed socket would.
func (l *fakeLink) useClosed() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.usedAfterClose = true
		return errors.New("use of closed connection")
	}
	return nil
}

func (l *fakeLink) status() (map[string]interface{}, error) {
	if err := l.useClosed(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.statusCalls++
	l.mu.Unlock()
	return map[string]interface{}{"model": map[string]interface{}{"verified": l.verified}, "eq": map[string]interface{}{"slot": 6}}, nil
}
func (l *fakeLink) capabilities() (bool, error) {
	if err := l.useClosed(); err != nil {
		return false, err
	}
	return l.verified, nil
}
func (l *fakeLink) hearingMode(on bool) (*hearingCall, error) {
	if err := l.useClosed(); err != nil {
		if !on {
			l.rec("mode:off-FAILED-closed")
		}
		return nil, err
	}
	if on {
		l.rec("mode:on")
	} else {
		l.rec("mode:off")
	}
	if l.onMode != nil {
		l.onMode(on)
	}
	if !on && l.modeOffErr != nil {
		return nil, l.modeOffErr
	}
	if on && l.modeErr != nil {
		return nil, l.modeErr
	}
	return &hearingCall{Sent: "ff0345010x"}, nil
}
func (l *fakeLink) hearingWrite(ear hearing.Ear, g, o [hearing.Bands]int) (*hearingCall, error) {
	if err := l.useClosed(); err != nil {
		return nil, err
	}
	l.rec("write:" + ear.String())
	l.mu.Lock()
	l.nWrites++
	n := l.nWrites
	l.mu.Unlock()
	if l.failWriteOn > 0 && n == l.failWriteOn {
		return nil, errors.New("device said no")
	}
	if l.writeErr != nil {
		return nil, l.writeErr
	}
	// The device reports the curve it was given (slot 11, same band layout
	// as the 0x46 body) unless the test says otherwise.
	mg := hearing.MasterGain(toF(g))
	if m2 := hearing.MasterGain(toF(o)); m2 < mg {
		mg = m2
	}
	mg, _ = hearing.ClampMaster(mg)
	c := &hearingCall{Sent: "ff", Ack: l.ack, MasterGain: &mg}
	if !l.noReadback {
		pl := hex.EncodeToString(hearing.WriteFrame(ear, toF(g), mg)[4:])
		if l.badReadback {
			pl = hex.EncodeToString(make([]byte, 143))
		}
		c.Readback = &struct {
			Slot       int     `json:"slot"`
			Effect     string  `json:"effect,omitempty"`
			MasterGain float64 `json:"masterGain"`
			Payload    string  `json:"payload"`
		}{Slot: 11, Effect: "Experiência auditiva", MasterGain: mg, Payload: pl}
	}
	return c, nil
}

func toF(g [hearing.Bands]int) [hearing.Bands]float64 {
	var o [hearing.Bands]float64
	for i, v := range g {
		o[i] = float64(v)
	}
	return o
}
func (l *fakeLink) eqRead() (*qcycore.EqSnapshot, error) {
	if err := l.useClosed(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.eqCalls++
	l.mu.Unlock()
	if l.eqErr != nil {
		return nil, l.eqErr
	}
	return &qcycore.EqSnapshot{Payload: l.snapPayload, Slot: 11, Effect: "Experiência auditiva", MasterGain: -10}, nil
}
func (l *fakeLink) ping() {
	if l.pingPanic {
		panic("ping boom")
	}
}
func (l *fakeLink) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return nil
}

func (l *fakeLink) list() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.events, ",")
}

type fakePlayer struct {
	mu            sync.Mutex
	started, stop int
	// deadAtStart: the Nth start (1-based) is dead at once; all: every start.
	deadAtStart map[int]bool
	allDead     bool
	// stopErr: the Nth tone's stop reports a failed playback.
	stopErr map[int]error
	// deadCh: the Nth start's dead channel is handed to the test, which
	// closes it when it wants the tone to die.
	deadCh map[int]chan struct{}
	// onStop runs inside the Nth tone's stop.
	onStop func(n int)
}

func (p *fakePlayer) Start(sink string, ear hearing.Ear, freq int, level float64) (func() error, <-chan struct{}, error) {
	p.mu.Lock()
	p.started++
	n := p.started
	dead := make(chan struct{})
	if p.deadAtStart[n] || p.allDead {
		close(dead)
	}
	if p.deadCh != nil {
		if _, ok := p.deadCh[n]; ok {
			p.deadCh[n] = dead
		}
	}
	err := p.stopErr[n]
	p.mu.Unlock()
	return func() error {
		p.mu.Lock()
		p.stop++
		hook := p.onStop
		p.mu.Unlock()
		if hook != nil {
			hook(n)
		}
		return err
	}, dead, nil
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
	saved       []string
	ttyOptional bool
	saveErr     error
}

func newRig(t *testing.T, keys keySource) *rig {
	t.Helper()
	ex := newFakeExec()
	lk := &fakeLink{verified: true}
	pl := &fakePlayer{}
	out := &bytes.Buffer{}
	cfg := t.TempDir()
	restored := new(int)
	rg := &rig{ex: ex, link: lk, pl: pl, out: out, cfg: cfg, ttyRestored: restored}
	rg.env = hearingEnv{
		out: out, exec: ex.run,
		have:      func(n string) bool { return ex.have[n] },
		link:      func() (hearingLink, error) { return lk, nil },
		player:    pl,
		keys:      func() (keySource, func(), error) { return keys, func() { *restored++ }, nil },
		sigs:      make(chan os.Signal),
		cachedMAC: func() string { return "aa:bb:cc:dd:ee:ff" },
		now:       func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) },
		configDir: func() (string, error) { return cfg, nil },
		sleep:     func(time.Duration) {},
		saveCurve: func(name string, snap *qcycore.EqSnapshot) (string, error) {
			if rg.saveErr != nil {
				return "", rg.saveErr
			}
			rg.saved = append(rg.saved, name)
			return "backup-antes-do-teste-auditivo-" + strings.ReplaceAll(strings.ReplaceAll(name[len(name)-15:], " ", ""), "-", ""), nil
		},
	}
	return rg
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
		if last != "pactl set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 39321 36044" {
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
	if *r.ttyRestored > 1 || (*r.ttyRestored == 0 && !r.ttyOptional) {
		t.Errorf("terminal restored %d times", *r.ttyRestored)
	}
}

func opts() hearingRunOpts {
	return hearingRunOpts{soundDB: hearing.DefaultSoundDB, sinkVolume: 100, deviceMode: true, acceptFloor: true}
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
	r.link.snapPayload = "0b50fb"
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
	if !strings.Contains(out, "qcyctl library apply backup-antes-do-teste-auditivo-") || len(r.saved) != 1 {
		t.Fatal(out)
	}
	if !strings.Contains(string(b), `"payload": "0b50fb"`) || !strings.Contains(string(b), `"libraryId"`) {
		t.Fatalf("backup lacks the raw payload / library id: %s", b)
	}
	if !strings.Contains(out, `slot 11 "Experiência auditiva"`) {
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

func TestHearingRunBackupFailureDirAbortsBeforeAnything(t *testing.T) {
	r := newRig(t, cycleKeys())
	f := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(f, nil, 0o600)
	r.env.configDir = func() (string, error) { return f, nil } // MkdirAll under a file fails
	o := opts()
	o.apply, o.yes = true, true
	if err := hearingRunLive(r.env, o); err == nil {
		t.Fatal("expected error")
	}
	r.untouched(t)
	r.ttyOptional = true
	r.assertRestored(t) // the music paused first is resumed
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
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "verified model") {
		t.Fatalf("%v", err)
	}
	r.untouched(t)
	r.ttyOptional = true
	r.assertRestored(t) // the music paused first is resumed
}

// Startup order: the music is paused BEFORE the daemon is touched, and the
// slow full status is neither read nor needed without --apply --yes.
func TestHearingRunStartupOrderAndNoHeavyStatus(t *testing.T) {
	r := newRig(t, cycleKeys())
	pausedBeforeDial := false
	r.env.link = func() (hearingLink, error) {
		pausedBeforeDial = r.ex.index(" pause") >= 0 && r.ex.index("set-sink-volume") < 0
		return r.link, nil
	}
	if err := hearingRunLive(r.env, opts()); err != nil {
		t.Fatal(err)
	}
	if !pausedBeforeDial {
		t.Fatal("the daemon was dialled before the music was paused")
	}
	if r.link.statusCalls != 0 || r.link.eqCalls != 0 {
		t.Fatalf("status/eqRead used without --apply: %d %d", r.link.statusCalls, r.link.eqCalls)
	}
	out := r.out.String()
	i, j, k := strings.Index(out, "pausing music"), strings.Index(out, "checking the earbuds"), strings.Index(out, "sink:")
	if i < 0 || j < 0 || !(k < i && i < j) {
		t.Fatalf("progress order wrong (%d %d %d):\n%s", k, i, j, out)
	}
}

// With --apply --yes the backup is read while the audio settles but is
// complete (and on disk) before the volume is raised and before 0x45.
func TestHearingRunBackupJoinedBeforeVolumeAndMode(t *testing.T) {
	r := newRig(t, cycleKeys())
	var atVolume, atMode bool
	r.ex.onCall = func(line string) {
		if strings.Contains(line, "set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 100%") {
			ents, _ := os.ReadDir(filepath.Join(r.cfg, "backups"))
			atVolume = len(ents) == 1
		}
	}
	r.link.onMode = func(on bool) {
		if on {
			ents, _ := os.ReadDir(filepath.Join(r.cfg, "backups"))
			atMode = len(ents) == 1
		}
	}
	o := opts()
	o.apply, o.yes = true, true
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if !atVolume || !atMode || r.link.statusCalls != 1 || r.link.eqCalls < 1 {
		t.Fatalf("volume %v mode %v status %d eq %d", atVolume, atMode, r.link.statusCalls, r.link.eqCalls)
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
	if s, err := pickSink(two, "", "aa:bb:cc:dd:ee:ff"); err != nil || s != sinks[0] {
		t.Fatal(s, err)
	}
	if _, err := pickSink(two, "", "00:00:00:00:00:00"); err == nil {
		t.Fatal("unmatched MAC must fail")
	}
	if _, err := pickSink(sinks, "", ""); err == nil {
		t.Fatal("the only sink is NOT assumed to be the earbuds")
	}
	if s, err := pickSink(sinks, sinks[0], ""); err != nil || s != sinks[0] {
		t.Fatal(s, err)
	}
	if _, err := pickSink(sinks, "bluez_output.nope", ""); err == nil {
		t.Fatal("unknown explicit sink must fail")
	}
	if _, err := pickSink(nil, "", "x"); err == nil {
		t.Fatal("no sink must fail")
	}
}

func TestParseSinkPropsAndStreams(t *testing.T) {
	ex := newFakeExec()
	p := parseSinkProps(ex.out["pactl list sinks"], "bluez_output.AA_BB_CC_DD_EE_FF.1")
	if p["api.bluez5.address"] != "AA:BB:CC:DD:EE:FF" || p["Description"] != "QCY MeloBuds N70" {
		t.Fatal(p)
	}
	if parseSinkProps(ex.out["pactl list sinks"], "nope") != nil {
		t.Fatal("unknown sink")
	}
	if a := activeStreamsOn(ex.out["pactl list sink-inputs"], "92"); len(a) != 0 {
		t.Fatalf("corked stream counted: %v", a)
	}
	if a := activeStreamsOn(ex.out["pactl list sink-inputs"], "47"); len(a) != 1 {
		t.Fatal(a)
	}
}

func TestParseSinkVolumeAndMute(t *testing.T) {
	v, err := parseSinkVolume(newFakeExec().vol)
	if err != nil || len(v) != 2 || v[0] != "39321" || v[1] != "36044" {
		t.Fatal(v, err)
	}
	if v, err := parseSinkVolume("Volume: mono: 60%\n"); err != nil || v[0] != "60%" {
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

func (r *rig) untouched(t *testing.T) {
	t.Helper()
	if r.ex.count("set-sink") != 0 || r.link.list() != "" || r.pl.startedCount() != 0 {
		t.Fatalf("side effects: %q %v", r.link.list(), r.ex.calls)
	}
}

func TestHearingRunNoMprisToolAborts(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.have["playerctl"] = false
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "neither playerctl nor busctl") {
		t.Fatal(err)
	}
	r.untouched(t)
}

func TestHearingRunBusctlFallbackPausesAndResumes(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.have["playerctl"] = false
	r.ex.have["busctl"] = true
	r.ex.out["busctl --user list --no-legend"] = "org.mpris.MediaPlayer2.spotify 123 spotify u -\norg.freedesktop.Notifications 5 x u -\norg.mpris.MediaPlayer2.mpv 9 mpv u -\n"
	get := func(p string) string {
		return "busctl --user get-property " + p + " /org/mpris/MediaPlayer2 org.mpris.MediaPlayer2.Player PlaybackStatus"
	}
	r.ex.out[get("org.mpris.MediaPlayer2.spotify")] = "s \"Playing\"\n"
	r.ex.out[get("org.mpris.MediaPlayer2.mpv")] = "s \"Paused\"\n"
	// after the pause call the status must read Paused: flip it on the call.
	r.ex.onCall = func(line string) {
		if strings.HasSuffix(line, "org.mpris.MediaPlayer2.Player Pause") && strings.Contains(line, "spotify") {
			r.ex.out[get("org.mpris.MediaPlayer2.spotify")] = "s \"Paused\"\n"
		}
	}
	if err := hearingRunLive(r.env, opts()); err != nil {
		t.Fatal(err)
	}
	if r.ex.count("call org.mpris.MediaPlayer2.spotify /org/mpris/MediaPlayer2 org.mpris.MediaPlayer2.Player Pause") != 1 ||
		r.ex.count("call org.mpris.MediaPlayer2.spotify /org/mpris/MediaPlayer2 org.mpris.MediaPlayer2.Player Play") != 1 ||
		r.ex.count("MediaPlayer2.mpv /org/mpris/MediaPlayer2 org.mpris.MediaPlayer2.Player Pause") != 0 {
		t.Fatalf("%v", r.ex.calls)
	}
}

func TestHearingRunStillPlayingAfterPauseAborts(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ttyOptional = true
	r.ex.ignorePause = map[string]bool{"--player=mpv": true} // ignores pause
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "still Playing") {
		t.Fatal(err)
	}
	if r.ex.count("set-sink-volume") != 0 || r.pl.startedCount() != 0 || r.link.list() != "" {
		t.Fatal("volume raised / tone played / mode set")
	}
	if r.ex.count(" play") == 0 {
		t.Fatal("paused players must be resumed")
	}
	r.assertRestored(t)
}

func TestHearingRunActiveStreamOnSinkAborts(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ttyOptional = true
	r.ex.out["pactl list sink-inputs"] = "Sink Input #9\n\tSink: 92\n\tCorked: no\n\t\tapplication.name = \"Firefox\"\n"
	r.ex.out["pactl list sinks"] = strings.Replace(r.ex.out["pactl list sinks"], "State: IDLE", "State: RUNNING", 1)
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "still playing into") || !strings.Contains(err.Error(), "Firefox") {
		t.Fatal(err)
	}
	if r.ex.count("set-sink-volume") != 0 || r.pl.startedCount() != 0 || r.link.list() != "" {
		t.Fatal("volume raised / tone played / mode set")
	}
	if r.ex.count(" play") != 2 {
		t.Fatalf("paused players must be resumed: %v", r.ex.calls)
	}
	r.assertRestored(t)
}

func TestHearingRunSingleUnmatchedSinkRefused(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.env.cachedMAC = func() string { return "" }
	if err := hearingRunLive(r.env, opts()); err == nil || !strings.Contains(err.Error(), "--sink") {
		t.Fatal(err)
	}
	r.untouched(t)
	o := opts()
	o.sinkName = "bluez_output.AA_BB_CC_DD_EE_FF.1"
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatalf("explicit --sink must work: %v", err)
	}
}

func TestHearingRunSinkIdentityMismatchRefused(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.out["pactl list sinks"] = strings.Replace(r.ex.out["pactl list sinks"], "AA:BB:CC:DD:EE:FF", "11:22:33:44:55:66", 1)
	if err := hearingRunLive(r.env, opts()); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	r.untouched(t)
}

func TestHearingRunSinkDisappearingAborts(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.dropSinkAfter = 3
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatal(err)
	}
	r.assertRestored(t)
}

func TestHearingRunNoDeviceModeWithoutFlag(t *testing.T) {
	r := newRig(t, cycleKeys())
	o := opts()
	o.deviceMode = false
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if r.link.list() != "" {
		t.Fatalf("0x45 sent without --device-mode: %s", r.link.list())
	}
	r.assertRestored(t)
}

func TestHearingRunApplyImpliesDeviceMode(t *testing.T) {
	r := newRig(t, cycleKeys())
	o := opts()
	o.deviceMode, o.apply, o.yes = false, true, true
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if ev := r.link.list(); ev != "mode:on,write:left,write:right,mode:off" {
		t.Fatal(ev)
	}
}

func TestHearingRunBackupFailuresAbortBeforeAnythingElse(t *testing.T) {
	for name, setup := range map[string]func(r *rig){
		"eqRead":  func(r *rig) { r.link.eqErr = errors.New("no eq") },
		"library": func(r *rig) { r.saveErr = errors.New("disk full") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, cycleKeys())
			setup(r)
			o := opts()
			o.apply, o.yes = true, true
			if err := hearingRunLive(r.env, o); err == nil {
				t.Fatal("expected error")
			}
			// Neither 0x45, nor volume, nor a tone; the music paused first is
			// resumed.
			r.untouched(t)
			r.ttyOptional = true
			r.assertRestored(t)
		})
	}
}

func TestHearingRunBackupIsPersistedBeforeModeOn(t *testing.T) {
	r := newRig(t, cycleKeys())
	var atModeOn bool
	r.link.onMode = func(on bool) {
		if on {
			ents, _ := os.ReadDir(filepath.Join(r.cfg, "backups"))
			atModeOn = len(ents) == 1 && len(r.saved) == 1
		}
	}
	o := opts()
	o.apply, o.yes = true, true
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if !atModeOn {
		t.Fatal("backup was not on disk when 0x45 was sent")
	}
}

func TestHearingRunModeOffFailurePrintsRestoreCommand(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.link.modeOffErr = errors.New("link lost")
	o := opts()
	o.apply, o.yes = true, true
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	out := r.out.String()
	i := strings.Index(out, "could not switch the test mode off")
	if i < 0 || !strings.Contains(out[i:], "qcyctl library apply backup-antes-do-teste-auditivo-") {
		t.Fatal(out)
	}
}

func TestHearingRunUnconfirmedWriteIsNotSuccess(t *testing.T) {
	for name, setup := range map[string]func(l *fakeLink){
		"no echo, wrong readback": func(l *fakeLink) { l.badReadback = true },
		"no echo, no readback":    func(l *fakeLink) { l.noReadback = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, cycleKeys())
			setup(r.link)
			o := opts()
			o.apply, o.yes = true, true
			err := hearingRunLive(r.env, o)
			out := r.out.String()
			if err == nil || !strings.Contains(err.Error(), "NOT CONFIRMED") || !strings.Contains(out, "NOT CONFIRMED") ||
				!strings.Contains(out, "qcyctl library apply backup-antes-do-teste-auditivo-") {
				t.Fatalf("%v\n%s", err, out)
			}
			r.assertRestored(t)
		})
	}
	// An echo alone confirms.
	r := newRig(t, cycleKeys())
	r.link.ack, r.link.noReadback = true, true
	o := opts()
	o.apply, o.yes = true, true
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
}

func TestWriteConfirmedMatchesBandsAndMaster(t *testing.T) {
	g := [hearing.Bands]int{3, 4, 2, 1, 0, -2, 0, 5}
	mg := -10.0
	pl := hex.EncodeToString(hearing.WriteFrame(hearing.Left, toF(g), mg)[4:])
	call := &hearingCall{MasterGain: &mg}
	call.Readback = &struct {
		Slot       int     `json:"slot"`
		Effect     string  `json:"effect,omitempty"`
		MasterGain float64 `json:"masterGain"`
		Payload    string  `json:"payload"`
	}{Slot: 11, MasterGain: -10, Payload: pl}
	if !writeConfirmed(call, g) {
		t.Fatal("matching readback rejected")
	}
	g2 := g
	g2[7] = 6
	if writeConfirmed(call, g2) {
		t.Fatal("a different band accepted")
	}
	call.Readback.MasterGain = -9
	if writeConfirmed(call, g) {
		t.Fatal("a different master accepted")
	}
}

func TestHearingRunPartialWriteReportsMixedState(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.link.failWriteOn = 2
	o := opts()
	o.apply, o.yes = true, true
	err := hearingRunLive(r.env, o)
	out := r.out.String()
	if err == nil || !strings.Contains(out, "MIXED STATE") || !strings.Contains(out, "qcyctl library apply backup-antes-do-teste-auditivo-") {
		t.Fatalf("%v\n%s", err, out)
	}
	r.assertRestored(t)
}

func TestHearingRunSignalDuringPreparationAborts(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		r := newRig(t, cycleKeys())
		sigs := make(chan os.Signal, 1)
		sigs <- sig
		r.env.sigs = sigs
		r.ttyOptional = true // the abort may be seen before the terminal is opened
		if err := hearingRunLive(r.env, opts()); !errors.Is(err, errInterrupted) {
			t.Fatalf("%v: %v", sig, err)
		}
		r.assertRestored(t)
	}
}

func TestHearingRunPanicInKeepaliveCleansUp(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	r := newRig(t, fnKeys{func() (byte, error) { <-block; return 0, io.EOF }})
	r.link.pingPanic = true
	r.env.pingEvery = time.Millisecond
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatal(err)
	}
	r.assertRestored(t)
}

func TestHearingRunMasterGainIsClamped(t *testing.T) {
	var all [hearing.Bands]float64
	for i := range all {
		all[i] = 6
	}
	res := computeResult(all, all)
	if !res.MasterClamped || res.MasterGain != -12 || res.MasterRaw >= -12 {
		t.Fatalf("%+v", res.MasterGain)
	}
	var b bytes.Buffer
	printResultText(&b, res)
	if !strings.Contains(b.String(), "WARNING") {
		t.Fatal(b.String())
	}
	if z := computeResult([hearing.Bands]float64{}, [hearing.Bands]float64{}); z.MasterClamped {
		t.Fatal("flat curve clamped")
	}
}

// A pw-cat that never reads stdin must not hang the stop.
func TestPwcatStopNeverHangs(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-pw-cat")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	stop, _, err := pwcatPlayer{bin: bin}.Start("sink", hearing.Left, 1000, -60)
	if err != nil {
		t.Skip("cannot run a shell script here:", err)
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- stop() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop hung")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestHearingModeCmd(t *testing.T) {
	lk := &fakeLink{}
	dial := func() (hearingLink, error) { lk.closed = false; return lk, nil } // a fresh connection per call
	var b bytes.Buffer
	for _, args := range [][]string{{"on"}, {"--yes"}, {"maybe", "--yes"}, {}} {
		if err := hearingModeCmd(args, &b, dial); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if lk.list() != "" {
		t.Fatal("frame sent without --yes")
	}
	if err := hearingModeCmd([]string{"off", "--yes"}, &b, dial); err != nil || lk.list() != "mode:off" {
		t.Fatal(err, lk.list())
	}
	if err := hearingModeCmd([]string{"on", "--yes"}, &b, dial); err != nil || lk.list() != "mode:off,mode:on" {
		t.Fatal(err, lk.list())
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

// answersIn reads the answers recorded in the --out file.
func answersIn(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var o struct{ Answers []json.RawMessage }
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	return len(o.Answers)
}

func TestHearingRunDeadToneDiscardsAnswerAndRepeats(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.pl.deadAtStart = map[int]bool{1: true, 5: true}
	o := opts()
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	// every start either produced one recorded answer or was discarded.
	if got, want := answersIn(t, o.outFile), r.pl.startedCount()-2; got != want {
		t.Fatalf("answers %d, tones started %d (2 died)", got, r.pl.startedCount())
	}
	if strings.Count(r.out.String(), "answer discarded") != 2 {
		t.Fatal(r.out.String())
	}
	r.assertRestored(t)
}

func TestHearingRunToneDyingMidWaitIgnoresLateKey(t *testing.T) {
	gate := make(chan struct{})
	n := 0
	keys := fnKeys{func() (byte, error) {
		<-gate
		n++
		if n%3 == 0 {
			return 'h', nil
		}
		return 'n', nil
	}}
	r := newRig(t, keys)
	r.pl.deadCh = map[int]chan struct{}{1: nil}
	o := opts()
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	go func() {
		// the first tone dies while the listener has not answered yet
		for {
			r.pl.mu.Lock()
			ch := r.pl.deadCh[1]
			r.pl.mu.Unlock()
			if ch != nil {
				close(ch)
				close(gate)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "answer discarded") {
		t.Fatal(r.out.String())
	}
	if got, want := answersIn(t, o.outFile), r.pl.startedCount()-1; got != want {
		t.Fatalf("answers %d, tones %d", got, r.pl.startedCount())
	}
	r.assertRestored(t)
}

func TestHearingRunStopErrorInvalidatesAnswer(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.pl.stopErr = map[int]error{1: errors.New("broken pipe")}
	o := opts()
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if got, want := answersIn(t, o.outFile), r.pl.startedCount()-1; got != want {
		t.Fatalf("answers %d, tones %d", got, r.pl.startedCount())
	}
}

func TestHearingRunPlayerAlwaysDeadAbortsAfterRetries(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.pl.allDead = true
	o := opts()
	o.apply, o.yes = true, true
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	err := hearingRunLive(r.env, o)
	if err == nil || !strings.Contains(err.Error(), "kept failing") {
		t.Fatal(err)
	}
	if n := r.pl.startedCount(); n != 1+maxToneRetries {
		t.Fatalf("tone started %d times", n)
	}
	if strings.Contains(r.link.list(), "write") {
		t.Fatal("wrote after a failed test")
	}
	if _, e := os.Stat(o.outFile); e == nil {
		t.Fatal("no result may be saved for an aborted test")
	}
	r.assertRestored(t)
}

func TestToneDeathIsReportedByPwcat(t *testing.T) {
	// A pw-cat that exits at once: dead closes and stop reports the failure.
	bin := filepath.Join(t.TempDir(), "fake-pw-cat")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stop, dead, err := pwcatPlayer{bin: bin}.Start("sink", hearing.Left, 1000, -60)
	if err != nil {
		t.Skip(err)
	}
	select {
	case <-dead:
	case <-time.After(3 * time.Second):
		t.Fatal("dead never closed")
	}
	done := make(chan struct{})
	go func() { _ = stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop hung")
	}
}

// A pw-cat that dies in the middle of the tone closes dead.
func TestPwcatDyingMidToneClosesDead(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-pw-cat")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 0.3\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stop, dead, err := pwcatPlayer{bin: bin}.Start("sink", hearing.Left, 1000, -60)
	if err != nil {
		t.Skip(err)
	}
	select {
	case <-dead:
	case <-time.After(3 * time.Second):
		t.Fatal("dead never closed")
	}
	_ = stop()
}

// ctlKeys is a keyboard the test drives: press returns once the runner's key
// reader has taken the key and asked for the next one, i.e. the key is in the
// queue (or already consumed).
type ctlKeys struct {
	in    chan byte
	reads int32
}

func newCtlKeys() *ctlKeys { return &ctlKeys{in: make(chan byte)} }

func (c *ctlKeys) ReadKey() (byte, error) {
	atomic.AddInt32(&c.reads, 1)
	if k, ok := <-c.in; ok {
		return k, nil
	}
	return 'n', nil
}

func (c *ctlKeys) press(k byte) {
	before := atomic.LoadInt32(&c.reads)
	c.in <- k
	for atomic.LoadInt32(&c.reads) <= before {
		time.Sleep(100 * time.Microsecond)
	}
}

func (c *ctlKeys) release() { close(c.in) }

func waitStarted(pl *fakePlayer, n int) {
	for pl.startedCount() < n {
		time.Sleep(100 * time.Microsecond)
	}
}

type answerRec struct {
	Tone  struct{ Freq int }
	Heard bool
}

func answersOf(t *testing.T, path string) []answerRec {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var o struct{ Answers []answerRec }
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	return o.Answers
}

// A key that reaches the queue while a dead tone is being torn down belongs to
// that tone, not to its repetition: the recorded answer comes from a LATER key.
func TestHearingRunKeyQueuedWhenToneDiesIsNotAppliedToRepeat(t *testing.T) {
	ck := newCtlKeys()
	r := newRig(t, ck)
	r.pl.deadCh = map[int]chan struct{}{1: nil}
	r.pl.onStop = func(n int) {
		if n == 1 {
			ck.press('n') // stale: typed for the tone that died
		}
	}
	go func() {
		waitStarted(r.pl, 1)
		for {
			r.pl.mu.Lock()
			ch := r.pl.deadCh[1]
			r.pl.mu.Unlock()
			if ch != nil {
				close(ch)
				break
			}
			time.Sleep(100 * time.Microsecond)
		}
		waitStarted(r.pl, 2)
		ck.press('h') // the real answer to the repeated tone
		ck.release()
	}()
	o := opts()
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	a := answersOf(t, o.outFile)
	if len(a) == 0 || !a[0].Heard {
		t.Fatalf("the first recorded answer must come from the later key (heard), got %+v", a[:1])
	}
}

// A key pressed after the repetition was decided but before its Start (the
// sinkAlive window) is discarded too.
func TestHearingRunKeyBeforeRepeatStartIsDiscarded(t *testing.T) {
	ck := newCtlKeys()
	r := newRig(t, ck)
	r.pl.deadAtStart = map[int]bool{1: true}
	calls := 0
	r.ex.onCall = func(line string) {
		if line == "pactl list short sinks" {
			calls++
			if calls == 3 { // 1: pick, 2: before tone 1, 3: before the repeat
				ck.press('h')
			}
		}
	}
	go func() {
		waitStarted(r.pl, 2)
		ck.press('n')
		ck.release()
	}()
	o := opts()
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	a := answersOf(t, o.outFile)
	if len(a) == 0 || a[0].Heard {
		t.Fatalf("the stale key (h) was applied: %+v", a[:1])
	}
}

// The retry ceiling is per presentation: an accepted answer resets it.
func TestHearingRunRetryCountResetsAfterAnswer(t *testing.T) {
	r := newRig(t, cycleKeys())
	// Band 0 is presented several times (one tone per staircase step). Starts
	// 1-2 die, start 3 is answered; starts 4-5 die on the NEXT presentation of
	// the same band: without the reset the count would reach 4 and abort.
	r.pl.deadAtStart = map[int]bool{1: true, 2: true, 4: true, 5: true}
	o := opts()
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
}

const activeInput = "Sink Input #1777\n\tSink: 92\n\tCorked: no\n\tMute: no\n\tVolume: front-left: 65536 / 100% / 0.00 dB\n\t\tapplication.name = \"Brave\"\n"

func sinkState(r *rig, st string) {
	o := r.ex.out["pactl list sinks"]
	for _, old := range []string{"State: IDLE", "State: RUNNING", "State: SUSPENDED"} {
		o = strings.Replace(o, old, "State: "+st, 1)
	}
	r.ex.out["pactl list sinks"] = o
}

// The sink is RUNNING for a few polls after the pause, then goes IDLE: the run
// waits, says so, and carries on.
func TestHearingRunWaitsForSinkToStopRunning(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	n := 0
	r.ex.onCall = func(line string) {
		if line == "pactl list sinks" {
			n++
			if n == 6 { // 1 = identity check, 2..5 = still running
				sinkState(r, "IDLE")
			}
		}
	}
	slept := 0
	r.env.sleep = func(time.Duration) { slept++ }
	if err := hearingRunLive(r.env, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "waiting for the audio to stop") || slept < 4 {
		t.Fatalf("slept %d\n%s", slept, r.out.String())
	}
	if r.ex.index("set-sink-volume bluez_output.AA_BB_CC_DD_EE_FF.1 100%") < 0 {
		t.Fatal("did not carry on")
	}
	r.assertRestored(t)
}

func TestHearingRunAlwaysRunningAbortsAtTimeoutWithoutTouchingVolume(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ttyOptional = true
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	var slept time.Duration
	r.env.sleep = func(d time.Duration) { slept += d }
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "still playing into") || !strings.Contains(err.Error(), "Brave") || !strings.Contains(err.Error(), "RUNNING") {
		t.Fatal(err)
	}
	if slept < 7*time.Second || slept > 9*time.Second {
		t.Fatalf("waited %s, want ~8s", slept)
	}
	if r.ex.count("set-sink") != 0 || r.link.list() != "" || r.pl.startedCount() != 0 {
		t.Fatalf("side effects: %v", r.ex.calls)
	}
	if r.ex.count(" play") != 2 {
		t.Fatal("paused players must be resumed")
	}
	r.assertRestored(t)
}

// A browser stream that lingers (not corked) on a SUSPENDED/IDLE sink carries
// no data: the run goes on without waiting.
func TestHearingRunLingeringInputOnSuspendedSinkIsFine(t *testing.T) {
	for _, st := range []string{"SUSPENDED", "IDLE"} {
		r := newRig(t, cycleKeys())
		r.ex.out["pactl list sink-inputs"] = activeInput
		sinkState(r, st)
		if err := hearingRunLive(r.env, opts()); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		if strings.Contains(r.out.String(), "waiting for the audio") {
			t.Fatalf("%s: waited for nothing", st)
		}
		r.assertRestored(t)
	}
}

// stdin is raw PCM: pw-cat needs --raw or it fails with "Format not recognised".
func TestPwcatArgsAreRawPCM(t *testing.T) {
	a := pwcatArgs("bluez_output.X.1")
	has := func(x string) int {
		for i, v := range a {
			if v == x {
				return i
			}
		}
		return -1
	}
	if has("--raw") < 0 && has("-a") < 0 {
		t.Fatalf("no --raw: %v", a)
	}
	if a[len(a)-1] != "-" || has("--target") < 0 || a[has("--target")+1] != "bluez_output.X.1" ||
		a[has("--format")+1] != "s16" || a[has("--rate")+1] != "44100" || a[has("--channels")+1] != "2" {
		t.Fatalf("%v", a)
	}
	// and --raw must come before the "-" input, as an option.
	if has("--raw") > len(a)-2 {
		t.Fatalf("%v", a)
	}
}

func pcm(vals ...int16) []byte {
	var b []byte
	for len(b) < 2*3200 { // 0.4 s at 8 kHz
		for _, v := range vals {
			b = append(b, byte(v), byte(uint16(v)>>8))
		}
	}
	return b
}

func TestHearingRunRunningSinkWithSilenceProceeds(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	reads := 0
	r.env.monitor = func(_, _ string) ([]byte, error) { reads++; return pcm(0, 3, -4, 0), nil }
	if err := hearingRunLive(r.env, opts()); err != nil {
		t.Fatal(err)
	}
	if reads != 3 { // two consecutive silent readings + the one before the volume
		t.Fatalf("monitor read %d times", reads)
	}
	r.assertRestored(t)
}

func TestHearingRunRunningSinkWithAudioAbortsAndReportsPeak(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ttyOptional = true
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	r.env.monitor = func(_, _ string) ([]byte, error) { return pcm(0, 3000, -3000), nil }
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "still playing into") || !strings.Contains(err.Error(), "peak -20.8 dBFS") {
		t.Fatal(err)
	}
	if r.ex.count("set-sink") != 0 || r.link.list() != "" {
		t.Fatal("side effects")
	}
	r.assertRestored(t)
}

func TestHearingRunMonitorMissingOrFailingKeepsConservativeAbort(t *testing.T) {
	for name, setup := range map[string]func(r *rig){
		"parec absent": func(r *rig) { r.ex.have["parec"] = false },
		"parec fails": func(r *rig) {
			r.env.monitor = func(_, _ string) ([]byte, error) { return nil, errors.New("no monitor") }
		},
		"short read": func(r *rig) { r.env.monitor = func(_, _ string) ([]byte, error) { return []byte{0, 0}, nil } },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, cycleKeys())
			r.ex.out["pactl list sink-inputs"] = activeInput
			sinkState(r, "RUNNING")
			r.env.monitor = func(_, _ string) ([]byte, error) { return pcm(0), nil }
			setup(r)
			r.ttyOptional = true
			err := hearingRunLive(r.env, opts())
			if err == nil || !strings.Contains(err.Error(), "peak not measured") {
				t.Fatal(err)
			}
			if r.ex.count("set-sink") != 0 {
				t.Fatal("volume raised")
			}
			r.assertRestored(t)
		})
	}
}

func TestHearingRunAudioReturningBeforeVolumeAborts(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ttyOptional = true
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	reads := 0
	r.env.monitor = func(_, _ string) ([]byte, error) {
		reads++
		if reads == 3 {
			return pcm(0, 5000), nil
		}
		return pcm(0), nil
	}
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "started again") || r.ex.count("set-sink") != 0 {
		t.Fatal(err)
	}
	r.assertRestored(t)
}

func heardKeys() fnKeys { return fnKeys{func() (byte, error) { return 'h', nil }} }

func TestHearingRunFloorBandsMarkedAndApplyRefused(t *testing.T) {
	r := newRig(t, heardKeys())
	o := opts()
	o.acceptFloor, o.apply, o.yes = false, true, true
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	err := hearingRunLive(r.env, o)
	out := r.out.String()
	if err == nil || !strings.Contains(err.Error(), "--accept-floor") || strings.Contains(r.link.list(), "write") {
		t.Fatalf("%v %s", err, r.link.list())
	}
	if !strings.Contains(out, "floor") || !strings.Contains(out, "bands were still heard at the lowest level") || !strings.Contains(out, "--sink-volume (e.g. 60%)") {
		t.Fatal(out)
	}
	b, _ := os.ReadFile(o.outFile)
	if !strings.Contains(string(b), `"floor"`) {
		t.Fatalf("floor missing in the JSON: %s", b)
	}
	r.assertRestored(t)
	// With the flag it goes through.
	r2 := newRig(t, heardKeys())
	o.acceptFloor = true
	if err := hearingRunLive(r2.env, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r2.link.list(), "write:left") {
		t.Fatal(r2.link.list())
	}
}

func TestHearingRunNoFloorNoWarning(t *testing.T) {
	// never heard: every band ends at +6, nowhere near the floor.
	r := newRig(t, fnKeys{func() (byte, error) { return 'n', nil }})
	o := opts()
	o.acceptFloor, o.apply, o.yes = false, true, true
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.out.String(), "lowest level") {
		t.Fatal(r.out.String())
	}
}

// The reading is per stream, taken after the STREAM's volume and mute: a
// muted or turned-down stream proves nothing. The sink's own volume/mute do
// not matter (the stream monitor is before them).
func TestHearingRunSilentMonitorNotTrustedWhenStreamMutedOrTurnedDown(t *testing.T) {
	cases := map[string]struct {
		setup func(r *rig)
		want  string
	}{
		"stream muted": {func(r *rig) {
			r.ex.out["pactl list sink-inputs"] = strings.Replace(r.ex.out["pactl list sink-inputs"], "Mute: no", "Mute: yes", 1)
		}, "is muted"},
		"stream at 20%": {func(r *rig) {
			r.ex.out["pactl list sink-inputs"] = strings.Replace(r.ex.out["pactl list sink-inputs"], "65536 / 100%", "13107 / 20%", -1)
		}, "is at 20%"},
		"stream volume unreadable": {func(r *rig) {
			r.ex.out["pactl list sink-inputs"] = strings.Replace(r.ex.out["pactl list sink-inputs"], "\tVolume: front-left: 65536 / 100% / 0.00 dB\n", "", 1)
		}, "could not be read"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, cycleKeys())
			r.ttyOptional = true
			r.ex.out["pactl list sink-inputs"] = activeInput
			sinkState(r, "RUNNING")
			c.setup(r)
			reads := 0
			r.env.monitor = func(_, _ string) ([]byte, error) { reads++; return pcm(0), nil } // silent
			err := hearingRunLive(r.env, opts())
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "the volume was not raised") {
				t.Fatal(err)
			}
			if reads != 0 {
				t.Fatalf("an untrustworthy stream was measured %d times", reads)
			}
			if r.ex.count("set-sink") != 0 {
				t.Fatalf("volume/mute changed: %v", r.ex.calls)
			}
			r.assertRestored(t)
		})
	}
}

func lowSink(r *rig) {
	r.ex.out["pactl list sinks"] = strings.Replace(r.ex.out["pactl list sinks"], "65536 / 100%", "19661 / 30%", -1)
}

// Sink at 30% (the listener's own level) and a silent open stream: measured
// per stream, so it is accepted.
func TestHearingRunLowSinkWithSilentStreamProceeds(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	lowSink(r)
	var asked []string
	r.env.monitor = func(sink, id string) ([]byte, error) { asked = append(asked, id); return pcm(0), nil }
	if err := hearingRunLive(r.env, opts()); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 3 || asked[0] != "1777" {
		t.Fatalf("monitor-stream readings: %v", asked)
	}
	r.assertRestored(t)
}

func TestHearingRunLowSinkWithRealAudioAborts(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ttyOptional = true
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	lowSink(r)
	r.env.monitor = func(_, _ string) ([]byte, error) { return pcm(0, 15047, -15047), nil }
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "#1777 Brave (peak -6.8 dBFS)") {
		t.Fatal(err)
	}
	if r.ex.count("set-sink") != 0 || r.link.list() != "" {
		t.Fatal("side effects")
	}
	r.assertRestored(t)
}

func TestHearingRunNewStreamAtReconfirmationAborts(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ttyOptional = true
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	n := 0
	r.ex.onCall = func(line string) {
		if line == "pactl list sink-inputs" {
			n++
			if n == 3 { // 1, 2: the two silent polls; 3: the reconfirmation
				r.ex.out["pactl list sink-inputs"] = activeInput +
					"Sink Input #1800\n\tSink: 92\n\tCorked: no\n\tMute: no\n\tVolume: front-left: 65536 / 100% / 0.00 dB\n\t\tapplication.name = \"Mpv\"\n"
			}
		}
	}
	r.env.monitor = func(_, _ string) ([]byte, error) { return pcm(0), nil }
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "new stream #1800 Mpv") || r.ex.count("set-sink") != 0 {
		t.Fatal(err)
	}
	r.assertRestored(t)
}

// One reading per active stream: audio on the second one is not hidden by
// silence on the first.
func TestHearingRunEveryActiveStreamMustBeSilent(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ttyOptional = true
	r.ex.out["pactl list sink-inputs"] = activeInput +
		"Sink Input #1800\n\tSink: 92\n\tCorked: no\n\tMute: no\n\tVolume: front-left: 65536 / 100% / 0.00 dB\n\t\tapplication.name = \"Mpv\"\n"
	sinkState(r, "RUNNING")
	r.env.monitor = func(_, id string) ([]byte, error) {
		if id == "1800" {
			return pcm(0, 900), nil
		}
		return pcm(0), nil
	}
	err := hearingRunLive(r.env, opts())
	if err == nil || !strings.Contains(err.Error(), "#1800 Mpv (peak") || !strings.Contains(err.Error(), "#1777 Brave (peak") || r.ex.count("set-sink") != 0 {
		t.Fatal(err)
	}
	r.assertRestored(t)
}

// The stream vanishes between the listing and the reading: the listing is
// repeated, not accepted blindly.
func TestHearingRunStreamVanishingBetweenListAndMeasureRelists(t *testing.T) {
	r := newRig(t, cycleKeys())
	r.ex.out["pactl list sink-inputs"] = activeInput
	sinkState(r, "RUNNING")
	failed := false
	r.env.monitor = func(_, id string) ([]byte, error) {
		if !failed {
			failed = true
			r.ex.out["pactl list sink-inputs"] = "" // gone
			sinkState(r, "IDLE")
			return nil, errors.New("no such stream")
		}
		return pcm(0), nil
	}
	if err := hearingRunLive(r.env, opts()); err != nil {
		t.Fatal(err)
	}
	r.assertRestored(t)
}

// Only h/Enter, n/space, r and q are keys; anything else is ignored: it is not
// a "not heard" and does not restart the tone.
func TestHearingRunUnknownKeysAreIgnored(t *testing.T) {
	ck := newCtlKeys()
	r := newRig(t, ck)
	o := opts()
	o.outFile = filepath.Join(t.TempDir(), "r.json")
	go func() {
		waitStarted(r.pl, 1)
		for _, k := range []byte{'x', 'z', 27, 'H' + 100, '1', '\t'} {
			ck.press(k)
		}
		ck.press('h') // the real answer to the first tone
		ck.release()
	}()
	if err := hearingRunLive(r.env, o); err != nil {
		t.Fatal(err)
	}
	a := answersOf(t, o.outFile)
	if len(a) == 0 || !a[0].Heard {
		t.Fatalf("an ignored key was taken as an answer: %+v", a[:1])
	}
	if strings.Contains(r.out.String(), "repeat ... ") {
		t.Fatal("an unknown key restarted the tone")
	}
}

// The test-mode off frame must go out while the connection is still open, on
// every way out of the run (the link is closed LAST).
func TestHearingRunModeOffIsSentBeforeTheLinkIsClosed(t *testing.T) {
	check := func(t *testing.T, r *rig, wantErr bool, err error) {
		t.Helper()
		if (err != nil) != wantErr {
			t.Fatalf("err = %v", err)
		}
		ev := r.link.list()
		if !strings.Contains(ev, "mode:on") || !strings.HasSuffix(ev, "mode:off") {
			t.Fatalf("events %s", ev)
		}
		if r.link.usedAfterClose || strings.Contains(ev, "FAILED") {
			t.Fatalf("a call hit the closed connection: %s", ev)
		}
		if !r.link.closed {
			t.Fatal("the link was never closed")
		}
	}
	t.Run("normal end", func(t *testing.T) {
		r := newRig(t, cycleKeys())
		err := hearingRunLive(r.env, opts())
		check(t, r, false, err)
	})
	t.Run("abort key", func(t *testing.T) {
		r := newRig(t, nil)
		r.env.keys = func() (keySource, func(), error) { return pacedKeys(r.pl, "nq"), func() { *r.ttyRestored++ }, nil }
		err := hearingRunLive(r.env, opts())
		check(t, r, true, err)
	})
	t.Run("error", func(t *testing.T) {
		r := newRig(t, cycleKeys())
		r.link.failWriteOn = 1
		o := opts()
		o.apply, o.yes = true, true
		err := hearingRunLive(r.env, o)
		check(t, r, true, err)
	})
	t.Run("sigint", func(t *testing.T) {
		block := make(chan struct{})
		t.Cleanup(func() { close(block) })
		r := newRig(t, fnKeys{func() (byte, error) { <-block; return 0, io.EOF }})
		sigs := make(chan os.Signal, 1)
		r.env.sigs = sigs
		go func() {
			waitStarted(r.pl, 1)
			sigs <- syscall.SIGINT
		}()
		err := hearingRunLive(r.env, opts())
		check(t, r, true, err)
	})
}
