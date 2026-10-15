package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// `qcyctl codec` shows and switches the computer's Bluetooth audio codec for
// the earbuds through PipeWire (pw-dump to read, wpctl to switch; no sudo, no
// BLE, no daemon). The device is found by its classic Bluetooth address.
//
// Exit codes: 0 ok, 1 other error, 2 usage, 3 unavailable (no pw-dump or
// wpctl, PipeWire not running, the device is not in PipeWire), 4 failed (the
// switch was not applied or not confirmed).

// codecConfirmBudget is how long `codec set` waits for the switch to show.
const codecConfirmBudget = 3 * time.Second

const (
	exitUsage       = 2
	exitUnavailable = 3
	exitFailed      = 4
)

// exitError carries a stable process exit code through cmdX's error return.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func codecErr(code int, format string, a ...interface{}) error {
	return &exitError{code: code, err: fmt.Errorf(format, a...)}
}

// codecEnv is the seam to PipeWire: tests replace exec and sleep.
type codecEnv struct {
	exec  func(name string, args ...string) (string, error)
	have  func(name string) bool
	sleep func(time.Duration)
	now   func() time.Time
}

func liveCodecEnv() codecEnv {
	return codecEnv{
		exec:  liveExec,
		have:  func(name string) bool { _, err := exec.LookPath(name); return err == nil },
		sleep: time.Sleep,
		now:   time.Now,
	}
}

var codecEnvFn = liveCodecEnv

type codecEntry struct {
	Codec   string `json:"codec"`
	Profile string `json:"profile"`
	Index   int    `json:"index"`
	Active  bool   `json:"active"`
}

type codecActive struct {
	Codec string `json:"codec"`
	Kind  string `json:"kind"` // music | call | off
}

type codecState struct {
	Device int          `json:"device"`
	Active codecActive  `json:"active"`
	Music  []codecEntry `json:"music"`
}

// Profile names are not codec-stable ("a2dp-sink" is whichever codec has the
// highest priority), so the label comes from the name suffix when it has one,
// else from the description, and for the active profile from the sink node.
var codecSuffixLabels = map[string]string{
	"sbc": "SBC", "sbc_xq": "SBC-XQ", "aac": "AAC", "ldac": "LDAC", "aptx": "aptX",
	"aptx_hd": "aptX HD", "aptx_ll": "aptX LL", "aptx_ll_duplex": "aptX LL",
	"faststream": "FastStream", "faststream_duplex": "FastStream",
	"cvsd": "CVSD", "msbc": "mSBC", "lc3": "LC3",
}

var codecDescRe = regexp.MustCompile(`codec ([^)]+)\)`)

func codecSuffixLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if l, ok := codecSuffixLabels[s]; ok {
		return l
	}
	switch {
	case strings.HasPrefix(s, "opus"):
		return "Opus"
	case strings.HasPrefix(s, "lc3"):
		return "LC3"
	}
	return ""
}

// classifyProfile returns the kind of a profile name and its suffix.
func classifyProfile(name string) (kind, suffix string) {
	for _, p := range []struct{ prefix, kind string }{
		{"a2dp-sink", "music"}, {"headset-head-unit", "call"},
	} {
		if strings.HasPrefix(name, p.prefix) {
			rest := name[len(p.prefix):]
			if rest == "" || rest[0] == '-' {
				return p.kind, strings.TrimPrefix(rest, "-")
			}
		}
	}
	if name == "off" {
		return "off", ""
	}
	return "", ""
}

func profileCodecLabel(name, desc string) string {
	if _, suffix := classifyProfile(name); suffix != "" {
		if l := codecSuffixLabel(suffix); l != "" {
			return l
		}
	}
	if m := codecDescRe.FindStringSubmatch(desc); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(desc)
}

type pwObject struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
	Info struct {
		Props  map[string]interface{} `json:"props"`
		Params struct {
			EnumProfile []pwProfile `json:"EnumProfile"`
			Profile     []pwProfile `json:"Profile"`
		} `json:"params"`
	} `json:"info"`
}

type pwProfile struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Available   string `json:"available"`
}

func propString(m map[string]interface{}, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	}
	return ""
}

var errCodecNotFound = errors.New("the earbuds' Bluetooth device is not in PipeWire")

// parseCodecState reads a pw-dump document and returns the state of the
// bluez5 device with the given address.
func parseCodecState(dump []byte, address string) (codecState, error) {
	var objs []pwObject
	if err := json.Unmarshal(dump, &objs); err != nil {
		return codecState{}, fmt.Errorf("pw-dump output is not valid JSON: %w", err)
	}
	var dev *pwObject
	for i := range objs {
		o := &objs[i]
		if o.Type != "PipeWire:Interface:Device" || propString(o.Info.Props, "device.api") != "bluez5" {
			continue
		}
		if strings.EqualFold(propString(o.Info.Props, "api.bluez5.address"), address) {
			dev = o
			break
		}
	}
	if dev == nil {
		return codecState{}, errCodecNotFound
	}
	st := codecState{Device: dev.ID, Music: []codecEntry{}}
	activeIdx, activeName, haveActive := 0, "", false
	if len(dev.Info.Params.Profile) > 0 {
		p := dev.Info.Params.Profile[0]
		activeIdx, activeName, haveActive = p.Index, p.Name, true
	}
	// The sink node knows the codec in use; it wins for the active profile.
	nodeCodec := ""
	for i := range objs {
		o := &objs[i]
		if o.Type != "PipeWire:Interface:Node" || propString(o.Info.Props, "device.id") != strconv.Itoa(dev.ID) {
			continue
		}
		if c := propString(o.Info.Props, "api.bluez5.codec"); c != "" {
			nodeCodec = c
			if strings.Contains(propString(o.Info.Props, "media.class"), "Sink") {
				break
			}
		}
	}
	activeLabel := func(fallback string) string {
		if nodeCodec != "" {
			if l := codecSuffixLabel(nodeCodec); l != "" {
				return l
			}
		}
		return fallback
	}
	st.Active = codecActive{Kind: "off"}
	for _, p := range dev.Info.Params.EnumProfile {
		if p.Available == "no" {
			continue
		}
		kind, _ := classifyProfile(p.Name)
		isActive := haveActive && p.Index == activeIdx
		label := profileCodecLabel(p.Name, p.Description)
		switch kind {
		case "music":
			if isActive {
				label = activeLabel(label)
				st.Active = codecActive{Codec: label, Kind: "music"}
			}
			st.Music = append(st.Music, codecEntry{Codec: label, Profile: p.Name, Index: p.Index, Active: isActive})
		case "call":
			if isActive {
				st.Active = codecActive{Codec: activeLabel(label), Kind: "call"}
			}
		}
	}
	// An active profile that is not in the enumerated (available) list.
	if st.Active.Kind == "off" && haveActive {
		if kind, _ := classifyProfile(activeName); kind == "call" {
			st.Active = codecActive{Codec: activeLabel(profileCodecLabel(activeName, "")), Kind: "call"}
		}
	}
	return st, nil
}

func readCodecState(env codecEnv, address string) (codecState, error) {
	if !env.have("pw-dump") {
		return codecState{}, codecErr(exitUnavailable, "pw-dump not found (PipeWire is needed)")
	}
	out, err := env.exec("pw-dump")
	if err != nil {
		return codecState{}, codecErr(exitUnavailable, "pw-dump failed (is PipeWire running?): %v", err)
	}
	st, err := parseCodecState([]byte(out), address)
	if err != nil {
		return codecState{}, codecErr(exitUnavailable, "%v", err)
	}
	return st, nil
}

func codecKey(s string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(s))
}

// setCodec switches the computer to the named music codec and confirms it.
func setCodec(env codecEnv, address, codec string) (codecState, error) {
	st, err := readCodecState(env, address)
	if err != nil {
		return st, err
	}
	var target *codecEntry
	var names []string
	for i := range st.Music {
		names = append(names, st.Music[i].Codec)
		if codecKey(st.Music[i].Codec) == codecKey(codec) {
			target = &st.Music[i]
		}
	}
	if target == nil {
		return st, codecErr(exitFailed, "codec %q is not offered by the earbuds (available: %s)", codec, strings.Join(names, ", "))
	}
	if target.Active && st.Active.Kind == "music" {
		return st, nil
	}
	if !env.have("wpctl") {
		return st, codecErr(exitUnavailable, "wpctl not found (WirePlumber is needed)")
	}
	if _, err := env.exec("wpctl", "set-profile", strconv.Itoa(st.Device), strconv.Itoa(target.Index)); err != nil {
		return st, codecErr(exitFailed, "wpctl set-profile failed: %v", err)
	}
	var last codecState
	// Overall confirm budget (the wpctl call excluded); each re-read is itself
	// bounded by the exec timeout, so the loop ends on the clock, not a count.
	deadline := env.now().Add(codecConfirmBudget)
	for {
		env.sleep(400 * time.Millisecond)
		cur, err := readCodecState(env, address)
		if err != nil {
			return cur, codecErr(exitFailed, "could not confirm the switch: %v", err)
		}
		last = cur
		if cur.Active.Kind == "music" {
			for _, m := range cur.Music {
				if m.Active && m.Index == target.Index {
					return cur, nil
				}
			}
		}
		if !env.now().Before(deadline) {
			break
		}
	}
	return last, codecErr(exitFailed, "the computer did not switch to %s (still %s)", target.Codec, describeActive(last.Active))
}

func describeActive(a codecActive) string {
	switch a.Kind {
	case "music":
		return a.Codec
	case "call":
		return "in a call (" + a.Codec + ")"
	}
	return "off"
}

func cmdCodec(args []string) error {
	return runCodec(codecEnvFn(), args, os.Stdout)
}

func runCodec(env codecEnv, args []string, out io.Writer) error {
	const usage = "usage: qcyctl codec [--json] --address AA:BB:CC:DD:EE:FF | qcyctl codec set <codec> [--json] --address AA:BB:CC:DD:EE:FF"
	jsonOut := false
	address := ""
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--address":
			if i+1 >= len(args) {
				return codecErr(exitUsage, "%s", usage)
			}
			i++
			address = args[i]
		case strings.HasPrefix(a, "--address="):
			address = strings.TrimPrefix(a, "--address=")
		case strings.HasPrefix(a, "--"):
			return codecErr(exitUsage, "codec: unknown flag %s\n%s", a, usage)
		default:
			pos = append(pos, a)
		}
	}
	if address == "" {
		return codecErr(exitUsage, "codec: --address is required\n%s", usage)
	}
	hw, err := net.ParseMAC(address)
	if err != nil || len(hw) != 6 {
		return codecErr(exitUsage, "codec: invalid --address %q\n%s", address, usage)
	}
	address = hw.String() // AA-BB-... and aa:bb:... both match the device
	setting := false
	switch {
	case len(pos) == 0:
	case pos[0] == "set" && len(pos) == 2:
		setting = true
	default:
		return codecErr(exitUsage, "%s", usage)
	}
	var st codecState
	if setting {
		st, err = setCodec(env, address, pos[1])
	} else {
		st, err = readCodecState(env, address)
	}
	if err != nil {
		return err
	}
	if jsonOut {
		b, err := json.Marshal(st)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(b))
		return err
	}
	fmt.Fprintf(out, "device: %d\nactive: %s\n", st.Device, describeActive(st.Active))
	var names []string
	for _, m := range st.Music {
		names = append(names, m.Codec)
	}
	fmt.Fprintf(out, "available: %s\n", strings.Join(names, ", "))
	return nil
}
