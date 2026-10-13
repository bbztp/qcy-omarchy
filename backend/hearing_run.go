package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bbztp/qcy-omarchy/backend/internal/hearing"
	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
)

// `qcyctl hearing run` (phase 2): the real test. Everything that touches the
// outside world sits behind hearingEnv so the tests drive it with fakes.
//
// Guarantees, whatever happens after the first side effect (error, abort key,
// SIGINT/SIGTERM): the tone is stopped, the test mode is switched off, the
// sink volume and mute are put back and the players this run paused are
// resumed. Nothing is written to the earbuds without --apply --yes.

const hearingRestoreCmd = "qcyctl library apply backup-antes-do-teste-auditivo --yes"

// ---------- seams ----------

// hearingLink is the daemon side: only the daemon speaks the hearing commands.
type hearingLink interface {
	status() (map[string]interface{}, error)
	hearingMode(on bool) (*hearingCall, error)
	hearingWrite(ear hearing.Ear, gains, other [hearing.Bands]int) (*hearingCall, error)
	eqRead() (*qcycore.EqSnapshot, error)
	// ping is a harmless read that keeps the daemon's idle window from
	// dropping the BLE link during a long test.
	ping()
	Close() error
}

type hearingCall struct {
	Sent       string   `json:"sent"`
	Ack        bool     `json:"ack"`
	AckPayload string   `json:"ackPayload,omitempty"`
	MasterGain *float64 `json:"masterGain,omitempty"`
	Readback   *struct {
		Slot       int     `json:"slot"`
		Effect     string  `json:"effect,omitempty"`
		MasterGain float64 `json:"masterGain"`
		Payload    string  `json:"payload"`
	} `json:"readback,omitempty"`
	ReadbackError string `json:"readbackError,omitempty"`
}

// tonePlayer plays one continuous tone until the returned stop is called.
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
	configDir func() (string, error)
	pingEvery time.Duration
}

type hearingRunOpts struct {
	soundDB    [hearing.Bands]float64
	sinkVolume int
	sinkName   string
	apply, yes bool
	outFile    string
}

// ---------- parsing ----------

// parseBluezSinks returns the Bluetooth sink names from `pactl list short sinks`.
func parseBluezSinks(listing string) []string {
	var out []string
	for _, ln := range strings.Split(listing, "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && (strings.HasPrefix(f[1], "bluez_output.") || strings.HasPrefix(f[1], "bluez_sink.")) {
			out = append(out, f[1])
		}
	}
	return out
}

// pickSink chooses the earbuds' sink: an explicit name, else the one whose
// name carries the cached MAC (":" -> "_"), else the only Bluetooth sink.
// warn is non-empty when the choice was not confirmed by the MAC.
func pickSink(sinks []string, explicit, mac string) (sink, warn string, err error) {
	if explicit != "" {
		for _, s := range sinks {
			if s == explicit {
				return s, "", nil
			}
		}
		return "", "", fmt.Errorf("sink %q is not a Bluetooth sink (found: %s)", explicit, strings.Join(sinks, ", "))
	}
	if len(sinks) == 0 {
		return "", "", errors.New("no Bluetooth sink found (pactl list short sinks): connect the earbuds' audio first")
	}
	if mac = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(mac), ":", "_")); mac != "" {
		for _, s := range sinks {
			if strings.Contains(strings.ToUpper(s), mac) {
				return s, "", nil
			}
		}
	}
	if len(sinks) == 1 {
		return sinks[0], fmt.Sprintf("the sink was not matched by the earbuds' MAC (%q); it is the only Bluetooth sink, so it is assumed to be them (use --sink to force another)", mac), nil
	}
	return "", "", fmt.Errorf("several Bluetooth sinks and none matches the earbuds' MAC %q: %s (choose with --sink)", mac, strings.Join(sinks, ", "))
}

var pctRe = regexp.MustCompile(`(\d+)%`)

// parseSinkVolume reads the per-channel percentages of `pactl get-sink-volume`.
func parseSinkVolume(s string) ([]int, error) {
	for _, ln := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(ln), "Volume:") {
			continue
		}
		var v []int
		for _, m := range pctRe.FindAllStringSubmatch(ln, -1) {
			n, _ := strconv.Atoi(m[1])
			v = append(v, n)
		}
		if len(v) > 0 {
			return v, nil
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

func pctArgs(v []int) []string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = strconv.Itoa(n) + "%"
	}
	return out
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
	SoundDB    []float64           `json:"soundDb"`
	Answers    []heardRec          `json:"answers"`
	Left       []int               `json:"left"`
	Right      []int               `json:"right"`
	Result     resultOut           `json:"result"`
	Applied    bool                `json:"applied"`
	Backup     string              `json:"backup,omitempty"`
	Writes     []*hearingCall      `json:"writes,omitempty"`
	Final      *qcycore.EqSnapshot `json:"finalEq,omitempty"`
}

// hearingRunLive runs the whole test. Cleanup runs on every return path.
func hearingRunLive(env hearingEnv, o hearingRunOpts) (err error) {
	out := env.out
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
	if !env.have("pactl") || !env.have("pw-cat") {
		return errors.New("hearing run needs pactl and pw-cat (PipeWire) in PATH")
	}
	listing, err := env.exec("pactl", "list", "short", "sinks")
	if err != nil {
		return fmt.Errorf("pactl list short sinks: %w", err)
	}
	sink, warn, err := pickSink(parseBluezSinks(listing), o.sinkName, env.cachedMAC())
	if err != nil {
		return err
	}
	if warn != "" {
		fmt.Fprintln(out, "warning:", warn)
	}
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
	warnf := func(f string, a ...interface{}) { fmt.Fprintf(out, "warning: "+f+"\n", a...) }

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

	// Music: pause only what is Playing, resume only that.
	var paused []string
	undo = append(undo, func() {
		for _, p := range paused {
			if _, e := env.exec("playerctl", "--player="+p, "play"); e != nil {
				warnf("could not resume %s: %v", p, e)
			}
		}
	})
	if env.have("playerctl") {
		names, e := env.exec("playerctl", "-l")
		if e != nil {
			names = ""
		}
		for _, p := range strings.Fields(names) {
			s, e := env.exec("playerctl", "--player="+p, "status")
			if e != nil || strings.TrimSpace(s) != "Playing" {
				continue
			}
			if _, e := env.exec("playerctl", "--player="+p, "pause"); e != nil {
				warnf("could not pause %s: %v", p, e)
				continue
			}
			paused = append(paused, p)
		}
		if len(paused) > 0 {
			fmt.Fprintf(out, "paused: %s (resumed at the end)\n", strings.Join(paused, ", "))
		}
	} else {
		fmt.Fprintln(out, "warning: playerctl not found: pause any music manually before starting.")
	}

	// Sink volume and mute.
	volChanged := false
	undo = append(undo, func() {
		if !volChanged {
			return
		}
		if _, e := env.exec("pactl", append([]string{"set-sink-volume", sink}, pctArgs(vols)...)...); e != nil {
			warnf("could not restore the sink volume (was %v): %v", pctArgs(vols), e)
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

	// Test mode: always switched off again, even when switching it on failed
	// half way (the frame may have reached the earbuds).
	modeOn := false
	modeOff := func() {
		if !modeOn {
			return
		}
		modeOn = false
		if _, e := link.hearingMode(false); e != nil {
			warnf("could not switch the test mode off (%v); run: qcyctl hearing result ... or reconnect the earbuds", e)
		}
	}
	undo = append(undo, modeOff)
	modeOn = true
	if _, e := link.hearingMode(true); e != nil {
		return fmt.Errorf("hearing mode on: %w", e)
	}

	if env.pingEvery > 0 {
		stopPing := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		case <-env.sigs:
			return 0, errInterrupted
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

	fmt.Fprintf(out, "sink: %s  volume %d%%\n", sink, o.sinkVolume)
	fmt.Fprintln(out, "keys: h/Enter = I hear it   n/space = I do not   r = repeat   q = abort")
	sess := hearing.NewSession(o.soundDB)
	var answers []heardRec
	for !sess.Done() {
		t := sess.Current()
		bandN := t.Band + 1
		fmt.Fprintf(out, "[%2d] %-5s ear  band %d/%d  %5d Hz  step %+d  %6.1f dBFS ... ", len(answers)+1, t.Ear, bandN, hearing.Bands, t.Freq, t.Gain, t.LevelDB)
		drainKeys()
		answered := false
		for !answered {
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
		Timestamp: env.now().Format(time.RFC3339), Sink: sink, SinkVolume: o.sinkVolume,
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
		cfg, e := env.configDir()
		if e != nil {
			return fmt.Errorf("backup: %w", e)
		}
		dir := filepath.Join(cfg, "backups")
		if e := os.MkdirAll(dir, 0o700); e != nil {
			return fmt.Errorf("backup: %w", e)
		}
		bk := filepath.Join(dir, "hearing-before-"+env.now().Format("20060102-150405")+".json")
		if e := writeJSONFile(bk, st); e != nil {
			return fmt.Errorf("backup failed, nothing was written to the earbuds: %w", e)
		}
		rec.Backup = bk
		fmt.Fprintf(out, "backup of the status before the test: %s\n", bk)
		endTone()
		var li, ri [hearing.Bands]int = l, r
		for _, w := range []struct {
			ear         hearing.Ear
			gains, peer [hearing.Bands]int
		}{{hearing.Left, li, ri}, {hearing.Right, ri, li}} {
			c, e := link.hearingWrite(w.ear, w.gains, w.peer)
			if e != nil {
				_ = save()
				return fmt.Errorf("write %s ear: %w", w.ear, e)
			}
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
		fmt.Fprintf(out, "to restore the previous state: %s\n", hearingRestoreCmd)
		fmt.Fprintf(out, "(needs the library entry saved before the test: qcyctl library import-device \"backup antes do teste auditivo\"; the full status snapshot is %s)\n", bk)
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
	return l.call("hearingMode", map[string]interface{}{"on": on})
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

// pwcatPlayer plays through `pw-cat --playback` fed on stdin.
type pwcatPlayer struct{}

func (pwcatPlayer) Start(sink string, ear hearing.Ear, freq int, levelDB float64) (func() error, error) {
	if err := refuseHardware(); err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	shrinkPipe(pw)
	cmd := exec.Command("pw-cat", "--playback", "--target", sink, "--format", "s16", "--rate", "44100", "--channels", "2", "-")
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
	go func() {
		if _, e := pw.Write(synth.Next(ramp, hearing.FadeIn)); e != nil {
			done <- e
			return
		}
		for {
			select {
			case <-quit:
				_, e := pw.Write(synth.Next(ramp, hearing.FadeOut))
				done <- e
				return
			default:
			}
			if _, e := pw.Write(synth.Next(ramp, hearing.Steady)); e != nil {
				done <- e
				return
			}
		}
	}()
	return func() error {
		close(quit)
		werr := <-done
		pw.Close()
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		return werr
	}, nil
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

func liveHearingEnv(out io.Writer) (hearingEnv, func()) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	return hearingEnv{
		out: out,
		exec: func(name string, args ...string) (string, error) {
			if err := refuseHardware(); err != nil {
				return "", err
			}
			b, err := exec.Command(name, args...).Output()
			return string(b), err
		},
		have: func(name string) bool { _, err := exec.LookPath(name); return err == nil },
		link: func() (hearingLink, error) {
			dc, err := dialDaemon(2 * time.Second)
			if err != nil {
				return nil, err
			}
			return &liveHearingLink{dc: dc}, nil
		},
		player: pwcatPlayer{},
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
		configDir: qcyConfigDir,
		pingEvery: 3 * time.Second,
	}, func() { signal.Stop(sigs) }
}
