package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/hearing"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// `qcyctl hearing run`: the real test. Everything that touches the outside
// world sits behind hearingEnv so the tests drive it with fakes.
//
// Guarantees, whatever happens after the first side effect (error, abort key,
// SIGINT/SIGTERM/SIGHUP, a panic in a helper goroutine): the tone is stopped,
// the test mode (when it was switched on) is switched off, the sink volume and
// mute are put back and the players this run paused are resumed.
//
// Nothing is written to the earbuds without --apply --yes. The device test
// mode (frame 0x45) is NOT verified on hardware; it is only sent with
// --device-mode or --apply --yes.

// ---------- seams ----------

// hearingLink is the daemon side: only the daemon speaks the hearing commands.
type hearingLink interface {
	status() (map[string]interface{}, error)
	// capabilities is answered from the daemon's cache (no BLE sweep): it
	// reports whether the connected model is a verified one.
	capabilities() (verified bool, err error)
	// hearingMode sends 0x45 (with confirm:true).
	hearingMode(on bool) (*hearingCall, error)
	// baseline "" or "factory": the 8 test bands; "zero": EXPERIMENT, the 5
	// factory bands at gain 0 first (13 bands per ear).
	hearingWrite(ear hearing.Ear, gains, other [hearing.Bands]int, baseline string) (*hearingCall, error)
	eqRead() (*qcycore.EqSnapshot, error)
	// ping is a harmless read that keeps the daemon's idle window from
	// dropping the BLE link during a long test.
	ping()
	Close() error
}

type hearingCall struct {
	Sent          string   `json:"sent"`
	Ack           bool     `json:"ack"`
	AckPayload    string   `json:"ackPayload,omitempty"`
	MasterGain    *float64 `json:"masterGain,omitempty"`
	MasterClamped bool     `json:"masterClamped,omitempty"`
	Readback      *struct {
		Slot       int     `json:"slot"`
		Effect     string  `json:"effect,omitempty"`
		MasterGain float64 `json:"masterGain"`
		Payload    string  `json:"payload"`
	} `json:"readback,omitempty"`
	ReadbackError string `json:"readbackError,omitempty"`
}

// tonePlayer plays one continuous tone until the returned stop is called.
// stop must return in bounded time and returns an error when the tone did not
// play cleanly (the player died or a write failed). dead is closed as soon as
// the tone can no longer be heard (player exited, writer failed): an answer
// given then is not a measurement.
type tonePlayer interface {
	Start(sink string, ear hearing.Ear, freq int, levelDB float64) (stop func() error, dead <-chan struct{}, err error)
}

// keySource blocks for one key; io.EOF ends the test.
type keySource interface{ ReadKey() (byte, error) }

type hearingEnv struct {
	out       io.Writer
	exec      func(name string, args ...string) (string, error)
	have      func(name string) bool
	link      func() (hearingLink, error)
	player    tonePlayer
	keys      func() (keySource, func(), error)
	sigs      <-chan os.Signal
	cachedMAC func() string
	now       func() time.Time
	sleep     func(time.Duration)
	configDir func() (string, error)
	// saveCurve stores the active curve in the library under name and returns
	// its id (the equivalent of `library import-device <name>`).
	saveCurve func(name string, snap *qcycore.EqSnapshot) (string, error)
	// slot11 preserves the slot 11 curve as a user entry of the library
	// (errNoSlot11Copy when there is nothing to preserve).
	slot11 func(snap *qcycore.EqSnapshot) (slot11Backup, error)
	// recordCurve stores (replacing atomically, the old version to the trash)
	// the hearing-test card after a confirmed write.
	recordCurve func(c qcycore.Curve) (replaced bool, err error)
	pingEvery   time.Duration
	// monitor reads ~0.4 s of ONE stream's monitor (parec --monitor-stream) as
	// s16le mono 8 kHz, read only. Nil/failing = not measurable (the
	// conservative path).
	monitor func(sink, streamID string) ([]byte, error)
}

type hearingRunOpts struct {
	soundDB    [hearing.Bands]float64
	sinkVolume int
	sinkName   string
	apply, yes bool
	// deviceMode sends the (unverified) 0x45 test-mode frame even without
	// --apply --yes.
	deviceMode bool
	// acceptFloor lets --apply --yes write bands that were still heard at the
	// lowest level of the staircase.
	acceptFloor bool
	// noSlot11Backup waives the slot 11 backup when there is no copy at all.
	noSlot11Backup bool
	outFile        string
	// baseline "" / "factory" or "zero" (EXPERIMENT): used by --apply --yes and
	// stored in the result file for `hearing apply`.
	baseline string
	// events, when set, is the --events JSON-lines stream (the progress text
	// goes to env.out, which is stderr then).
	events io.Writer
}

// ---------- parsing ----------

type sinkInfo struct {
	Index string
	Name  string
}

// parseSinks parses `pactl list short sinks`.
func parseSinks(listing string) []sinkInfo {
	var out []sinkInfo
	for _, ln := range strings.Split(listing, "\n") {
		if f := strings.Fields(ln); len(f) >= 2 {
			out = append(out, sinkInfo{Index: f[0], Name: f[1]})
		}
	}
	return out
}

// parseBluezSinks returns the Bluetooth sink names from `pactl list short sinks`.
func parseBluezSinks(listing string) []string {
	var out []string
	for _, s := range parseSinks(listing) {
		if strings.HasPrefix(s.Name, "bluez_output.") || strings.HasPrefix(s.Name, "bluez_sink.") {
			out = append(out, s.Name)
		}
	}
	return out
}

func normMAC(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), ":", "_"), "-", "_"))
}

var sinkMACRe = regexp.MustCompile(`(?i)([0-9a-f]{2}(?:_[0-9a-f]{2}){5})`)

// macNear reports whether the sink name carries a MAC (normalised, "AA_BB_...")
// that matches m in the first five octets and differs by at most one in the last.
func macNear(m, sink string) bool {
	a := strings.Split(m, "_")
	f := sinkMACRe.FindString(sink)
	if len(a) != 6 || f == "" {
		return false
	}
	b := strings.Split(strings.ToUpper(f), "_")
	for i := 0; i < 5; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	x, err1 := strconv.ParseUint(a[5], 16, 8)
	y, err2 := strconv.ParseUint(b[5], 16, 8)
	if err1 != nil || err2 != nil {
		return false
	}
	d := int(x) - int(y)
	return d >= -1 && d <= 1
}

// pickSink chooses the earbuds' sink: an explicit name, or the one whose name
// carries the cached MAC. There is no "only sink" fallback: guessing could play
// the test on somebody else's headphones.
func pickSink(sinks []string, explicit, mac string) (string, error) {
	if explicit != "" {
		for _, s := range sinks {
			if s == explicit {
				return s, nil
			}
		}
		return "", fmt.Errorf("sink %q is not a Bluetooth sink (found: %s)", explicit, strings.Join(sinks, ", "))
	}
	if len(sinks) == 0 {
		return "", errors.New("no Bluetooth sink found (pactl list short sinks): connect the earbuds' audio first")
	}
	if m := normMAC(mac); m != "" {
		for _, s := range sinks {
			if strings.Contains(strings.ToUpper(s), m) {
				return s, nil
			}
		}
		// The BLE address often differs from the audio (classic) one by one in
		// the last octet; accept it only when exactly one sink is that close.
		var near []string
		for _, s := range sinks {
			if macNear(m, s) {
				near = append(near, s)
			}
		}
		if len(near) == 1 {
			return near[0], nil
		}
	}
	return "", fmt.Errorf("no Bluetooth sink matches the earbuds' cached MAC %q (sinks: %s): choose one with --sink (the cached address is the BLE one and may differ from the audio address)", mac, strings.Join(sinks, ", "))
}

var propRe = regexp.MustCompile(`^\s*([A-Za-z0-9_.\-]+)\s*=\s*"(.*)"\s*$`)

// parseSinkProps returns the Name/properties block of `pactl list sinks` for
// the sink called name, or nil.
func parseSinkProps(long, name string) map[string]string {
	for _, blk := range strings.Split(long, "Sink #") {
		props := map[string]string{}
		found := false
		for _, ln := range strings.Split(blk, "\n") {
			t := strings.TrimSpace(ln)
			if strings.HasPrefix(t, "Name: ") {
				found = strings.TrimPrefix(t, "Name: ") == name
				continue
			}
			if strings.HasPrefix(t, "State: ") {
				props["State"] = strings.TrimPrefix(t, "State: ")
			}
			if strings.HasPrefix(t, "Mute: ") {
				props["Mute"] = strings.TrimPrefix(t, "Mute: ")
			}
			if strings.HasPrefix(t, "Volume: ") {
				props["Volume"] = strings.TrimPrefix(t, "Volume: ")
			}
			if strings.HasPrefix(t, "Description: ") {
				props["Description"] = strings.TrimPrefix(t, "Description: ")
			}
			if m := propRe.FindStringSubmatch(ln); m != nil {
				props[m[1]] = m[2]
			}
		}
		if found {
			return props
		}
	}
	return nil
}

type streamInfo struct {
	ID     string
	App    string
	Mute   bool
	MinPct int // lowest channel volume in %, -1 when not reported
}

func (st streamInfo) String() string { return strings.TrimSpace("#" + st.ID + " " + st.App) }

// minPct is the lowest percentage on a pactl "Volume:" line, or -1.
func minPct(line string) int {
	m := pctRe.FindAllStringSubmatch(line, -1)
	if len(m) == 0 {
		return -1
	}
	low := -1
	for _, x := range m {
		n, _ := strconv.Atoi(x[1])
		if low < 0 || n < low {
			low = n
		}
	}
	return low
}

// streamsOn lists the uncorked sink inputs playing into sink index idx
// (`pactl list sink-inputs`) with their own mute and volume.
func streamsOn(long, idx string) []streamInfo {
	var out []streamInfo
	parts := strings.Split(long, "Sink Input #")
	for _, blk := range parts[1:] {
		id, _, _ := strings.Cut(blk, "\n")
		st := streamInfo{ID: strings.TrimSpace(id), MinPct: -1}
		sink, corked := "", ""
		for _, ln := range strings.Split(blk, "\n") {
			t := strings.TrimSpace(ln)
			switch {
			case strings.HasPrefix(t, "Sink: "):
				sink = strings.TrimPrefix(t, "Sink: ")
			case strings.HasPrefix(t, "Corked: "):
				corked = strings.TrimPrefix(t, "Corked: ")
			case strings.HasPrefix(t, "Mute: "):
				st.Mute = strings.TrimPrefix(t, "Mute: ") == "yes"
			case strings.HasPrefix(t, "Volume: "):
				st.MinPct = minPct(t)
			case strings.HasPrefix(t, "application.name = "):
				st.App = strings.Trim(strings.TrimPrefix(t, "application.name = "), `"`)
			}
		}
		if sink == idx && corked != "yes" {
			out = append(out, st)
		}
	}
	return out
}

// activeStreamsOn lists the uncorked streams on the sink as "#id app".
func activeStreamsOn(long, idx string) []string {
	var out []string
	for _, st := range streamsOn(long, idx) {
		out = append(out, st.String())
	}
	return out
}

var rawVolRe = regexp.MustCompile(`(\d+)\s*/\s*\d+%`)
var pctRe = regexp.MustCompile(`(\d+)%`)

// parseSinkVolume reads the per-channel RAW values of `pactl get-sink-volume`
// (65536 = 100%), the most precise thing it prints; percentages are the
// fallback when the raw column is absent.
func parseSinkVolume(s string) (vals []string, err error) {
	for _, ln := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(ln), "Volume:") {
			continue
		}
		for _, m := range rawVolRe.FindAllStringSubmatch(ln, -1) {
			vals = append(vals, m[1])
		}
		if len(vals) == 0 {
			for _, m := range pctRe.FindAllStringSubmatch(ln, -1) {
				vals = append(vals, m[1]+"%")
			}
		}
		if len(vals) > 0 {
			return vals, nil
		}
	}
	return nil, fmt.Errorf("cannot read the sink volume from %q", strings.TrimSpace(s))
}

// parseMute reads `pactl get-sink-mute` ("Mute: yes").
func parseMute(s string) (bool, error) {
	f := strings.Fields(s)
	if len(f) >= 2 && f[0] == "Mute:" {
		switch f[1] {
		case "yes":
			return true, nil
		case "no":
			return false, nil
		}
	}
	return false, fmt.Errorf("cannot read the sink mute from %q", strings.TrimSpace(s))
}

// ---------- MPRIS ----------

type mprisCtl struct {
	list   func() ([]string, error)
	status func(p string) (string, error)
	pause  func(p string) error
	play   func(p string) error
}

const (
	mprisPath  = "/org/mpris/MediaPlayer2"
	mprisIface = "org.mpris.MediaPlayer2.Player"
)

// mprisFor picks playerctl, else busctl --user; neither -> error (the test
// cannot know that nothing is playing).
func mprisFor(env hearingEnv) (*mprisCtl, error) {
	switch {
	case env.have("playerctl"):
		return &mprisCtl{
			list: func() ([]string, error) {
				o, err := env.exec("playerctl", "-l")
				if err != nil && strings.TrimSpace(o) == "" && !strings.Contains(err.Error(), "exit status 1") {
					return nil, err
				}
				return strings.Fields(o), nil // exit 1 = no players
			},
			status: func(p string) (string, error) {
				o, err := env.exec("playerctl", "--player="+p, "status")
				return strings.TrimSpace(o), err
			},
			pause: func(p string) error { _, err := env.exec("playerctl", "--player="+p, "pause"); return err },
			play:  func(p string) error { _, err := env.exec("playerctl", "--player="+p, "play"); return err },
		}, nil
	case env.have("busctl"):
		return &mprisCtl{
			list: func() ([]string, error) {
				o, err := env.exec("busctl", "--user", "list", "--no-legend")
				if err != nil {
					return nil, err
				}
				var out []string
				for _, ln := range strings.Split(o, "\n") {
					if f := strings.Fields(ln); len(f) > 0 && strings.HasPrefix(f[0], "org.mpris.MediaPlayer2.") {
						out = append(out, f[0])
					}
				}
				return out, nil
			},
			status: func(p string) (string, error) {
				o, err := env.exec("busctl", "--user", "get-property", p, mprisPath, mprisIface, "PlaybackStatus")
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(o), "s")), `"`), err
			},
			pause: func(p string) error {
				_, err := env.exec("busctl", "--user", "call", p, mprisPath, mprisIface, "Pause")
				return err
			},
			play: func(p string) error {
				_, err := env.exec("busctl", "--user", "call", p, mprisPath, mprisIface, "Play")
				return err
			},
		}, nil
	}
	return nil, errors.New("neither playerctl nor busctl found: cannot make sure the music is paused, so the test will not raise the volume")
}

// ---------- the run ----------

var errInterrupted = errors.New("interrupted")

// How long the run waits for the sink to go quiet after the players were
// paused: audioStopPolls polls, audioStopPoll apart (8 s).
const (
	audioStopPoll  = 250 * time.Millisecond
	audioStopPolls = 32
)

// silencePeak: a monitor reading whose largest |s16 sample| is below this is
// silence (about -70 dBFS). Two consecutive readings are required.
const silencePeak = 10

// peakS16 is the largest |sample| of s16le mono PCM and its level in dBFS.
func peakS16(b []byte) (int, float64) {
	peak := 0
	for i := 0; i+1 < len(b); i += 2 {
		v := int(int16(uint16(b[i]) | uint16(b[i+1])<<8))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	if peak == 0 {
		return 0, math.Inf(-1)
	}
	return peak, 20 * math.Log10(float64(peak)/32768)
}

// errToneDead: the tone stopped playing while waiting for the answer.
var errToneDead = errors.New("tone died")

// maxToneRetries is how many times a band's tone is replayed after dying
// before the test aborts.
const maxToneRetries = 2

type heardRec struct {
	Tone  hearing.Tone `json:"tone"`
	Heard bool         `json:"heard"`
}

type hearingRunOut struct {
	Timestamp  string     `json:"timestamp"`
	Sink       string     `json:"sink"`
	SinkVolume int        `json:"sinkVolume"`
	DeviceMode bool       `json:"deviceMode"`
	SoundDB    []float64  `json:"soundDb"`
	Answers    []heardRec `json:"answers"`
	Left       []int      `json:"left"`
	Right      []int      `json:"right"`
	// Floor: bands still heard at the lowest level of the staircase (the gain
	// is a bound, not a measurement).
	Floor   map[string][]bool `json:"floor"`
	Result  resultOut         `json:"result"`
	Applied bool              `json:"applied"`
	// Baseline is "zero" for the experiment; absent means the factory baseline.
	Baseline string              `json:"baseline,omitempty"`
	Backup   string              `json:"backup,omitempty"`
	Restore  string              `json:"restore,omitempty"`
	Writes   []*hearingCall      `json:"writes,omitempty"`
	Final    *qcycore.EqSnapshot `json:"finalEq,omitempty"`
}

type hearingBackup struct {
	Timestamp string                 `json:"timestamp"`
	Status    map[string]interface{} `json:"status"`
	Eq        *qcycore.EqSnapshot    `json:"eq"`
	Library   string                 `json:"libraryId"`
	Restore   string                 `json:"restore"`
	// Slot11 is the library entry that preserves slot 11 (the hearing-test
	// curve the write overwrites) and Slot11Restore the command that reapplies it.
	Slot11        string `json:"slot11Backup,omitempty"`
	Slot11Restore string `json:"slot11Restore,omitempty"`
}

// hearingRunLive runs the whole test. Cleanup runs on every return path. With
// o.events set it also writes the JSON-lines events (hearing_events.go); the
// terminal event "stopped" is written only after the cleanup.
func hearingRunLive(env hearingEnv, o hearingRunOpts) (err error) {
	if o.events == nil {
		return hearingRunCore(env, o, func(string, map[string]interface{}) {})
	}
	emit := newEventEmitter(o.events)
	// A panic on this goroutine has already run the core's cleanup (deferred
	// undo chain) when it gets here: report it and still close the stream.
	defer func() {
		if r := recover(); r != nil {
			err = finishEvents(emit, fmt.Errorf("panic: %v", r))
		}
	}()
	err = hearingRunCore(env, o, emit)
	return finishEvents(emit, err)
}

func hearingRunCore(env hearingEnv, o hearingRunOpts, emit func(string, map[string]interface{})) (err error) {
	out := env.out
	if env.sleep == nil {
		env.sleep = time.Sleep
	}
	eventsMode := o.events != nil
	warnf := func(f string, a ...interface{}) {
		fmt.Fprintf(out, "warning: "+f+"\n", a...)
		emit("warning", map[string]interface{}{"message": fmt.Sprintf(f, a...)})
	}
	phase := func(name, msg string) {
		emit("phase", map[string]interface{}{"name": name, "message": msg})
	}

	// One permanent watcher turns any signal into an abort that every phase
	// observes (checkAbort, waitKey).
	ab := newAborter(env.sigs)
	defer ab.close()
	abort, checkAbort, abortCh := ab.abort, ab.check, ab.ch

	// 1. Instant, local checks only (pactl/busctl answer in milliseconds; the
	// daemon is not touched yet): nothing audible or persistent has happened.
	var restoreCmd, backupPath string
	var backupInfo hearingBackupInfo
	if !env.have("pactl") || !env.have("pw-cat") {
		return errors.New("hearing run needs pactl and pw-cat (PipeWire) in PATH")
	}
	mp, err := mprisFor(env)
	if err != nil {
		return err
	}
	tgt, err := resolveBluezSink(env, o.sinkName)
	if err != nil {
		return err
	}
	sink := tgt.Name
	fmt.Fprintf(out, "sink: %s  (%s, address %s)  volume %d%%\n", sink, tgt.Desc, tgt.Addr, o.sinkVolume)
	if e := checkAbort(); e != nil {
		return e
	}
	keys, restoreTTY, err := env.keys()
	if err != nil {
		return err
	}
	defer restoreTTY()

	// The key reader starts before any side effect so that, in events mode,
	// "abort" (or EOF) on stdin is seen by every phase, the music pause and the
	// silence wait included, and nothing is raised after it.
	keyCh := make(chan byte, 16)
	keyErr := make(chan error, 1)
	// curTone is the index of the tone being presented (events mode: a command
	// "heard N" / "not-heard N" for another tone is discarded).
	var curTone int64
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
				if k == 'q' {
					ab.abort(errStopRequested)
					return
				}
				if ik, ok := keys.(interface{ LastIndex() int }); ok {
					if n := ik.LastIndex(); n > 0 && int64(n) != atomic.LoadInt64(&curTone) {
						continue // an answer to a tone that is not the current one
					}
				}
				select { // never block: an "abort" behind a full queue must be read
				case keyCh <- k:
				default:
				}
				continue
			}
			keyCh <- k
		}
	}()

	// 2. Side effects, each with its undo registered before it is attempted.
	var undo []func()
	defer func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()

	var stopTone func() error
	// stopNow stops the tone and returns the player's verdict (nil when none
	// was playing).
	stopNow := func() error {
		if stopTone == nil {
			return nil
		}
		e := stopTone()
		stopTone = nil
		return e
	}
	endTone := func() {
		if e := stopNow(); e != nil {
			warnf("tone: %v", e)
		}
	}

	// Music: pause what is Playing (resume only that), confirm it is paused,
	// then refuse to go on while any stream still plays into the sink.
	phase("pausing-music", "pausing the music")
	if err := pauseMusic(env, mp, func(f func()) { undo = append(undo, f) }, warnf); err != nil {
		return err
	}
	// The daemon is only needed from here on. `capabilities` is answered from
	// the daemon's cache (no BLE sweep, unlike `status`, which takes ~8 s).
	fmt.Fprintln(out, "checking the earbuds…")
	phase("checking-earbuds", "checking the earbuds")
	link, err := env.link()
	if err != nil {
		return fmt.Errorf("hearing run needs the qcyctld daemon: %w", err)
	}
	// Closed by the undo chain, NOT by a loose defer: a deferred Close would
	// run before the chain (LIFO) and the test-mode off frame would hit a
	// closed connection. Registered here, it runs after everything registered
	// later (tone, mode off, volume).
	undo = append(undo, func() { _ = link.Close() })
	verified, err := link.capabilities()
	if err != nil {
		return fmt.Errorf("capabilities: %w", err)
	}
	if !verified {
		return errors.New("hearing run is only for a verified model (this earbud is not)")
	}
	// The backup (full status + raw active EQ + library entry) is slow (BLE)
	// and only needed with --apply --yes: it runs while the audio settles and
	// is joined before anything audible changes. A failure aborts the run
	// with nothing changed on the earbuds.
	var backupCh chan error
	if o.apply && o.yes {
		fmt.Fprintln(out, "reading the earbuds' state for the backup…")
		backupCh = make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					backupCh <- fmt.Errorf("backup panicked: %v", r)
				}
			}()
			backupCh <- func() error {
				info, err := hearingTakeBackup(env, link, o.noSlot11Backup)
				if err != nil {
					return err
				}
				backupInfo = info
				backupPath, restoreCmd = info.Path, info.Restore
				return nil
			}()
		}()
	}
	phase("waiting-silence", "waiting for the sink to be silent")
	if err := waitSinkSilent(env, tgt, checkAbort); err != nil {
		return err
	}
	if e := checkAbort(); e != nil {
		return e
	}

	if backupCh != nil {
		select {
		case e := <-backupCh:
			if e != nil {
				return fmt.Errorf("backup failed, nothing was changed on the earbuds: %w", e)
			}
		case <-abortCh:
			return checkAbort()
		}
		printBackup(out, backupInfo)
	}

	// Sink volume and mute.
	phase("starting", "setting the volume")
	volUndo, markVol := tgt.restoreVolume(env, warnf)
	undo = append(undo, volUndo)
	markVol()
	if _, e := env.exec("pactl", "set-sink-volume", sink, fmt.Sprintf("%d%%", o.sinkVolume)); e != nil {
		return fmt.Errorf("pactl set-sink-volume: %w", e)
	}
	if _, e := env.exec("pactl", "set-sink-mute", sink, "0"); e != nil {
		return fmt.Errorf("pactl set-sink-mute: %w", e)
	}

	// Device test mode (0x45, NOT verified): only with --device-mode or
	// --apply --yes. Always switched off again, even when switching it on
	// failed half way (the frame may have reached the earbuds).
	useMode := o.deviceMode || (o.apply && o.yes)
	modeOn := false
	modeOff := func() {
		if !modeOn {
			return
		}
		modeOn = false
		if _, e := link.hearingMode(false); e != nil {
			msg := "run: qcyctl hearing mode off --yes"
			if restoreCmd != "" {
				msg += "; to restore the previous state: " + restoreCmd
			}
			warnf("could not switch the test mode off (%v); %s", e, msg)
		}
	}
	undo = append(undo, modeOff)
	if useMode {
		modeOn = true
		if _, e := link.hearingMode(true); e != nil {
			return fmt.Errorf("hearing mode on: %w", e)
		}
	} else {
		fmt.Fprintln(out, "device test mode (0x45) not sent: pass --device-mode (or --apply --yes) to enable it")
	}

	if env.pingEvery > 0 {
		stopPing := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					abort(fmt.Errorf("keepalive panicked: %v", r))
				}
			}()
			t := time.NewTicker(env.pingEvery)
			defer t.Stop()
			for {
				select {
				case <-stopPing:
					return
				case <-t.C:
					link.ping()
				}
			}
		}()
		undo = append(undo, func() { close(stopPing); wg.Wait() })
	}

	// The tone is the first thing the cleanup silences, before the volume is
	// put back and the music resumed (undo runs last-registered first).
	undo = append(undo, endTone)

	// 3. The staircase.
	waitKey := func(dead <-chan struct{}) (byte, error) {
		select { // a dead tone wins over a queued key
		case <-dead:
			return 0, errToneDead
		default:
		}
		select {
		case <-dead:
			return 0, errToneDead
		case k := <-keyCh:
			return k, nil
		case e := <-keyErr:
			if errors.Is(e, io.EOF) {
				return 0, errors.New("input closed: test aborted")
			}
			return 0, e
		case <-abortCh:
			return 0, checkAbort()
		}
	}
	drainKeys := func() {
		for {
			select {
			case <-keyCh:
			default:
				return
			}
		}
	}
	sinkAlive := func() error {
		l, e := env.exec("pactl", "list", "short", "sinks")
		if e != nil {
			return fmt.Errorf("pactl list short sinks: %w", e)
		}
		for _, s := range parseSinks(l) {
			if s.Name == sink {
				return nil
			}
		}
		return fmt.Errorf("sink %s disappeared (the earbuds disconnected?): aborted", sink)
	}

	if !eventsMode {
		fmt.Fprintln(out, "keys: h/Enter = I hear it   n/space = I do not   r = repeat   q = abort")
	}
	phase("testing", "the test is running")
	sess := hearing.NewSession(o.soundDB)
	var answers []heardRec
	fails := map[int]int{}
	for !sess.Done() {
		t := sess.Current()
		fmt.Fprintf(out, "[%2d] %-5s ear  band %d/%d  %5d Hz  step %+d  %6.1f dBFS ... ", len(answers)+1, t.Ear, t.Band+1, hearing.Bands, t.Freq, t.Gain, t.LevelDB)
		answered := false
		for !answered {
			// Every presentation of a tone starts with an empty key queue: a
			// key typed for a tone that died (or in the gaps) is not an
			// answer to this one.
			drainKeys()
			if e := checkAbort(); e != nil {
				fmt.Fprintln(out)
				return e
			}
			if e := sinkAlive(); e != nil {
				fmt.Fprintln(out)
				return e
			}
			drainKeys() // the window of sinkAlive and of the previous tone's stop
			atomic.StoreInt64(&curTone, int64(len(answers)+1))
			stop, dead, e := env.player.Start(sink, t.Ear, t.Freq, t.LevelDB)
			if e != nil {
				return fmt.Errorf("play tone: %w", e)
			}
			stopTone = stop
			emit("tone", map[string]interface{}{
				"index": len(answers) + 1, "ear": t.Ear.String(), "band": t.Band, "bands": hearing.Bands,
				"freq": t.Freq, "step": t.Gain, "levelDb": t.LevelDB,
				"progress": float64(int(t.Ear)*hearing.Bands+t.Band) / float64(2*hearing.Bands),
			})
			var key byte
			died := false
		keyLoop:
			for {
				k, e := waitKey(dead)
				if errors.Is(e, errToneDead) {
					died = true
					break keyLoop
				}
				if e != nil {
					endTone()
					fmt.Fprintln(out)
					return e
				}
				key = k
				switch k {
				case 'h', 'H', '\r', '\n', 'n', 'N', ' ', 'r', 'R', 'q', 'Q':
					break keyLoop
				}
			}
			if !died {
				select {
				case <-dead:
					died = true
				default:
				}
			}
			stopErr := stopNow()
			if key == 'q' || key == 'Q' {
				fmt.Fprintln(out, "aborted")
				return errors.New("aborted by the listener (q); nothing was written")
			}
			if stopErr != nil {
				died = true
			}
			if died {
				// The tone was not (fully) played: the answer, if any, is not
				// a measurement. Replay it, a bounded number of times.
				id := int(t.Ear)*hearing.Bands + t.Band
				fails[id]++
				if fails[id] > maxToneRetries {
					fmt.Fprintln(out)
					return fmt.Errorf("the tone player kept failing at %s ear %d Hz (%v); aborted, nothing was written", t.Ear, t.Freq, stopErr)
				}
				fmt.Fprintf(out, "tone interrupted, answer discarded, repeating (%d/%d) ... ", fails[id], maxToneRetries)
				emit("warning", map[string]interface{}{"message": fmt.Sprintf("tone interrupted, answer discarded, repeating (%d/%d)", fails[id], maxToneRetries)})
				continue
			}
			switch key {
			case 'r', 'R':
				fmt.Fprint(out, "repeat ... ")
			case 'h', 'H', '\r', '\n':
				fmt.Fprintln(out, "heard")
				answers = append(answers, heardRec{Tone: t, Heard: true})
				emit("answer", map[string]interface{}{"ear": t.Ear.String(), "band": t.Band, "heard": true})
				if e := sess.Answer(true); e != nil {
					return e
				}
				answered = true
				delete(fails, int(t.Ear)*hearing.Bands+t.Band)
			case 'n', 'N', ' ':
				fmt.Fprintln(out, "not heard")
				answers = append(answers, heardRec{Tone: t, Heard: false})
				emit("answer", map[string]interface{}{"ear": t.Ear.String(), "band": t.Band, "heard": false})
				if e := sess.Answer(false); e != nil {
					return e
				}
				answered = true
				delete(fails, int(t.Ear)*hearing.Bands+t.Band)
			}
		}
	}

	// 4. Result.
	phase("finishing", "computing the result")
	l, r := sess.Result()
	var gl, gr [hearing.Bands]float64
	for i := range l {
		gl[i], gr[i] = float64(l[i]), float64(r[i])
	}
	res := computeResult(gl, gr)
	fl, fr := sess.Floor()
	nFloor := printFloor(out, l, r, fl, fr)
	printResultText(out, res)
	printFloorWarning(out, nFloor)
	rec := hearingRunOut{
		Timestamp: env.now().Format(time.RFC3339), Sink: sink, SinkVolume: o.sinkVolume, DeviceMode: useMode,
		SoundDB: o.soundDB[:], Answers: answers, Left: l[:], Right: r[:], Result: res,
		Floor: map[string][]bool{"left": fl[:], "right": fr[:]},
	}
	if o.baseline == "zero" {
		rec.Baseline = "zero"
	}
	save := func() error {
		if o.outFile == "" {
			return nil
		}
		return writeJSONFile(o.outFile, rec)
	}

	switch {
	case o.apply && o.yes:
		endTone()
		if e := checkAbort(); e != nil {
			_ = save()
			return e
		}
		if nFloor > 0 && !o.acceptFloor {
			_ = save()
			return fmt.Errorf("refused to write: %d band(s) were still heard at the lowest level, so the result is only relative. Rerun with a lower --sink-volume (e.g. 60%%), or pass --accept-floor to write it anyway; nothing was written (restore point: %s)", nFloor, restoreCmd)
		}
		rec.Backup, rec.Restore = backupPath, restoreCmd
		writes, final, e := hearingCommit(out, link, l, r, restoreCmd, modeOff, baselineOrFactory(o.baseline), env.recorder(l, r, o.sinkVolume, rec.Timestamp, baselineOrFactory(o.baseline)))
		rec.Writes, rec.Final = writes, final
		if e != nil {
			_ = save()
			return e
		}
		rec.Applied = true
	case o.apply:
		fmt.Fprintln(out, "--apply needs --yes: nothing was written to the earbuds.")
	default:
		fmt.Fprintln(out, "nothing was written to the earbuds (use --apply --yes to write).")
	}
	if e := save(); e != nil {
		return e
	}
	if eventsMode {
		emit("result", map[string]interface{}{
			"left": l[:], "right": r[:],
			"floor":      map[string][]bool{"left": fl[:], "right": fr[:]},
			"masterGain": res.MasterGain, "outFile": o.outFile,
		})
	}
	return nil
}

func baselineOrFactory(b string) string {
	if b == "zero" {
		return "zero"
	}
	return "factory"
}

// readbackBands decodes the bands of a 0x22 readback.
func readbackBands(c *hearingCall) ([]qcycore.EqBand, bool) {
	if c.Readback == nil {
		return nil, false
	}
	p, err := hex.DecodeString(c.Readback.Payload)
	if err != nil {
		return nil, false
	}
	snap, err := qcycore.DecodeEq(p)
	if err != nil {
		return nil, false
	}
	return snap.Bands, true
}

// tailMatches compares eight bands positionally with whole-dB gains (0.1 dB).
func tailMatches(tail []qcycore.EqBand, gains [hearing.Bands]int) bool {
	if len(tail) != hearing.Bands {
		return false
	}
	for i, g := range gains {
		if tail[i].Freq != hearing.Freqs[i] || tail[i].Gain-float64(g) > 0.101 || float64(g)-tail[i].Gain > 0.101 {
			return false
		}
	}
	return true
}

// prefixIsZero: the first five bands are the factory baseline's frequencies at
// gain 0 (what the "zero" experiment wrote).
func prefixIsZero(bands []qcycore.EqBand) bool {
	for i, f := range hearing.FactoryBaseline {
		if bands[i].Freq != f.Freq || bands[i].Gain > 0.101 || bands[i].Gain < -0.101 {
			return false
		}
	}
	return true
}

// readbackShows checks that the 0x22 readback shows the eight gains, BY
// POSITION (the 8000 Hz band of the default effect would collide with the test's
// 8000 Hz band in a lookup by frequency), and returns an experiment note when
// the layout is one of the unexpected-but-understood ones.
//
// baseline "factory" (the default; measured): 13 bands = the 5 of the default
// effect + the 8 test bands, or a bare 8.
//
// baseline "zero" (EXPERIMENT, not verified): 13 bands with the first 5 at
// gain 0 is the expected outcome; 18 bands (the firmware prefixed the factory
// baseline again) and 13 bands with a non-zero prefix (it replaced the zeros)
// are reported as results of the experiment, not as failures; any other band
// count is not confirmed.
func readbackShows(c *hearingCall, gains [hearing.Bands]int, baseline string) (ok bool, note string) {
	bands, have := readbackBands(c)
	if !have || !readbackMasterMatches(c) {
		return false, ""
	}
	n := len(bands)
	if baseline != "zero" {
		if n != hearing.Bands && n != 5+hearing.Bands {
			return false, ""
		}
		return tailMatches(bands[n-hearing.Bands:], gains), ""
	}
	switch n {
	case 5 + hearing.Bands:
		if !tailMatches(bands[5:], gains) {
			return false, ""
		}
		if prefixIsZero(bands) {
			return true, ""
		}
		return true, "firmware replaced the zero baseline with the factory baseline (13 bands)"
	case 10 + hearing.Bands:
		if !tailMatches(bands[10:], gains) || !prefixIsZero(bands[5:]) {
			return false, ""
		}
		return true, "firmware still prefixes the factory baseline (18 bands)"
	}
	return false, ""
}

// readbackMasterMatches: the readback's master gain is the one written (0.05 dB).
func readbackMasterMatches(c *hearingCall) bool {
	if c.Readback == nil || c.MasterGain == nil {
		return false
	}
	d := c.Readback.MasterGain - *c.MasterGain
	return d <= 0.051 && d >= -0.051
}

// writeConfirmed (factory baseline): the device echoed the opcode, or its 0x22
// readback shows the master gain and the eight written bands.
func writeConfirmed(c *hearingCall, gains [hearing.Bands]int) bool {
	ok, _ := writeConfirmedB(c, gains, "factory")
	return ok
}

// writeConfirmedB is writeConfirmed for a baseline; note carries the result of
// the experiment, if any.
func writeConfirmedB(c *hearingCall, gains [hearing.Bands]int, baseline string) (bool, string) {
	if c.Ack {
		return true, ""
	}
	return readbackShows(c, gains, baseline)
}

// rightUnverifiable: the 0x22 reports ONE ear (measured: the left one), so
// after the right ear is written its curve never shows up. The right write is
// accepted as "sent, not independently verifiable" only when the readback is
// recognisably the left curve that was just confirmed, with the written master.
func rightUnverifiable(c *hearingCall, left [hearing.Bands]int, baseline string) (bool, string) {
	return readbackShows(c, left, baseline)
}

// earsNote is the state text stored with a partially confirmed result.
const partialConfirmNote = "esquerda confirmada pelo readback do 0x22; direita enviada, não verificável de forma independente (o 0x22 reporta a orelha esquerda)"

// printFloor prints the per-ear gains with the floor bands marked and returns
// how many bands are at the floor.
func printFloor(out io.Writer, l, r [hearing.Bands]int, fl, fr [hearing.Bands]bool) int {
	n := 0
	line := func(name string, g [hearing.Bands]int, f [hearing.Bands]bool) {
		fmt.Fprintf(out, "%-5s:", name)
		for i, v := range g {
			mark := ""
			if f[i] {
				mark = " floor"
				n++
			}
			fmt.Fprintf(out, " %d%s", v, mark)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out)
	line("left", l, fl)
	line("right", r, fr)
	return n
}

func printFloorWarning(out io.Writer, n int) {
	if n > 0 {
		fmt.Fprintf(out, "WARNING: %d bands were still heard at the lowest level: the result is only relative; rerun with a lower --sink-volume (e.g. 60%%)\n", n)
	}
}

func writeJSONFile(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ---------- real implementations ----------

type liveHearingLink struct {
	mu sync.Mutex
	dc *daemonClient
}

func (l *liveHearingLink) call(cmd string, args map[string]interface{}) (*hearingCall, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := l.dc.request(cmd, args)
	if err != nil {
		return nil, err
	}
	var c hearingCall
	if err := remarshal(data, &c); err != nil {
		return nil, fmt.Errorf("daemon returned a malformed %s result: %w", cmd, err)
	}
	return &c, nil
}

func (l *liveHearingLink) status() (map[string]interface{}, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dc.status()
}

func (l *liveHearingLink) capabilities() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := l.dc.request("capabilities", nil)
	if err != nil {
		return false, err
	}
	m, ok := data.(map[string]interface{})
	if !ok {
		return false, errors.New("daemon returned a non-object capabilities reply")
	}
	v, _ := m["verified"].(bool)
	return v, nil
}

func (l *liveHearingLink) hearingMode(on bool) (*hearingCall, error) {
	return l.call("hearingMode", map[string]interface{}{"on": on, "confirm": true})
}

func (l *liveHearingLink) hearingWrite(ear hearing.Ear, gains, other [hearing.Bands]int, baseline string) (*hearingCall, error) {
	args := map[string]interface{}{
		"ear": ear.String(), "gains": gains[:], "otherGains": other[:], "confirm": true,
	}
	if baseline == "zero" {
		args["baseline"] = "zero"
	}
	return l.call("hearingWrite", args)
}

func (l *liveHearingLink) eqRead() (*qcycore.EqSnapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dc.eqRead()
}

func (l *liveHearingLink) ping() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.dc.request("battery", nil)
}

func (l *liveHearingLink) Close() error { return l.dc.Close() }

func dialHearingLink() (hearingLink, error) {
	dc, err := dialDaemon(2 * time.Second)
	if err != nil {
		return nil, err
	}
	return &liveHearingLink{dc: dc}, nil
}

// pwcatPlayer plays through `pw-cat --playback` fed on stdin. bin is the
// executable (a test points it at a fake).
type pwcatPlayer struct{ bin string }

const (
	toneWriteDeadline = 500 * time.Millisecond
	toneStopGrace     = 600 * time.Millisecond
)

// pwcatArgs are the arguments of pw-cat: stdin ("-") carries RAW s16le stereo
// 44.1 kHz PCM, not an audio file, so --raw is required; without it pw-cat
// tries to sniff a container with libsndfile and dies with "Format not
// recognised". The properties keep the stream on the earbuds' sink (no
// fallback to another device, no reconnect elsewhere).
func pwcatArgs(sink string) []string {
	return []string{"--playback", "--raw", "--target", sink,
		"-P", "node.dont-fallback=true node.dont-reconnect=true",
		"--format", "s16", "--rate", "44100", "--channels", "2", "-"}
}

func (p pwcatPlayer) Start(sink string, ear hearing.Ear, freq int, levelDB float64) (func() error, <-chan struct{}, error) {
	synth := hearing.NewSynth(ear, freq, levelDB)
	ramp := hearing.RampSamples()
	return startPwcat(p.bin, sink, func(write func([]byte) error, quit <-chan struct{}) error {
		if e := write(synth.Next(ramp, hearing.FadeIn)); e != nil {
			return e
		}
		for {
			select {
			case <-quit:
				return write(synth.Next(ramp, hearing.FadeOut))
			default:
			}
			if e := write(synth.Next(ramp, hearing.Steady)); e != nil {
				return e
			}
		}
	})
}

// startPwcat starts `pw-cat --playback` on sink and runs produce in a
// goroutine to feed its stdin (write blocks at the player's pace and fails
// after toneWriteDeadline). produce returns once quit is closed (after a
// short fade-out) or when a write fails. stop ends it in bounded time and
// returns the producer's verdict; dead is closed as soon as nothing can be
// heard any more (pw-cat exited, a write failed).
func startPwcat(bin, sink string, produce func(write func([]byte) error, quit <-chan struct{}) error) (func() error, <-chan struct{}, error) {
	if bin == "" {
		bin = "pw-cat"
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	shrinkPipe(pw)
	cmd := exec.Command(bin, pwcatArgs(sink)...)
	cmd.Stdin = pr
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, err
	}
	pr.Close()
	dead := make(chan struct{})
	var deadOnce sync.Once
	markDead := func() { deadOnce.Do(func() { close(dead) }) }
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); markDead(); close(exited) }()

	quit := make(chan struct{})
	done := make(chan error, 1)
	write := func(b []byte) error {
		_ = pw.SetWriteDeadline(time.Now().Add(toneWriteDeadline))
		_, e := pw.Write(b)
		if e != nil {
			markDead()
		}
		return e
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				markDead()
				done <- fmt.Errorf("tone writer panicked: %v", r)
			}
		}()
		done <- produce(write, quit)
	}()
	stop := func() error {
		close(quit)
		var werr error
		select {
		case werr = <-done:
		case <-time.After(toneStopGrace):
			werr = errors.New("tone writer did not stop in time; pw-cat killed")
			_ = cmd.Process.Kill()
			pw.Close()
			select {
			case <-done:
			case <-time.After(toneStopGrace):
			}
		}
		pw.Close()
		select {
		case <-exited:
		case <-time.After(toneStopGrace):
			_ = cmd.Process.Kill()
			select {
			case <-exited:
			case <-time.After(toneStopGrace):
			}
		}
		return werr
	}
	return stop, dead, nil
}

// guardedPlayer refuses to play while hardware is disabled (tests).
type guardedPlayer struct{ tonePlayer }

func (g guardedPlayer) Start(sink string, ear hearing.Ear, freq int, level float64) (func() error, <-chan struct{}, error) {
	if err := refuseHardware(); err != nil {
		return nil, nil, err
	}
	return g.tonePlayer.Start(sink, ear, freq, level)
}

type stdinKeys struct{}

func (stdinKeys) ReadKey() (byte, error) {
	var b [1]byte
	for {
		n, err := os.Stdin.Read(b[:])
		if n == 1 {
			return b[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// hearingExecTimeout bounds every pactl/playerctl/busctl call.
const hearingExecTimeout = 3 * time.Second

func liveExec(name string, args ...string) (string, error) {
	if err := refuseHardware(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hearingExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.Output()
	return string(b), err
}

// liveMonitor records ~0.4 s of one sink-input with parec (read only).
// --monitor-stream taps the stream before the sink's volume and mute, so a
// quiet sink does not hide it; the stream's own volume still applies.
// Evidence (measured live, sink at 30%): the plain sink monitor read a peak of
// 15047 from ANOTHER stream (Brave), while --monitor-stream=<index> isolated
// our -50 dBFS tone and read 38, i.e. pre-sink-volume (post-volume it would
// have been about 1).
func liveMonitor(sink, streamID string) ([]byte, error) {
	if err := refuseHardware(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "parec", "-d", sink+".monitor", "--monitor-stream="+streamID, "--format=s16le", "--rate=8000", "--channels=1", "--latency-msec=50")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err := cmd.Run() // killed by the deadline on purpose: the bytes read are the reading
	if buf.Len() == 0 {
		if err == nil {
			err = errors.New("parec returned no data")
		}
		return nil, err
	}
	return buf.Bytes(), nil
}

func saveBackupCurve(name string, snap *qcycore.EqSnapshot) (string, error) {
	if snap == nil {
		return "", errors.New("no active EQ snapshot")
	}
	c, err := qcycore.SeedFromPayload(qcycore.SourceDeviceImport, snap.Slot, name, snap.Payload)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

func liveHearingEnv(out io.Writer) (hearingEnv, func()) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return hearingEnv{
		out:  out,
		exec: liveExec,
		have: func(name string) bool { _, err := exec.LookPath(name); return err == nil },
		link: func() (hearingLink, error) {
			return dialHearingLink()
		},
		player: guardedPlayer{pwcatPlayer{bin: "pw-cat"}},
		keys: func() (keySource, func(), error) {
			if err := refuseHardware(); err != nil {
				return nil, nil, err
			}
			restore, err := rawTerminal(int(os.Stdin.Fd()))
			if err != nil {
				return nil, nil, err
			}
			return stdinKeys{}, restore, nil
		},
		sigs: sigs,
		cachedMAC: func() string {
			if a, ok := loadCachedAddr(); ok {
				return a.String()
			}
			return ""
		},
		now:         time.Now,
		sleep:       time.Sleep,
		configDir:   qcyConfigDir,
		saveCurve:   saveBackupCurve,
		recordCurve: qcycore.UpsertCurve,
		slot11:      func(snap *qcycore.EqSnapshot) (slot11Backup, error) { return hearingSlot11Backup(snap, time.Now()) },
		monitor:     liveMonitor,
		pingEvery:   3 * time.Second,
	}, func() { signal.Stop(sigs) }
}
