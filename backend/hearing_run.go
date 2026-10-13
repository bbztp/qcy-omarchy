package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bbztp/qcy-omarchy/backend/internal/hearing"
	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
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
	// hearingMode sends 0x45 (with confirm:true).
	hearingMode(on bool) (*hearingCall, error)
	hearingWrite(ear hearing.Ear, gains, other [hearing.Bands]int) (*hearingCall, error)
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
// stop must return in bounded time.
type tonePlayer interface {
	Start(sink string, ear hearing.Ear, freq int, levelDB float64) (stop func() error, err error)
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
	pingEvery time.Duration
}

type hearingRunOpts struct {
	soundDB    [hearing.Bands]float64
	sinkVolume int
	sinkName   string
	apply, yes bool
	// deviceMode sends the (unverified) 0x45 test-mode frame even without
	// --apply --yes.
	deviceMode bool
	outFile    string
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

// activeStreamsOn lists the uncorked sink inputs playing into sink index idx
// (`pactl list sink-inputs`).
func activeStreamsOn(long, idx string) []string {
	var out []string
	for _, blk := range strings.Split(long, "Sink Input #")[1:] {
		id, _, _ := strings.Cut(blk, "\n")
		sink, corked, app := "", "", ""
		for _, ln := range strings.Split(blk, "\n") {
			t := strings.TrimSpace(ln)
			switch {
			case strings.HasPrefix(t, "Sink: "):
				sink = strings.TrimPrefix(t, "Sink: ")
			case strings.HasPrefix(t, "Corked: "):
				corked = strings.TrimPrefix(t, "Corked: ")
			case strings.HasPrefix(t, "application.name = "):
				app = strings.Trim(strings.TrimPrefix(t, "application.name = "), `"`)
			}
		}
		if sink == idx && corked != "yes" {
			out = append(out, strings.TrimSpace("#"+strings.TrimSpace(id)+" "+app))
		}
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

type heardRec struct {
	Tone  hearing.Tone `json:"tone"`
	Heard bool         `json:"heard"`
}

type hearingRunOut struct {
	Timestamp  string              `json:"timestamp"`
	Sink       string              `json:"sink"`
	SinkVolume int                 `json:"sinkVolume"`
	DeviceMode bool                `json:"deviceMode"`
	SoundDB    []float64           `json:"soundDb"`
	Answers    []heardRec          `json:"answers"`
	Left       []int               `json:"left"`
	Right      []int               `json:"right"`
	Result     resultOut           `json:"result"`
	Applied    bool                `json:"applied"`
	Backup     string              `json:"backup,omitempty"`
	Restore    string              `json:"restore,omitempty"`
	Writes     []*hearingCall      `json:"writes,omitempty"`
	Final      *qcycore.EqSnapshot `json:"finalEq,omitempty"`
}

type hearingBackup struct {
	Timestamp string                 `json:"timestamp"`
	Status    map[string]interface{} `json:"status"`
	Eq        *qcycore.EqSnapshot    `json:"eq"`
	Library   string                 `json:"libraryId"`
	Restore   string                 `json:"restore"`
}

// hearingRunLive runs the whole test. Cleanup runs on every return path.
func hearingRunLive(env hearingEnv, o hearingRunOpts) (err error) {
	out := env.out
	if env.sleep == nil {
		env.sleep = time.Sleep
	}
	warnf := func(f string, a ...interface{}) { fmt.Fprintf(out, "warning: "+f+"\n", a...) }

	// One permanent watcher turns any signal into an abort that every phase
	// observes (checkAbort, waitKey).
	var (
		abortOnce sync.Once
		abortMu   sync.Mutex
		abortErr  error
		abortCh   = make(chan struct{})
		watchDone = make(chan struct{})
	)
	abort := func(e error) {
		abortOnce.Do(func() {
			abortMu.Lock()
			abortErr = e
			abortMu.Unlock()
			close(abortCh)
		})
	}
	checkAbort := func() error {
		select {
		case <-abortCh:
			abortMu.Lock()
			defer abortMu.Unlock()
			return abortErr
		default:
			return nil
		}
	}
	go func() {
		select {
		case <-env.sigs:
			abort(errInterrupted)
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	link, err := env.link()
	if err != nil {
		return fmt.Errorf("hearing run needs the qcyctld daemon: %w", err)
	}
	defer link.Close()

	// 1. Read-only checks first: nothing audible or persistent has happened.
	st, err := link.status()
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if m, _ := st["model"].(map[string]interface{}); m == nil || m["verified"] != true {
		return errors.New("hearing run is only for a verified model (this earbud is not)")
	}
	var eqBefore *qcycore.EqSnapshot
	if o.apply && o.yes {
		// The active curve, raw, before anything (test mode included) can
		// change it: it is what the backup and the library entry hold.
		if eqBefore, err = link.eqRead(); err != nil {
			return fmt.Errorf("backup: cannot read the active EQ, nothing was done: %w", err)
		}
	}
	if !env.have("pactl") || !env.have("pw-cat") {
		return errors.New("hearing run needs pactl and pw-cat (PipeWire) in PATH")
	}
	mp, err := mprisFor(env)
	if err != nil {
		return err
	}
	listing, err := env.exec("pactl", "list", "short", "sinks")
	if err != nil {
		return fmt.Errorf("pactl list short sinks: %w", err)
	}
	sink, err := pickSink(parseBluezSinks(listing), o.sinkName, env.cachedMAC())
	if err != nil {
		return err
	}
	sinkIdx := ""
	for _, s := range parseSinks(listing) {
		if s.Name == sink {
			sinkIdx = s.Index
		}
	}
	long, err := env.exec("pactl", "list", "sinks")
	if err != nil {
		return fmt.Errorf("pactl list sinks: %w", err)
	}
	props := parseSinkProps(long, sink)
	addr := props["api.bluez5.address"]
	if addr == "" {
		return fmt.Errorf("sink %s is not identified as a Bluetooth device (no api.bluez5.address in pactl list sinks)", sink)
	}
	if o.sinkName == "" && normMAC(addr) != normMAC(env.cachedMAC()) {
		return fmt.Errorf("sink %s reports address %s, not the earbuds' %s", sink, addr, env.cachedMAC())
	}
	fmt.Fprintf(out, "sink: %s  (%s, address %s)  volume %d%%\n", sink, props["Description"], addr, o.sinkVolume)
	volOut, err := env.exec("pactl", "get-sink-volume", sink)
	if err != nil {
		return fmt.Errorf("pactl get-sink-volume: %w", err)
	}
	vols, err := parseSinkVolume(volOut)
	if err != nil {
		return err
	}
	muteOut, err := env.exec("pactl", "get-sink-mute", sink)
	if err != nil {
		return fmt.Errorf("pactl get-sink-mute: %w", err)
	}
	muted, err := parseMute(muteOut)
	if err != nil {
		return err
	}
	if e := checkAbort(); e != nil {
		return e
	}
	keys, restoreTTY, err := env.keys()
	if err != nil {
		return err
	}
	defer restoreTTY()

	// 2. Side effects, each with its undo registered before it is attempted.
	var undo []func()
	defer func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()

	var stopTone func() error
	endTone := func() {
		if stopTone != nil {
			if e := stopTone(); e != nil {
				warnf("tone: %v", e)
			}
			stopTone = nil
		}
	}
	undo = append(undo, endTone)

	// Music: pause what is Playing (resume only that), confirm it is paused,
	// then refuse to go on while any stream still plays into the sink.
	var paused []string
	undo = append(undo, func() {
		for _, p := range paused {
			if e := mp.play(p); e != nil {
				warnf("could not resume %s: %v", p, e)
			}
		}
	})
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
	env.sleep(200 * time.Millisecond)
	inputs, err := env.exec("pactl", "list", "sink-inputs")
	if err != nil {
		return fmt.Errorf("pactl list sink-inputs: %w", err)
	}
	if act := activeStreamsOn(inputs, sinkIdx); len(act) > 0 {
		return fmt.Errorf("audio is still playing into %s (%s): stop it and run again; the volume was not raised", sink, strings.Join(act, ", "))
	}
	if e := checkAbort(); e != nil {
		return e
	}

	// Sink volume and mute.
	volChanged := false
	undo = append(undo, func() {
		if !volChanged {
			return
		}
		if _, e := env.exec("pactl", append([]string{"set-sink-volume", sink}, vols...)...); e != nil {
			warnf("could not restore the sink volume (was %v): %v", vols, e)
		}
		m := "0"
		if muted {
			m = "1"
		}
		if _, e := env.exec("pactl", "set-sink-mute", sink, m); e != nil {
			warnf("could not restore the sink mute: %v", e)
		}
	})
	volChanged = true
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
			warnf("could not switch the test mode off (%v); run: qcyctl hearing mode off --yes", e)
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

	// 3. The staircase.
	keyCh := make(chan byte, 16)
	keyErr := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				keyErr <- fmt.Errorf("key reader panicked: %v", r)
			}
		}()
		for {
			k, e := keys.ReadKey()
			if e != nil {
				keyErr <- e
				return
			}
			keyCh <- k
		}
	}()
	waitKey := func() (byte, error) {
		select {
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

	fmt.Fprintln(out, "keys: h/Enter = I hear it   n/space = I do not   r = repeat   q = abort")
	sess := hearing.NewSession(o.soundDB)
	var answers []heardRec
	for !sess.Done() {
		t := sess.Current()
		fmt.Fprintf(out, "[%2d] %-5s ear  band %d/%d  %5d Hz  step %+d  %6.1f dBFS ... ", len(answers)+1, t.Ear, t.Band+1, hearing.Bands, t.Freq, t.Gain, t.LevelDB)
		drainKeys()
		answered := false
		for !answered {
			if e := checkAbort(); e != nil {
				fmt.Fprintln(out)
				return e
			}
			if e := sinkAlive(); e != nil {
				fmt.Fprintln(out)
				return e
			}
			stop, e := env.player.Start(sink, t.Ear, t.Freq, t.LevelDB)
			if e != nil {
				return fmt.Errorf("play tone: %w", e)
			}
			stopTone = stop
			var key byte
		keyLoop:
			for {
				k, e := waitKey()
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
			endTone()
			switch key {
			case 'q', 'Q':
				fmt.Fprintln(out, "aborted")
				return errors.New("aborted by the listener (q); nothing was written")
			case 'r', 'R':
				fmt.Fprint(out, "repeat ... ")
			case 'h', 'H', '\r', '\n':
				fmt.Fprintln(out, "heard")
				answers = append(answers, heardRec{Tone: t, Heard: true})
				if e := sess.Answer(true); e != nil {
					return e
				}
				answered = true
			default:
				fmt.Fprintln(out, "not heard")
				answers = append(answers, heardRec{Tone: t, Heard: false})
				if e := sess.Answer(false); e != nil {
					return e
				}
				answered = true
			}
		}
	}

	// 4. Result.
	l, r := sess.Result()
	var gl, gr [hearing.Bands]float64
	for i := range l {
		gl[i], gr[i] = float64(l[i]), float64(r[i])
	}
	res := computeResult(gl, gr)
	fmt.Fprintf(out, "\nleft : %v\nright: %v\n", l, r)
	printResultText(out, res)
	rec := hearingRunOut{
		Timestamp: env.now().Format(time.RFC3339), Sink: sink, SinkVolume: o.sinkVolume, DeviceMode: useMode,
		SoundDB: o.soundDB[:], Answers: answers, Left: l[:], Right: r[:], Result: res,
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
		cfg, e := env.configDir()
		if e != nil {
			return fmt.Errorf("backup: %w", e)
		}
		stamp := env.now().Format("20060102-150405")
		libID, e := env.saveCurve("backup antes do teste auditivo "+stamp, eqBefore)
		if e != nil {
			return fmt.Errorf("backup: library entry failed, nothing was written to the earbuds: %w", e)
		}
		restore := "qcyctl library apply " + libID + " --yes"
		dir := filepath.Join(cfg, "backups")
		if e := os.MkdirAll(dir, 0o700); e != nil {
			return fmt.Errorf("backup: %w", e)
		}
		bk := filepath.Join(dir, "hearing-before-"+stamp+".json")
		if e := writeJSONFile(bk, hearingBackup{Timestamp: stamp, Status: st, Eq: eqBefore, Library: libID, Restore: restore}); e != nil {
			return fmt.Errorf("backup failed, nothing was written to the earbuds: %w", e)
		}
		rec.Backup, rec.Restore = bk, restore
		fmt.Fprintf(out, "backup: %s\nlibrary entry: %s (slot %d)\n", bk, libID, eqBefore.Slot)

		var li, ri [hearing.Bands]int = l, r
		wrote := 0
		for _, w := range []struct {
			ear         hearing.Ear
			gains, peer [hearing.Bands]int
		}{{hearing.Left, li, ri}, {hearing.Right, ri, li}} {
			c, e := link.hearingWrite(w.ear, w.gains, w.peer)
			if e != nil {
				_ = save()
				state := "no ear was confirmed written"
				if wrote > 0 {
					state = "MIXED STATE: the left ear was written and the right was not"
				}
				fmt.Fprintf(out, "ERROR writing the %s ear: %v\n%s; to restore the previous state: %s\n", w.ear, e, state, restore)
				return fmt.Errorf("write %s ear: %w", w.ear, e)
			}
			wrote++
			rec.Writes = append(rec.Writes, c)
			fmt.Fprintf(out, "wrote %-5s sent %s  ack=%v", w.ear, c.Sent, c.Ack)
			if c.Readback != nil {
				fmt.Fprintf(out, "  device: slot %d %q masterGain %.2f dB", c.Readback.Slot, c.Readback.Effect, c.Readback.MasterGain)
			} else if c.ReadbackError != "" {
				fmt.Fprintf(out, "  readback failed: %s", c.ReadbackError)
			}
			fmt.Fprintln(out)
		}
		rec.Applied = true
		modeOff()
		if snap, e := link.eqRead(); e != nil {
			warnf("final read: %v", e)
		} else {
			rec.Final = snap
			fmt.Fprintf(out, "device now reports: slot %d %q masterGain %.2f dB, %d bands\n", snap.Slot, snap.Effect, snap.MasterGain, len(snap.Bands))
		}
		fmt.Fprintf(out, "to restore the previous state: %s\n", restore)
	case o.apply:
		fmt.Fprintln(out, "--apply needs --yes: nothing was written to the earbuds.")
	default:
		fmt.Fprintln(out, "nothing was written to the earbuds (use --apply --yes to write).")
	}
	return save()
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

func (l *liveHearingLink) hearingMode(on bool) (*hearingCall, error) {
	return l.call("hearingMode", map[string]interface{}{"on": on, "confirm": true})
}

func (l *liveHearingLink) hearingWrite(ear hearing.Ear, gains, other [hearing.Bands]int) (*hearingCall, error) {
	return l.call("hearingWrite", map[string]interface{}{
		"ear": ear.String(), "gains": gains[:], "otherGains": other[:], "confirm": true,
	})
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

func (p pwcatPlayer) Start(sink string, ear hearing.Ear, freq int, levelDB float64) (func() error, error) {
	bin := p.bin
	if bin == "" {
		bin = "pw-cat"
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	shrinkPipe(pw)
	cmd := exec.Command(bin, "--playback", "--target", sink,
		"-P", "node.dont-fallback=true node.dont-reconnect=true",
		"--format", "s16", "--rate", "44100", "--channels", "2", "-")
	cmd.Stdin = pr
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	pr.Close()
	synth := hearing.NewSynth(ear, freq, levelDB)
	ramp := hearing.RampSamples()
	quit := make(chan struct{})
	done := make(chan error, 1)
	write := func(b []byte) error {
		_ = pw.SetWriteDeadline(time.Now().Add(toneWriteDeadline))
		_, e := pw.Write(b)
		return e
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("tone writer panicked: %v", r)
			}
		}()
		if e := write(synth.Next(ramp, hearing.FadeIn)); e != nil {
			done <- e
			return
		}
		for {
			select {
			case <-quit:
				done <- write(synth.Next(ramp, hearing.FadeOut))
				return
			default:
			}
			if e := write(synth.Next(ramp, hearing.Steady)); e != nil {
				done <- e
				return
			}
		}
	}()
	return func() error {
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
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
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
	}, nil
}

// guardedPlayer refuses to play while hardware is disabled (tests).
type guardedPlayer struct{ tonePlayer }

func (g guardedPlayer) Start(sink string, ear hearing.Ear, freq int, level float64) (func() error, error) {
	if err := refuseHardware(); err != nil {
		return nil, err
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
		now:       time.Now,
		sleep:     time.Sleep,
		configDir: qcyConfigDir,
		saveCurve: saveBackupCurve,
		pingEvery: 3 * time.Second,
	}, func() { signal.Stop(sigs) }
}
