package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// `qcyctl find`: "find my earbuds" as the vendor app does it
// (LocalMapActivity): no BLE command. The phone plays a sound in a loop on the
// earbuds' A2DP sink and raises the system volume one step every 2 s; on stop
// the saved volume is restored. The app has no ceiling, no warning and no
// timeout; this command adds them:
//
//   - the sink volume starts at 20% and rises 5 points every 2 s, never past
//     the ceiling (--max-volume, default 70%);
//   - it stops by itself after --timeout (default 60 s);
//   - the tone is a short "di-di" beep at -20 dBFS peak or less;
//   - the volume is never raised before the sink was measured silent, and the
//     safety machinery (sink identity, music pause, restore on any exit) is
//     the one `hearing run` uses (audio_guard.go).

const (
	findStartPct      = 20
	findStepPct       = 5
	findStepEvery     = 2 * time.Second
	findPoll          = 100 * time.Millisecond
	findSinkEvery     = time.Second
	findDefaultMax    = 70
	findDefaultTime   = 60
	findWarning       = "Remove the earbuds from your ears before starting."
	findBeepFreq      = 2000
	findBeepAmp       = 0.09 // peak 0.09 of full scale = -20.9 dBFS
	findBeepMs        = 150
	findGapMs         = 100
	findRestMs        = 700 // after the second beep
	findBeepRampMs    = 10
	findBeepRate      = 44100
	findBeepChunkMs   = 20
	findBeepsPerBurst = 2
)

// beepPlayer plays the find tone until stop is called (same contract as
// tonePlayer: stop is bounded, dead closes when nothing can be heard).
type beepPlayer interface {
	Start(sink string) (stop func() error, dead <-chan struct{}, err error)
}

// findEnv is hearingEnv (exec, have, sigs, cachedMAC, sleep, monitor, keys,
// out = progress text) plus the beep player and the --events stream.
type findEnv struct {
	hearingEnv
	beep   beepPlayer
	events io.Writer // JSON lines, nil unless --events
}

type findOpts struct {
	sinkName string
	maxPct   int
	timeout  time.Duration
	yes      bool
}

// ---------- the tone ----------

// beepGen generates the repeating "di-di ... " pattern as s16le stereo.
type beepGen struct {
	pos   int     // frame index inside the pattern
	phase float64 // continuous sine phase
}

func msFrames(ms int) int { return findBeepRate * ms / 1000 }

func beepPatternFrames() int {
	return findBeepsPerBurst*msFrames(findBeepMs) + (findBeepsPerBurst-1)*msFrames(findGapMs) + msFrames(findRestMs)
}

// beepEnv is the gain (0..1) at frame i of the pattern: raised-cosine ramps
// of findBeepRampMs on each beep, 0 in the gaps.
func beepEnv(i int) float64 {
	b, g, r := msFrames(findBeepMs), msFrames(findGapMs), msFrames(findBeepRampMs)
	for k := 0; k < findBeepsPerBurst; k++ {
		start := k * (b + g)
		if i >= start && i < start+b {
			j := i - start
			switch {
			case j < r:
				return 0.5 - 0.5*math.Cos(math.Pi*float64(j)/float64(r))
			case j >= b-r:
				return 0.5 - 0.5*math.Cos(math.Pi*float64(b-1-j)/float64(r))
			}
			return 1
		}
	}
	return 0
}

func (g *beepGen) frame(gain float64) (lo, hi byte) {
	v := int16(math.Round(math.Sin(g.phase) * gain * findBeepAmp * 32767))
	g.phase += 2 * math.Pi * findBeepFreq / findBeepRate
	if g.phase > 2*math.Pi {
		g.phase -= 2 * math.Pi
	}
	return byte(uint16(v)), byte(uint16(v) >> 8)
}

// Next returns n frames of the pattern.
func (g *beepGen) Next(n int) []byte {
	out := make([]byte, 0, n*4)
	total := beepPatternFrames()
	for i := 0; i < n; i++ {
		lo, hi := g.frame(beepEnv(g.pos))
		out = append(out, lo, hi, lo, hi)
		g.pos = (g.pos + 1) % total
	}
	return out
}

// Fade ends the stream: a findBeepRampMs ramp from the current gain to 0 so
// the stop does not click.
func (g *beepGen) Fade() []byte {
	n := msFrames(findBeepRampMs)
	last := beepEnv((g.pos + beepPatternFrames() - 1) % beepPatternFrames())
	out := make([]byte, 0, n*4)
	for i := 0; i < n; i++ {
		lo, hi := g.frame(last * (1 - float64(i+1)/float64(n)))
		out = append(out, lo, hi, lo, hi)
	}
	return out
}

type pwcatBeeper struct{ bin string }

func (p pwcatBeeper) Start(sink string) (func() error, <-chan struct{}, error) {
	var g beepGen
	chunk := msFrames(findBeepChunkMs)
	return startPwcat(p.bin, sink, func(write func([]byte) error, quit <-chan struct{}) error {
		for {
			select {
			case <-quit:
				return write(g.Fade())
			default:
			}
			if e := write(g.Next(chunk)); e != nil {
				return e
			}
		}
	})
}

// guardedBeeper refuses to play while hardware is disabled (tests).
type guardedBeeper struct{ beepPlayer }

func (g guardedBeeper) Start(sink string) (func() error, <-chan struct{}, error) {
	if err := refuseHardware(); err != nil {
		return nil, nil, err
	}
	return g.beepPlayer.Start(sink)
}

// ---------- stdin commands (--events) ----------

// lineKeys turns stdin lines into keys for --events: "stop" is 'q', EOF is
// io.EOF (the controller went away: stop).
type lineKeys struct{ sc *bufio.Scanner }

func newLineKeys(r io.Reader) *lineKeys { return &lineKeys{sc: bufio.NewScanner(r)} }

func (l *lineKeys) ReadKey() (byte, error) {
	for l.sc.Scan() {
		if strings.EqualFold(strings.TrimSpace(l.sc.Text()), "stop") {
			return 'q', nil
		}
	}
	if err := l.sc.Err(); err != nil {
		return 0, err
	}
	return 0, io.EOF
}

// ---------- the run ----------

// errStopRequested: "stop" (or EOF) arrived on stdin in --events mode. Raised
// through the aborter so that every phase, including the music pause and the
// silence wait, observes it and nothing is raised after it.
var errStopRequested = errors.New("stop requested")

// findBaselineAt: when the set of streams on the sink (our tone included) is
// recorded as the baseline for the new-stream check.
const findBaselineAt = 500 * time.Millisecond

// findRunLive runs `find`. Every return path restores the sink volume and
// mute and resumes the paused players; the "stopped" event is written only
// after that.
func findRunLive(env findEnv, o findOpts) error {
	if env.sleep == nil {
		env.sleep = time.Sleep
	}
	emit := func(m map[string]interface{}) {
		if env.events == nil {
			return
		}
		b, _ := json.Marshal(m)
		fmt.Fprintf(env.events, "%s\n", b)
	}
	if env.now == nil {
		env.now = time.Now
	}
	reason, err := findCore(env, o, emit)
	if errors.Is(err, errStopRequested) {
		reason, err = "stop", nil
	}
	if err != nil {
		if errors.Is(err, errInterrupted) {
			reason = "signal"
		}
		if reason == "" {
			reason = "error"
		}
		emit(map[string]interface{}{"event": "error", "message": err.Error()})
	}
	emit(map[string]interface{}{"event": "stopped", "reason": reason})
	if err == nil {
		fmt.Fprintf(env.out, "stopped (%s); volume and music restored\n", reason)
	}
	return err
}

func findCore(env findEnv, o findOpts, emit func(map[string]interface{})) (reason string, err error) {
	out := env.out
	warnf := func(f string, a ...interface{}) { fmt.Fprintf(out, "warning: "+f+"\n", a...) }
	ab := newAborter(env.sigs)
	defer ab.close()
	checkAbort, abortCh := ab.check, ab.ch

	if o.maxPct < 1 || o.maxPct > 100 {
		return "", fmt.Errorf("--max-volume must be 1%%..100%%")
	}
	if o.timeout <= 0 {
		return "", errors.New("--timeout must be positive")
	}
	if !env.have("pactl") || !env.have("pw-cat") {
		return "", errors.New("find needs pactl and pw-cat (PipeWire) in PATH")
	}
	mp, err := mprisFor(env.hearingEnv)
	if err != nil {
		return "", err
	}
	tgt, err := resolveBluezSink(env.hearingEnv, o.sinkName)
	if err != nil {
		return "", err
	}
	sink := tgt.Name
	start := findStartPct
	if start > o.maxPct {
		start = o.maxPct
	}
	fmt.Fprintf(out, "sink: %s  (%s, address %s)\n", sink, tgt.Desc, tgt.Addr)
	fmt.Fprintf(out, "plan: volume %d%% -> at most %d%%, +%d points every %s, stops after %s\n",
		start, o.maxPct, findStepPct, findStepEvery, o.timeout)
	fmt.Fprintln(out, "warning: "+findWarning)
	if e := checkAbort(); e != nil {
		return "", e
	}
	keys, restoreTTY, err := env.keys()
	if err != nil {
		return "", err
	}
	defer restoreTTY()

	keyCh := make(chan byte, 16)
	keyErr := make(chan error, 1)
	eventsMode := env.events != nil
	go func() {
		defer func() {
			if r := recover(); r != nil {
				keyErr <- fmt.Errorf("key reader panicked: %v", r)
			}
		}()
		for {
			k, e := keys.ReadKey()
			if e != nil {
				if eventsMode && errors.Is(e, io.EOF) {
					ab.abort(errStopRequested) // the controller went away
					return
				}
				keyErr <- e
				return
			}
			if eventsMode {
				if k == 'q' { // "stop" on stdin: seen by every phase
					ab.abort(errStopRequested)
					return
				}
				continue
			}
			keyCh <- k
		}
	}()
	if !o.yes {
		fmt.Fprint(out, "Continue? type y to start, anything else cancels: ")
		select {
		case k := <-keyCh:
			fmt.Fprintln(out)
			if k != 'y' && k != 'Y' {
				return "", errors.New("cancelled: nothing was changed")
			}
		case e := <-keyErr:
			fmt.Fprintln(out)
			return "", fmt.Errorf("no confirmation (%v): nothing was changed", e)
		case <-abortCh:
			fmt.Fprintln(out)
			return "", checkAbort()
		}
	}

	// Side effects, each undo registered before it is attempted.
	var undo []func()
	defer func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()
	if err := pauseMusic(env.hearingEnv, mp, func(f func()) { undo = append(undo, f) }, warnf); err != nil {
		return "", err
	}
	if e := checkAbort(); e != nil {
		return "", e
	}
	if err := waitSinkSilent(env.hearingEnv, tgt, checkAbort); err != nil {
		return "", err
	}

	// Only now, with the sink measured silent, may the volume change.
	volUndo, markVol := tgt.restoreVolume(env.hearingEnv, warnf)
	undo = append(undo, volUndo)
	cur := 0
	setVol := func(v int) error {
		if v > o.maxPct { // the ceiling is never crossed, whatever the caller asks
			v = o.maxPct
		}
		if v < 1 {
			v = 1
		}
		markVol()
		if _, e := env.exec("pactl", "set-sink-volume", sink, fmt.Sprintf("%d%%", v)); e != nil {
			return fmt.Errorf("pactl set-sink-volume: %w", e)
		}
		cur = v
		return nil
	}
	if e := checkAbort(); e != nil { // a stop received while waiting for silence
		return "", e
	}
	if err := setVol(start); err != nil {
		return "", err
	}
	if _, e := env.exec("pactl", "set-sink-mute", sink, "0"); e != nil {
		return "", fmt.Errorf("pactl set-sink-mute: %w", e)
	}

	stop, dead, e := env.beep.Start(sink)
	if e != nil {
		return "", fmt.Errorf("play tone: %w", e)
	}
	var stopTone func() error = stop
	endTone := func() {
		if stopTone == nil {
			return
		}
		f := stopTone
		stopTone = nil
		if e := f(); e != nil {
			warnf("tone: %v", e)
		}
	}
	// The tone is the first thing the cleanup silences, before the volume is
	// put back and the music resumed (undo runs last-registered first).
	undo = append(undo, endTone)

	for { // TTY only: a key typed before the tone started is not a stop
		select {
		case <-keyCh:
			continue
		default:
		}
		break
	}
	fmt.Fprintf(out, "playing at %d%% (q / Enter / space stops)\n", cur)
	emit(map[string]interface{}{"event": "started", "sink": sink, "volume": cur, "maxVolume": o.maxPct,
		"timeoutSeconds": int(o.timeout / time.Second)})

	// Time is read from the clock, not summed from sleeps: a slow pactl call
	// must not stretch the timeout or the ramp.
	t0 := env.now()
	var elapsed time.Duration
	nextStep, nextSink := findStepEvery, findSinkEvery
	var baseline map[string]bool
	streamsNow := func() ([]streamInfo, error) {
		in, e := env.exec("pactl", "list", "sink-inputs")
		if e != nil {
			return nil, fmt.Errorf("pactl list sink-inputs: %w", e)
		}
		return streamsOn(in, tgt.Idx), nil
	}
	for {
		elapsed = env.now().Sub(t0)
		select { // a dead tone wins over everything else
		case <-dead:
			return "player-died", errors.New("the tone player died: stopped, volume restored")
		default:
		}
		select {
		case <-dead:
			return "player-died", errors.New("the tone player died: stopped, volume restored")
		case <-abortCh:
			return "signal", checkAbort()
		case k := <-keyCh:
			switch k {
			case 'q', 'Q', '\r', '\n', ' ':
				return "stop", nil
			}
		case e := <-keyErr:
			if errors.Is(e, io.EOF) {
				return "stop", nil // the controller closed stdin
			}
			return "error", e
		default:
		}
		if elapsed >= o.timeout {
			return "timeout", nil
		}
		if baseline == nil && elapsed >= findBaselineAt {
			st, e := streamsNow()
			if e != nil {
				return "error", e
			}
			baseline = map[string]bool{}
			for _, x := range st {
				baseline[x.ID] = true
			}
		}
		if elapsed >= nextStep {
			nextStep += findStepEvery
			if baseline != nil {
				// Cheap re-check before every rise: a stream that was not
				// there when the tone settled (music resumed, another app)
				// ends the run before it can be made louder.
				st, e := streamsNow()
				if e != nil {
					return "error", e
				}
				for _, x := range st {
					if !baseline[x.ID] {
						return "audio-started", fmt.Errorf("a new audio stream %s started on %s: stopped, volume restored", x, sink)
					}
				}
			}
			if cur < o.maxPct {
				next := cur + findStepPct
				if next > o.maxPct {
					next = o.maxPct
				}
				if e := setVol(next); e != nil {
					return "error", e
				}
				fmt.Fprintf(out, "volume %d%%\n", cur)
				emit(map[string]interface{}{"event": "volume", "volume": cur, "elapsedSeconds": int(elapsed / time.Second)})
			}
		}
		if elapsed >= nextSink {
			nextSink += findSinkEvery
			l, e := env.exec("pactl", "list", "short", "sinks")
			if e != nil {
				return "error", fmt.Errorf("pactl list short sinks: %w", e)
			}
			alive := false
			for _, s := range parseSinks(l) {
				if s.Name == sink {
					alive = true
				}
			}
			if !alive {
				return "disconnected", fmt.Errorf("sink %s disappeared (the earbuds disconnected?): stopped", sink)
			}
		}
		env.sleep(findPoll)
	}
}

// liveFindEnv is the production wiring. In events mode the progress text goes
// to stderr and stdout carries only JSON lines; commands come as lines on
// stdin (no terminal needed).
func liveFindEnv(out io.Writer, events bool) (findEnv, func()) {
	h, done := liveHearingEnv(out)
	fe := findEnv{hearingEnv: h, beep: guardedBeeper{pwcatBeeper{bin: "pw-cat"}}}
	if events {
		fe.hearingEnv.out = os.Stderr
		fe.events = out
		fe.hearingEnv.keys = func() (keySource, func(), error) {
			if err := refuseHardware(); err != nil {
				return nil, nil, err
			}
			return newLineKeys(os.Stdin), func() {}, nil
		}
	}
	return fe, done
}
