package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Shared safety machinery of every command that makes the earbuds' sink loud
// (`hearing run`, `find`): signals as an abort, the sink identity, pausing the
// music, waiting for the sink to be silent (measured per stream) and putting
// the volume and mute back.

// aborter turns the first signal (or an explicit abort) into an error every
// phase of a run observes.
type aborter struct {
	once sync.Once
	mu   sync.Mutex
	err  error
	ch   chan struct{}
	done chan struct{}
}

func newAborter(sigs <-chan os.Signal) *aborter {
	a := &aborter{ch: make(chan struct{}), done: make(chan struct{})}
	go func() {
		select {
		case <-sigs:
			a.abort(errInterrupted)
		case <-a.done:
		}
	}()
	return a
}

func (a *aborter) abort(e error) {
	a.once.Do(func() {
		a.mu.Lock()
		a.err = e
		a.mu.Unlock()
		close(a.ch)
	})
}

// check returns the abort error, or nil while none was raised.
func (a *aborter) check() error {
	select {
	case <-a.ch:
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.err
	default:
		return nil
	}
}

func (a *aborter) close() { close(a.done) }

// sinkTarget is the earbuds' Bluetooth sink with the volume and mute it had.
type sinkTarget struct {
	Name, Idx, Desc, Addr string
	Vols                  []string // raw per-channel values (or percentages)
	Muted                 bool
}

// resolveBluezSink picks the earbuds' sink (explicit name or the cached MAC),
// refuses anything that is not a Bluetooth sink identified by
// api.bluez5.address, and reads its volume and mute.
func resolveBluezSink(env hearingEnv, explicit string) (sinkTarget, error) {
	var t sinkTarget
	listing, err := env.exec("pactl", "list", "short", "sinks")
	if err != nil {
		return t, fmt.Errorf("pactl list short sinks: %w", err)
	}
	sink, err := pickSink(parseBluezSinks(listing), explicit, env.cachedMAC())
	if err != nil {
		return t, err
	}
	for _, s := range parseSinks(listing) {
		if s.Name == sink {
			t.Idx = s.Index
		}
	}
	long, err := env.exec("pactl", "list", "sinks")
	if err != nil {
		return t, fmt.Errorf("pactl list sinks: %w", err)
	}
	props := parseSinkProps(long, sink)
	addr := props["api.bluez5.address"]
	if addr == "" {
		return t, fmt.Errorf("sink %s is not identified as a Bluetooth device (no api.bluez5.address in pactl list sinks)", sink)
	}
	if explicit == "" && normMAC(addr) != normMAC(env.cachedMAC()) {
		return t, fmt.Errorf("sink %s reports address %s, not the earbuds' %s", sink, addr, env.cachedMAC())
	}
	t.Name, t.Desc, t.Addr = sink, props["Description"], addr
	volOut, err := env.exec("pactl", "get-sink-volume", sink)
	if err != nil {
		return t, fmt.Errorf("pactl get-sink-volume: %w", err)
	}
	if t.Vols, err = parseSinkVolume(volOut); err != nil {
		return t, err
	}
	muteOut, err := env.exec("pactl", "get-sink-mute", sink)
	if err != nil {
		return t, fmt.Errorf("pactl get-sink-mute: %w", err)
	}
	if t.Muted, err = parseMute(muteOut); err != nil {
		return t, err
	}
	return t, nil
}

// restoreVolume returns the undo that puts the sink's volume and mute back
// (a no-op until mark is called) and mark, to be called BEFORE the first
// change is attempted: a change that fails half way is undone too.
func (t sinkTarget) restoreVolume(env hearingEnv, warnf func(string, ...interface{})) (undo func(), mark func()) {
	changed := false
	undo = func() {
		if !changed {
			return
		}
		if _, e := env.exec("pactl", append([]string{"set-sink-volume", t.Name}, t.Vols...)...); e != nil {
			warnf("could not restore the sink volume (was %v): %v", t.Vols, e)
		}
		m := "0"
		if t.Muted {
			m = "1"
		}
		if _, e := env.exec("pactl", "set-sink-mute", t.Name, m); e != nil {
			warnf("could not restore the sink mute: %v", e)
		}
	}
	return undo, func() { changed = true }
}

// pauseMusic pauses what is Playing (MPRIS) and confirms it. The resume of
// exactly those players is handed to register before anything is paused.
func pauseMusic(env hearingEnv, mp *mprisCtl, register func(func()), warnf func(string, ...interface{})) error {
	out := env.out
	var paused []string
	register(func() {
		for _, p := range paused {
			if e := mp.play(p); e != nil {
				warnf("could not resume %s: %v", p, e)
			}
		}
	})
	fmt.Fprintln(out, "pausing music…")
	players, err := mp.list()
	if err != nil {
		return fmt.Errorf("listing media players: %w", err)
	}
	for _, p := range players {
		s, e := mp.status(p)
		if e != nil {
			return fmt.Errorf("cannot read the state of player %s: %w", p, e)
		}
		if s != "Playing" {
			continue
		}
		paused = append(paused, p) // resumed even if the pause fails half way
		if e := mp.pause(p); e != nil {
			return fmt.Errorf("could not pause %s: %w", p, e)
		}
	}
	for _, p := range paused {
		ok := false
		for try := 0; try < 5 && !ok; try++ {
			if s, e := mp.status(p); e == nil && s != "Playing" {
				ok = true
			} else {
				env.sleep(100 * time.Millisecond)
			}
		}
		if !ok {
			return fmt.Errorf("player %s is still Playing after the pause: aborted, the volume was not raised", p)
		}
	}
	if len(paused) > 0 {
		fmt.Fprintf(out, "paused: %s (resumed at the end)\n", strings.Join(paused, ", "))
	}
	return nil
}

// waitSinkSilent waits (bounded) until nothing audible plays into the sink:
// the sink is IDLE/SUSPENDED, or every stream on it was measured silent twice
// by its own monitor and once more right before returning. Anything else
// (still RUNNING at the timeout, a stream that cannot be judged, a new
// stream) is an error and the caller must not raise the volume.
func waitSinkSilent(env hearingEnv, t sinkTarget, checkAbort func() error) error {
	out := env.out
	sink, sinkIdx := t.Name, t.Idx
	var lastState, distrust string
	var lastActive []streamInfo
	peaks := map[string]string{} // stream id -> "peak -X dBFS" / "peak not measured"
	silentRuns, usedMonitor := 0, false
	haveMonitor := env.monitor != nil && env.have("parec")
	// readSink reads the sink state and the streams on it. why is non-empty
	// when a stream cannot be judged from its own monitor: the per-stream
	// reading is taken AFTER that stream's volume and mute (before the
	// sink's), so a muted or turned-down stream proves nothing.
	readSink := func() (state string, active []streamInfo, why string, err error) {
		so, err := env.exec("pactl", "list", "sinks")
		if err != nil {
			return "", nil, "", fmt.Errorf("pactl list sinks: %w", err)
		}
		in, err := env.exec("pactl", "list", "sink-inputs")
		if err != nil {
			return "", nil, "", fmt.Errorf("pactl list sink-inputs: %w", err)
		}
		state = strings.ToUpper(strings.TrimSpace(parseSinkProps(so, sink)["State"]))
		active = streamsOn(in, sinkIdx)
		for _, st := range active {
			switch {
			case why != "":
			case st.Mute:
				why = fmt.Sprintf("stream %s is muted: unmute it or stop the app, then retry", st)
			case st.MinPct < 0:
				why = fmt.Sprintf("the volume of stream %s could not be read", st)
			case st.MinPct < 90:
				why = fmt.Sprintf("stream %s is at %d%%: raise it to 100%% or stop the app, then retry", st, st.MinPct)
			}
		}
		return state, active, why, nil
	}
	// measureStream reads one stream's own monitor (pre sink volume).
	measureStream := func(id string) (int, float64, bool) {
		if !haveMonitor {
			return 0, 0, false
		}
		b, err := env.monitor(sink, id)
		if err != nil || len(b) < 2*800 { // < 0.1 s of data is not a reading
			return 0, 0, false
		}
		p, db := peakS16(b)
		return p, db, true
	}
	// allSilent measures every active stream: silent only when every one was
	// read and is below the silence threshold. It records what it saw.
	allSilent := func(active []streamInfo) bool {
		silent := len(active) > 0
		for _, st := range active {
			p, db, ok := measureStream(st.ID)
			switch {
			case !ok:
				peaks[st.ID] = "peak not measured"
				silent = false
			case p >= silencePeak:
				peaks[st.ID] = fmt.Sprintf("peak %.1f dBFS", db)
				silent = false
			default:
				peaks[st.ID] = fmt.Sprintf("peak %.1f dBFS", db)
			}
		}
		return silent
	}
	var measured []string // ids of the streams of the accepted readings
	for poll := 0; ; poll++ {
		if e := checkAbort(); e != nil {
			return e
		}
		state, active, why, err := readSink()
		if err != nil {
			return err
		}
		lastState, distrust, lastActive = state, why, active
		quiet := len(active) == 0
		if state != "" { // the state is authoritative when pactl reports it
			quiet = state == "IDLE" || state == "SUSPENDED"
		}
		if !quiet && state == "RUNNING" && why == "" && len(active) > 0 {
			// RUNNING does not prove sound: a browser may keep a stream open
			// sending silence. Measure each stream (a vanished stream fails
			// its reading: the next poll lists again).
			if allSilent(active) {
				silentRuns++
			} else {
				silentRuns = 0
			}
			if silentRuns >= 2 {
				quiet, usedMonitor = true, true
				measured = measured[:0]
				for _, st := range active {
					measured = append(measured, st.ID)
				}
			}
		} else {
			silentRuns = 0
		}
		if quiet {
			break
		}
		if poll >= audioStopPolls {
			who := "no stream listed"
			if len(lastActive) > 0 {
				var parts []string
				for _, st := range lastActive {
					m := peaks[st.ID]
					if m == "" {
						m = "peak not measured"
					}
					parts = append(parts, fmt.Sprintf("%s (%s)", st, m))
				}
				who = strings.Join(parts, ", ")
			}
			reason := ""
			if distrust != "" {
				reason = ": " + distrust
			}
			return fmt.Errorf("audio is still playing into %s (state %s; %s) after %s%s; the volume was not raised",
				sink, lastState, who, time.Duration(audioStopPolls)*audioStopPoll, reason)
		}
		if poll == 0 {
			fmt.Fprintln(out, "waiting for the audio to stop…")
		}
		env.sleep(audioStopPoll)
	}
	if usedMonitor {
		// Shrink the window: list again and measure again right before
		// raising the volume. A stream that was not in the accepted readings
		// (or a reading that fails) is not accepted.
		_, active, why, err := readSink()
		if err != nil {
			return err
		}
		if why != "" {
			return fmt.Errorf("audio may be playing on %s: %s; the volume was not raised", sink, why)
		}
		known := map[string]bool{}
		for _, id := range measured {
			known[id] = true
		}
		for _, st := range active {
			if !known[st.ID] {
				return fmt.Errorf("a new stream %s appeared on %s: the volume was not raised", st, sink)
			}
		}
		if !allSilent(active) {
			var parts []string
			for _, st := range active {
				parts = append(parts, fmt.Sprintf("%s (%s)", st, peaks[st.ID]))
			}
			return fmt.Errorf("audio started again on %s: %s; the volume was not raised", sink, strings.Join(parts, ", "))
		}
	}
	return checkAbort()
}
