package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/bbztp/qcy-omarchy/backend/internal/hearing"
)

// `qcyctl hearing ...`: the app's hearing test. plan, result and run --dry-run
// are offline (tones, simulated answers, curve, master gain, frame bytes);
// the real `run` lives in hearing_run.go.

const hearingUsage = `usage: qcyctl hearing <subcommand>
  hearing plan [--sound-db a,b,..8] [--json]                         (tones of the first step of every band)
  hearing result --left g1,..,g8 --right g1,..,g8 [--json]            (master gain, curves and the write frames as hex; nothing is sent)
  hearing run --dry-run --answers "h,n,h,..." [--sound-db ..] [--json] (simulates the staircase from answers: h = heard, n = not heard; no audio)
  hearing run [--sink NAME] [--sink-volume 100%] [--sound-db ..] [--device-mode] [--apply --yes] [--out file.json]
                                                                      (the real test: pauses the music (playerctl or busctl; aborts if anything still plays
                                                                       into the sink), plays the tones on the earbuds' Bluetooth sink; keys h/Enter = heard,
                                                                       n/space = not heard, r = repeat, q = abort. The device test mode (frame 0x45, NOT
                                                                       verified on hardware) is only sent with --device-mode or --apply --yes; the volume,
                                                                       the music and the mode are always restored. Nothing is written without --apply --yes)
  hearing mode on|off --yes                                           (sends the 0x45 test-mode frame, NOT verified on hardware; for recovery)`

func cmdHearing(args []string) error { return hearingMain(args, os.Stdout) }

func hearingMain(args []string, out io.Writer) error {
	if len(args) < 1 {
		return errors.New(hearingUsage)
	}
	switch args[0] {
	case "plan":
		return hearingPlan(args[1:], out)
	case "result":
		return hearingResult(args[1:], out)
	case "run":
		return hearingRunCmd(args[1:], out)
	case "mode":
		return hearingModeCmd(args[1:], out, dialHearingLink)
	}
	return fmt.Errorf("hearing: unknown subcommand %q\n%s", args[0], hearingUsage)
}

func hearingFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("hearing "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseBands parses n comma-separated numbers.
func parseBands(s, what string) ([hearing.Bands]float64, error) {
	var v [hearing.Bands]float64
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) != hearing.Bands {
		return v, fmt.Errorf("%s: need %d comma-separated values, got %d", what, hearing.Bands, len(parts))
	}
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || f != f || f > 1e6 || f < -1e6 {
			return v, fmt.Errorf("%s: invalid value %q", what, p)
		}
		v[i] = f
	}
	return v, nil
}

func soundDBFlag(s string) ([hearing.Bands]float64, error) {
	if s == "" {
		return hearing.DefaultSoundDB, nil
	}
	return parseBands(s, "--sound-db")
}

func hearingJSON(out io.Writer, v interface{}) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func hearingPlan(args []string, out io.Writer) error {
	fs := hearingFlags("plan")
	jsonOut := fs.Bool("json", false, "")
	sdb := fs.String("sound-db", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: qcyctl hearing plan [--sound-db a,b,..8] [--json]")
	}
	db, err := soundDBFlag(*sdb)
	if err != nil {
		return err
	}
	var tones []hearing.Tone
	for _, ear := range []hearing.Ear{hearing.Left, hearing.Right} {
		for b, f := range hearing.Freqs {
			tones = append(tones, hearing.Tone{Ear: ear, Band: b, Freq: f, Gain: hearing.InitialStep,
				LevelDB: hearing.ToneLevel(db[b], f, hearing.InitialStep)})
		}
	}
	if *jsonOut {
		return hearingJSON(out, map[string]interface{}{"soundDb": db, "initialStep": hearing.InitialStep, "tones": tones})
	}
	fmt.Fprintf(out, "order: left ear first, then right; %d bands each, start at step +%d\n", hearing.Bands, hearing.InitialStep)
	for _, t := range tones {
		fmt.Fprintf(out, "%-5s %5d Hz  step %+d  %7.1f dBFS\n", t.Ear, t.Freq, t.Gain, t.LevelDB)
	}
	return nil
}

type earResult struct {
	Gains []float64 `json:"gains"`
	Frame string    `json:"frame"`
}

type resultOut struct {
	MasterGain float64 `json:"masterGain"`
	// MasterClamped: the rule gave MasterRaw, outside +-12 dB, and MasterGain
	// is that value limited.
	MasterClamped bool                            `json:"masterClamped,omitempty"`
	MasterRaw     float64                         `json:"masterRaw,omitempty"`
	Left          earResult                       `json:"left"`
	Right         earResult                       `json:"right"`
	Flag          map[string]string               `json:"flag"`
	Curves        map[string][]hearing.CurvePoint `json:"curves,omitempty"`
	Sent          bool                            `json:"sent"`
}

// computeResult derives the master gain and the frames from both ears' gains.
// Master gain uses the left and right curves separately and takes the lower
// (more attenuated) one, so neither ear exceeds the volume rule. (The app's
// exact combination is not known; this is an assumption.)
func computeResult(left, right [hearing.Bands]float64) resultOut {
	for i := range left {
		left[i] = hearing.ClampGain(left[i])
		right[i] = hearing.ClampGain(right[i])
	}
	mg := hearing.MasterGain(left)
	if r := hearing.MasterGain(right); r < mg {
		mg = r
	}
	raw := mg
	mg, clamped := hearing.ClampMaster(mg)
	enc := func(g [hearing.Bands]float64, e hearing.Ear) earResult {
		return earResult{Gains: g[:], Frame: hex.EncodeToString(hearing.WriteFrame(e, g, mg))}
	}
	ro := resultOut{
		MasterGain: mg,
		Left:       enc(left, hearing.Left),
		Right:      enc(right, hearing.Right),
		Flag: map[string]string{
			"on":  hex.EncodeToString(hearing.FlagFrame(true)),
			"off": hex.EncodeToString(hearing.FlagFrame(false)),
		},
		Curves: map[string][]hearing.CurvePoint{"left": decimate(hearing.Curve(left)), "right": decimate(hearing.Curve(right))},
	}
	if clamped {
		ro.MasterClamped, ro.MasterRaw = true, raw
	}
	return ro
}

// decimate keeps the curve at the band frequencies and a coarse grid so the
// printed output stays readable.
func decimate(c []hearing.CurvePoint) []hearing.CurvePoint {
	var out []hearing.CurvePoint
	for i, p := range c {
		if i%40 == 0 { // every 200 Hz
			out = append(out, p)
		}
	}
	return out
}

func hearingResult(args []string, out io.Writer) error {
	fs := hearingFlags("result")
	jsonOut := fs.Bool("json", false, "")
	l := fs.String("left", "", "")
	r := fs.String("right", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *l == "" || *r == "" {
		return errors.New("usage: qcyctl hearing result --left g1,..,g8 --right g1,..,g8 [--json]")
	}
	lg, err := parseBands(*l, "--left")
	if err != nil {
		return err
	}
	rg, err := parseBands(*r, "--right")
	if err != nil {
		return err
	}
	res := computeResult(lg, rg)
	if *jsonOut {
		return hearingJSON(out, res)
	}
	printResultText(out, res)
	fmt.Fprintln(out, "nothing was sent to the earbuds (not verified on hardware).")
	return nil
}

type dryStep struct {
	Tone  hearing.Tone `json:"tone"`
	Heard bool         `json:"heard"`
}

func hearingRunCmd(args []string, out io.Writer) error {
	fs := hearingFlags("run")
	jsonOut := fs.Bool("json", false, "")
	dry := fs.Bool("dry-run", false, "")
	answers := fs.String("answers", "", "")
	sdb := fs.String("sound-db", "", "")
	apply := fs.Bool("apply", false, "")
	yes := fs.Bool("yes", false, "")
	outFile := fs.String("out", "", "")
	sinkName := fs.String("sink", "", "")
	sinkVol := fs.String("sink-volume", "100%", "")
	devMode := fs.Bool("device-mode", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: qcyctl hearing run [--dry-run --answers \"h,n,h,...\"] [--sound-db ..] [--json] [--sink NAME] [--sink-volume 100%] [--device-mode] [--apply --yes] [--out file.json]")
	}
	db, err := soundDBFlag(*sdb)
	if err != nil {
		return err
	}
	if !*dry {
		if *answers != "" || *jsonOut {
			return errors.New("hearing run: --answers and --json belong to --dry-run (the real test is answered on the keyboard; use --out for the result)")
		}
		pct, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(*sinkVol), "%"))
		if err != nil || pct < 1 || pct > 100 {
			return fmt.Errorf("--sink-volume: %q must be 1%%..100%%", *sinkVol)
		}
		env, done := liveHearingEnv(out)
		defer done()
		return hearingRunLive(env, hearingRunOpts{soundDB: db, sinkVolume: pct, sinkName: *sinkName,
			apply: *apply, yes: *yes, deviceMode: *devMode, outFile: *outFile})
	}
	var seq []bool
	for _, a := range strings.Split(*answers, ",") {
		switch strings.ToLower(strings.TrimSpace(a)) {
		case "h", "y", "1":
			seq = append(seq, true)
		case "n", "0":
			seq = append(seq, false)
		case "":
		default:
			return fmt.Errorf("--answers: invalid answer %q (h = heard, n = not heard)", a)
		}
	}
	s := hearing.NewSession(db)
	var steps []dryStep
	for _, h := range seq {
		if s.Done() {
			return fmt.Errorf("--answers: %d answers given but the test ended after %d", len(seq), len(steps))
		}
		steps = append(steps, dryStep{Tone: s.Current(), Heard: h})
		if err := s.Answer(h); err != nil {
			return err
		}
	}
	l, r := s.Result()
	complete := s.Done()
	var gl, gr [hearing.Bands]float64
	for i := range l {
		gl[i], gr[i] = float64(l[i]), float64(r[i])
	}
	if *jsonOut {
		o := map[string]interface{}{"steps": steps, "complete": complete}
		if complete {
			o["left"], o["right"] = l, r
			o["result"] = computeResult(gl, gr)
		} else {
			o["next"] = s.Current()
		}
		return hearingJSON(out, o)
	}
	for i, st := range steps {
		ans := "not heard"
		if st.Heard {
			ans = "heard"
		}
		fmt.Fprintf(out, "%3d %-5s %5d Hz step %+d %7.1f dBFS -> %s\n", i+1, st.Tone.Ear, st.Tone.Freq, st.Tone.Gain, st.Tone.LevelDB, ans)
	}
	if !complete {
		t := s.Current()
		fmt.Fprintf(out, "incomplete after %d answers; next: %s %d Hz step %+d %.1f dBFS\n", len(steps), t.Ear, t.Freq, t.Gain, t.LevelDB)
		return nil
	}
	fmt.Fprintf(out, "left : %v\nright: %v\n", l, r)
	res := computeResult(gl, gr)
	fmt.Fprintf(out, "master gain: %.2f dB\nleft frame:  %s\nright frame: %s\n", res.MasterGain, res.Left.Frame, res.Right.Frame)
	return nil
}

// printResultText prints master gain, frames, flag frames and the curves.
func printResultText(out io.Writer, res resultOut) {
	fmt.Fprintf(out, "master gain: %.2f dB\n", res.MasterGain)
	if res.MasterClamped {
		fmt.Fprintf(out, "WARNING: the app's rule gave %.2f dB; limited to +-%.0f dB (the range PROTOCOL.md records as accepted).\n", res.MasterRaw, hearing.MasterLimit)
	}
	fmt.Fprintf(out, "left  gains: %v\nleft  frame: %s\n", res.Left.Gains, res.Left.Frame)
	fmt.Fprintf(out, "right gains: %v\nright frame: %s\n", res.Right.Gains, res.Right.Frame)
	fmt.Fprintf(out, "flag on:  %s\nflag off: %s\n", res.Flag["on"], res.Flag["off"])
	fmt.Fprintln(out, "curve (summed peaking response, dB, every 200 Hz):")
	for i := range res.Curves["left"] {
		lp, rp := res.Curves["left"][i], res.Curves["right"][i]
		fmt.Fprintf(out, "  %6.0f Hz  L %+6.2f  R %+6.2f\n", lp.Freq, lp.DB, rp.DB)
	}
}

// hearingModeCmd: `hearing mode on|off --yes`. Frame 0x45 was read from the
// app and is NOT verified on hardware, hence the explicit --yes.
func hearingModeCmd(args []string, out io.Writer, dial func() (hearingLink, error)) error {
	const usage = "usage: qcyctl hearing mode on|off --yes"
	fs := hearingFlags("mode")
	yes := fs.Bool("yes", false, "")
	var word string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		word, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || (word != "on" && word != "off") {
		return errors.New(usage)
	}
	if !*yes {
		return errors.New(usage + " (sends the 0x45 frame, not verified on hardware)")
	}
	link, err := dial()
	if err != nil {
		return fmt.Errorf("hearing mode needs the qcyctld daemon: %w", err)
	}
	defer link.Close()
	c, err := link.hearingMode(word == "on")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sent %s  ack=%v", c.Sent, c.Ack)
	if c.Readback != nil {
		fmt.Fprintf(out, "  device: slot %d %q masterGain %.2f dB", c.Readback.Slot, c.Readback.Effect, c.Readback.MasterGain)
	}
	fmt.Fprintln(out)
	return nil
}
