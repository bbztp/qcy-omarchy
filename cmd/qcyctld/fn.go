package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/protocol"
	"github.com/bbztp/qcy-omarchy/internal/qcycore/models"
)

// Generic functions: the controls the model's panel declares that no
// specialised handler covers. fnList describes them; fnSet writes exactly the
// bytes the panel declares for one of them. Nothing here invents a frame: a
// function whose declared bytes do not validate is simply not offered.
//
//	fnList                                   -> []fnItem
//	fnSet {cmdId, value | on | off,
//	       confirm?, confirmAgain?}          -> fnResult

// fnQueryTimeout bounds one state query (the result, silent or not, is cached
// for the connection).
var fnQueryTimeout = 600 * time.Millisecond

// fnReadbackTimeout bounds the readback after a write.
var fnReadbackTimeout = 3 * time.Second

// fnSettle is the pause before the readback query (the earbuds echo the
// pre-write state when asked at once).
var fnSettle = 200 * time.Millisecond

// fnSilentLimit: after this many unanswered queries in a row the rest of the
// pass is skipped, so a unit that answers none costs a few seconds once.
const fnSilentLimit = 3

// fnPlainCharacter is the characteristic the daemon's send path writes to;
// a function that names another one is not driven.
const fnPlainCharacter = "00001001"

var fnForbidden = regexp.MustCompile(`(?i)firmware|upgrade|\bota\b|update`)

// fnSpecialOpcodes are the opcodes some specialised handler already drives;
// a generic function on one of them would be a second path to the same
// setting (and could bypass the gates of the first).
var fnSpecialOpcodes = func() map[byte]bool {
	m := map[byte]bool{}
	for _, op := range protocol.ActionOpcodes {
		m[op] = true
	}
	for _, op := range []byte{protocol.OpEQ, protocol.OpHearingMode, protocol.OpHearingLeft,
		protocol.OpHearingRight, protocol.OpBattery, protocol.OpVersion, protocol.OpQuery, protocol.SOF} {
		m[op] = true
	}
	return m
}()

type fnOption struct {
	Label  string `json:"label"`
	Value  string `json:"value"` // hex of the parameter bytes
	params []byte
	never  bool
	mins   int
}

// genericFn is one validated, drivable panel function.
type genericFn struct {
	CmdID  int
	Opcode byte
	Kind   string // toggle, choices, standby, language, command
	Name   string
	// Text is the panel's confirmation / restart text, when it has one.
	Text string
	// Restart: the panel says the earbuds restart after the write.
	Restart bool
	// NeedsConfirm: the write needs confirm:true. Danger (reset, clear
	// pairing, factory) needs confirmAgain:true as well.
	NeedsConfirm bool
	Danger       bool
	Options      []fnOption
	on, off      []byte
}

// type of the widget the panel should draw.
func (f genericFn) Type() string {
	switch f.Kind {
	case "toggle":
		return "toggle"
	case "command":
		return "action"
	}
	return "list"
}

func (f genericFn) queryable() bool { return f.Kind != "command" }

// parsePanelFrame validates one frame the panel declares. Accepted shapes:
//
//	FF <bodyLen> <op> <paramLen> <params...>   exactly one block, no extra bytes
//	<op> <paramLen> <params...>                 the same block without the envelope
//	<op>                                        only when allowBare (a command with no parameters)
//
// wantOp, when set, must equal the opcode (the function's declared cmdId).
func parsePanelFrame(h string, wantOp *int, allowBare bool) (byte, []byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(h))
	if err != nil || len(b) == 0 {
		return 0, nil, fmt.Errorf("frame %q is not hex", h)
	}
	var op byte
	var params []byte
	switch {
	case b[0] == protocol.SOF:
		if len(b) < 4 || int(b[1]) != len(b)-2 || int(b[3]) != len(b)-4 {
			return 0, nil, fmt.Errorf("frame %x: inconsistent lengths", b)
		}
		op, params = b[2], b[4:]
	case len(b) == 1 && allowBare:
		op = b[0]
	case len(b) >= 2 && int(b[1]) == len(b)-2:
		op, params = b[0], b[2:]
	default:
		return 0, nil, fmt.Errorf("frame %x: inconsistent lengths", b)
	}
	if wantOp != nil && (*wantOp < 0 || *wantOp > 255 || byte(*wantOp) != op) {
		return 0, nil, fmt.Errorf("frame %x: opcode 0x%02x is not the declared cmdId %d", b, op, *wantOp)
	}
	if op == protocol.OpQuery || op == protocol.SOF {
		return 0, nil, fmt.Errorf("frame %x: reserved opcode", b)
	}
	if len(params) > 32 {
		return 0, nil, fmt.Errorf("frame %x: too long", b)
	}
	return op, params, nil
}

func fnText(m models.Messages) string {
	for _, s := range []string{m.RebootTip, m.AlarmMsg, m.ConfTip, m.Msg, m.MsgAndroid, m.MsgIos, m.ToastTip} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// buildGenericFn validates one panel function. ok=false means it is not
// offered (no drivable block, undeclared shape, invalid bytes, forbidden).
func buildGenericFn(f models.Function, autoOffCovered bool) (genericFn, bool) {
	if f.Character != "" && !strings.EqualFold(f.Character, fnPlainCharacter) {
		return genericFn{}, false
	}
	if fnForbidden.MatchString(f.Name) {
		return genericFn{}, false
	}
	g := genericFn{Name: f.Name}
	var op byte
	var err error
	cmd := f.CmdID

	switch {
	case f.ToggleBlock != nil:
		t, _ := f.Toggle()
		if cmd == nil || len(t.On) != 1 || len(t.Off) != 1 {
			return genericFn{}, false
		}
		var onP, offP []byte
		var o2 byte
		if op, onP, err = parsePanelFrame(t.On[0], cmd, false); err != nil {
			return genericFn{}, false
		}
		if o2, offP, err = parsePanelFrame(t.Off[0], cmd, false); err != nil || o2 != op {
			return genericFn{}, false
		}
		if len(onP) == 0 || bytes.Equal(onP, offP) {
			return genericFn{}, false
		}
		g.Kind, g.on, g.off = "toggle", onP, offP
	case f.CommandBlock != nil:
		c, _ := f.Command()
		var p []byte
		if op, p, err = parsePanelFrame(c.Bytes, cmd, true); err != nil {
			return genericFn{}, false
		}
		g.Kind, g.on = "command", p
		g.Danger, g.NeedsConfirm = true, true
		if cmd == nil {
			id := int(op)
			cmd = &id
		}
	case f.StandbyBlock != nil:
		s, _ := f.Standby()
		if cmd == nil || *cmd < 0 || *cmd > 255 || len(s.Options) == 0 {
			return genericFn{}, false
		}
		if autoOffCovered || *cmd == int(protocol.OpAutoOff) {
			return genericFn{}, false
		}
		op = byte(*cmd)
		for _, o := range s.Options {
			p, err := hex.DecodeString(o.Bytes)
			if err != nil || len(p) != 2 {
				return genericFn{}, false
			}
			opt := fnOption{Label: o.Label, Value: strings.ToUpper(o.Bytes), params: p, never: o.Never}
			if o.Minutes != nil {
				opt.mins = *o.Minutes
			}
			g.Options = append(g.Options, opt)
		}
		g.Kind = "standby"
	case f.LanguageBlock != nil:
		l, _ := f.Language()
		if cmd == nil || *cmd < 0 || *cmd > 255 || len(l.Options) == 0 {
			return genericFn{}, false
		}
		op = byte(*cmd)
		for _, o := range l.Options {
			p, err := hex.DecodeString(o.Bytes)
			if err != nil || len(p) != 4 {
				return genericFn{}, false
			}
			g.Options = append(g.Options, fnOption{Label: o.Label, Value: strings.ToUpper(o.Bytes), params: p})
		}
		g.Kind = "language"
	case len(f.ChoiceList) > 0:
		// Only options that carry their bytes are driven: a bare level
		// number has no declared frame, so none is invented for it.
		if cmd == nil || *cmd < 0 || *cmd > 255 {
			return genericFn{}, false
		}
		op = byte(*cmd)
		for _, c := range f.Choices() {
			p, err := hex.DecodeString(c.Bytes)
			if c.Bytes == "" || err != nil || len(p) == 0 || len(p) > 8 {
				return genericFn{}, false
			}
			g.Options = append(g.Options, fnOption{Label: c.Label, Value: strings.ToUpper(c.Bytes), params: p})
		}
		g.Kind = "choices"
	default:
		return genericFn{}, false
	}
	if cmd == nil {
		return genericFn{}, false
	}
	if fnSpecialOpcodes[op] {
		return genericFn{}, false
	}
	if e, ok := protocol.Lookup(op); ok && fnForbidden.MatchString(e.Name) {
		return genericFn{}, false
	}
	g.CmdID, g.Opcode = *cmd, op
	if m, ok := f.Messages(); ok {
		g.Text = fnText(m)
		if m.RebootTip != "" || m.AlarmMsg != "" || m.ConfTip != "" {
			g.Restart, g.NeedsConfirm = true, true
		}
	}
	return g, true
}

// fnFunctionsFor is the panel source; tests replace it.
var fnFunctionsFor = models.FunctionsFor

// genericFunctionsFor lists the drivable functions of a vendorId, in panel
// order. autoOff says the handler already drives the auto power-off.
func genericFunctionsFor(vendorID uint16, autoOff bool) []genericFn {
	var out []genericFn
	seen := map[int]bool{}
	for _, f := range fnFunctionsFor(vendorID) {
		g, ok := buildGenericFn(f, autoOff)
		if !ok || seen[g.CmdID] {
			continue
		}
		seen[g.CmdID] = true
		out = append(out, g)
	}
	return out
}

// genericFns is the list for the connected model (empty when the vendorId is
// not known or the panel declares nothing generic).
func (d *daemon) genericFns() []genericFn {
	d.mu.Lock()
	vendor, have := d.vendorID, d.haveVendor
	d.mu.Unlock()
	if !have {
		return nil
	}
	autoOff := false
	if h := d.knownHandler(); h != nil {
		for _, a := range h.Capabilities().Actions {
			if daemonActionID(a.ID) == "autoOff" {
				autoOff = true
			}
		}
	}
	return genericFunctionsFor(vendor, autoOff)
}

// ---- state ----

// fnState turns a reply into the function's value: "on"/"off" for a toggle,
// the option's value (hex) for a list; ok=false when the reply matches none.
func (f genericFn) state(reply []byte) (string, bool) {
	switch f.Kind {
	case "toggle":
		switch {
		case bytes.HasPrefix(reply, f.on):
			return "on", true
		case bytes.HasPrefix(reply, f.off):
			return "off", true
		case len(f.on) == 1 && f.on[0] == 1 && len(reply) >= 1 && (reply[0] == 0 || reply[0] == 2):
			return "off", true // 0 and 2 both mean off on the shared toggles
		}
	case "choices", "standby", "language":
		for _, o := range f.Options {
			if bytes.HasPrefix(reply, o.params) {
				return o.Value, true
			}
		}
	}
	return "", false
}

type fnReading struct {
	raw   []byte
	value string
	ok    bool // the unit answered
}

// fnCacheFor returns the per-connection reading cache, replacing it when the
// session changed (a reconnect, e.g. after a restart the earbuds announced).
func (d *daemon) fnCacheLocked() map[int]fnReading {
	s := d.sess
	if d.fnSess != s || d.fnCache == nil {
		d.fnSess, d.fnCache = s, map[int]fnReading{}
	}
	return d.fnCache
}

// fnRead returns the readings of fns, querying what is not cached yet. A
// silent function is remembered as silent for the connection. No lock may be
// held by the caller.
func (d *daemon) fnRead(fns []genericFn) map[int]fnReading {
	out := map[int]fnReading{}
	silent := 0
	for _, f := range fns {
		if !f.queryable() {
			continue
		}
		d.mu.Lock()
		r, have := d.fnCacheLocked()[f.CmdID]
		d.mu.Unlock()
		if !have {
			if silent >= fnSilentLimit {
				r = fnReading{}
			} else if p, err := d.query(f.Opcode, fnQueryTimeout); err == nil && len(p) > 0 {
				r = fnReading{raw: p, ok: true}
				r.value, _ = f.state(p)
				silent = 0
			} else {
				r = fnReading{}
				silent++
			}
			d.mu.Lock()
			d.fnCacheLocked()[f.CmdID] = r
			d.mu.Unlock()
		}
		out[f.CmdID] = r
	}
	return out
}

type fnItem struct {
	CmdID   int        `json:"cmdId"`
	Type    string     `json:"type"` // toggle, list, action
	Kind    string     `json:"kind"`
	Label   string     `json:"label"`
	Options []fnOption `json:"options,omitempty"`
	// State: "on"/"off", the option value (hex), or "" when unknown; Raw is
	// the device's reply (hex) when it answered.
	State   string `json:"state,omitempty"`
	Raw     string `json:"raw,omitempty"`
	Text    string `json:"text,omitempty"`
	Restart bool   `json:"restart,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
	Danger  bool   `json:"danger,omitempty"`
}

func (d *daemon) fnList() []fnItem {
	fns := d.genericFns()
	out := make([]fnItem, 0, len(fns))
	if len(fns) == 0 {
		return out
	}
	rd := d.fnRead(fns)
	for _, f := range fns {
		it := fnItem{CmdID: f.CmdID, Type: f.Type(), Kind: f.Kind, Label: f.Name, Options: f.Options,
			Text: f.Text, Restart: f.Restart, Confirm: f.NeedsConfirm, Danger: f.Danger}
		if r, ok := rd[f.CmdID]; ok && r.ok {
			it.State, it.Raw = r.value, hex.EncodeToString(r.raw)
		}
		out = append(out, it)
	}
	return out
}

// fnStatus is the "functions" map of the status report: cmdId -> value
// ("on", "off", an option value, or "raw:<hex>" for a reply no option
// matches). Nil when the model declares no generic function or none answered.
func (d *daemon) fnStatus() map[string]string {
	fns := d.genericFns()
	if len(fns) == 0 {
		return nil
	}
	rd := d.fnRead(fns)
	out := map[string]string{}
	for _, f := range fns {
		r, ok := rd[f.CmdID]
		if !ok || !r.ok {
			continue
		}
		if r.value != "" {
			out[strconv.Itoa(f.CmdID)] = r.value
		} else {
			out[strconv.Itoa(f.CmdID)] = "raw:" + hex.EncodeToString(r.raw)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---- write ----

type fnResult struct {
	CmdID int    `json:"cmdId"`
	Sent  string `json:"sent"`
	// Confirmed: the readback matched the value written.
	Confirmed bool   `json:"confirmed"`
	State     string `json:"state,omitempty"`
	Raw       string `json:"raw,omitempty"`
	// Restart: the panel announces a restart and the link did not confirm;
	// the write was not repeated.
	Restart bool   `json:"restart,omitempty"`
	Note    string `json:"note,omitempty"`
}

func fnCmdID(v interface{}) (int, error) {
	switch x := v.(type) {
	case float64:
		if x == float64(int(x)) && x >= 0 && x <= 255 {
			return int(x), nil
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil && n >= 0 && n <= 255 {
			return n, nil
		}
	}
	return 0, errors.New("fnSet: cmdId must be an integer 0..255")
}

func truthy(v interface{}) (on, ok bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case float64:
		if x == 1 {
			return true, true
		}
		if x == 0 {
			return false, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "on", "1", "true", "yes":
			return true, true
		case "off", "0", "false", "no":
			return false, true
		}
	}
	return false, false
}

// resolve picks the parameter bytes and the wanted state for the request.
func (f genericFn) resolve(args map[string]interface{}) (params []byte, want string, err error) {
	switch f.Kind {
	case "command":
		return f.on, "", nil
	case "toggle":
		if v, ok := args["on"]; ok {
			if b, _ := v.(bool); b {
				return f.on, "on", nil
			}
		}
		if v, ok := args["off"]; ok {
			if b, _ := v.(bool); b {
				return f.off, "off", nil
			}
		}
		if v, ok := args["value"]; ok {
			if on, ok := truthy(v); ok {
				if on {
					return f.on, "on", nil
				}
				return f.off, "off", nil
			}
		}
		return nil, "", fmt.Errorf("fnSet: %q is a toggle: give on:true, off:true or value on|off", f.Name)
	}
	v, ok := args["value"]
	if !ok {
		return nil, "", fmt.Errorf("fnSet: %q needs a value (%s)", f.Name, f.optionList())
	}
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		hx := strings.ToUpper(strings.TrimPrefix(strings.ToLower(s), "0x"))
		for _, o := range f.Options {
			if o.Value == hx {
				return o.params, o.Value, nil
			}
		}
		for _, o := range f.Options {
			if strings.EqualFold(o.Label, s) {
				return o.params, o.Value, nil
			}
		}
		if f.Kind == "standby" {
			if strings.EqualFold(s, "never") {
				for _, o := range f.Options {
					if o.never {
						return o.params, o.Value, nil
					}
				}
			}
			if n, err := strconv.Atoi(s); err == nil {
				return f.byMinutes(n)
			}
		}
	case float64:
		if f.Kind == "standby" && x == float64(int(x)) {
			return f.byMinutes(int(x))
		}
	}
	return nil, "", fmt.Errorf("fnSet: %v is not an option of %q (%s)", v, f.Name, f.optionList())
}

func (f genericFn) byMinutes(n int) ([]byte, string, error) {
	for _, o := range f.Options {
		if !o.never && o.mins == n {
			return o.params, o.Value, nil
		}
	}
	return nil, "", fmt.Errorf("fnSet: %d minutes is not an option of %q (%s)", n, f.Name, f.optionList())
}

func (f genericFn) optionList() string {
	var s []string
	for _, o := range f.Options {
		s = append(s, o.Label+"="+o.Value)
	}
	return strings.Join(s, ", ")
}

func argBool(args map[string]interface{}, k string) bool {
	b, _ := args[k].(bool)
	return b
}

// fnSet writes one declared generic function. The frame is built from the
// panel's own bytes (protocol.Block through d.send) and never repeated.
func (d *daemon) fnSet(args map[string]interface{}) (*fnResult, error) {
	id, err := fnCmdID(args["cmdId"])
	if err != nil {
		return nil, err
	}
	// Whatever the panel declares, the opcodes of the specialised controls
	// (EQ 0x22, ANC 0x17, prompt volume 0x1D, ...) are never reached here.
	if fnSpecialOpcodes[byte(id)] {
		return nil, fmt.Errorf("fnSet: function %d is driven by a specialised control, not by the generic path", id)
	}
	var f genericFn
	found := false
	for _, g := range d.genericFns() {
		if g.CmdID == id {
			f, found = g, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("fnSet: function %d is not declared by this model's panel, or is handled by a specialised control", id)
	}
	params, want, err := f.resolve(args)
	if err != nil {
		return nil, err
	}
	if f.NeedsConfirm && !argBool(args, "confirm") {
		return nil, fmt.Errorf("fnSet: %q needs confirm:true%s", f.Name, withText(f.Text))
	}
	if f.Danger && !argBool(args, "confirmAgain") {
		return nil, fmt.Errorf("fnSet: %q is a reset-type command: it needs confirm:true and confirmAgain:true%s", f.Name, withText(f.Text))
	}

	d.txMu.Lock()
	defer d.txMu.Unlock()
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}
	res := &fnResult{CmdID: id, Sent: hex.EncodeToString(protocol.Block(f.Opcode, params...))}
	d.drain()
	if err := d.send(f.Opcode, params); err != nil {
		return nil, err
	}
	if !f.queryable() {
		_, _ = d.wait(f.Opcode, 250*time.Millisecond)
		res.Note = "command sent; the device does not report a state for it"
		return res, nil
	}
	d.sleep(fnSettle)
	if err := d.send(protocol.OpQuery, []byte{f.Opcode}); err != nil {
		if f.Restart {
			res.Restart, res.Note = true, "written; the earbuds restart, re-read the state once they are back"
			d.fnForget(f.CmdID)
			return res, nil
		}
		return nil, err
	}
	match := func(p []byte) bool {
		if len(p) == 0 {
			return false
		}
		s, ok := f.state(p)
		return ok && s == want
	}
	got, last, ok := d.awaitMatchLast(f.Opcode, match, fnReadbackTimeout)
	d.fnForget(f.CmdID)
	switch {
	case ok:
		res.Confirmed = true
		res.Raw = hex.EncodeToString(got)
		res.State = want
		d.mu.Lock()
		d.fnCacheLocked()[f.CmdID] = fnReading{raw: got, value: want, ok: true}
		d.mu.Unlock()
	case len(last) > 0 && d.isConnected():
		if f.Restart {
			res.Restart, res.Raw = true, hex.EncodeToString(last)
			res.Note = "written; the device still reports the old value, the restart may be pending"
			return res, nil
		}
		return nil, fmt.Errorf("fnSet: %q written but the device reported %x (wanted %x); re-read the state, the write is not repeated", f.Name, last, params)
	case f.Restart || !d.isConnected():
		res.Restart = true
		res.Note = "written; the link dropped or stayed silent (the earbuds restart); the write is not repeated"
	default:
		res.Note = "written; the device did not answer the readback query"
	}
	return res, nil
}

func withText(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// fnForget drops the cached reading of one function so the next read asks
// again.
func (d *daemon) fnForget(id int) {
	d.mu.Lock()
	delete(d.fnCacheLocked(), id)
	d.mu.Unlock()
}
