package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/protocol"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
	quicky "github.com/hui1601/Quicky/lib"
	"tinygo.org/x/bluetooth"
)

var (
	svcUUID      = mustUUID("0000a001-0000-1000-8000-00805f9b34fb")
	cmdUUID      = mustUUID("00001001-0000-1000-8000-00805f9b34fb")
	notifyUUID   = mustUUID("00001002-0000-1000-8000-00805f9b34fb")
	battUUID     = mustUUID("00000008-0000-1000-8000-00805f9b34fb")
	verUUID      = mustUUID("00000007-0000-1000-8000-00805f9b34fb")
	keysUUID     = mustUUID("0000000d-0000-1000-8000-00805f9b34fb")
	eqPresetUUID = mustUUID("0000000b-0000-1000-8000-00805f9b34fb")

	// ANC opcode is 0x17 with 3 params [mode, scene, value].
	// "anc" is an alias for "adaptive".
	ancModes = map[string][3]byte{
		"off":          {0x02, 0x00, 0x00},
		"adaptive":     {0x01, 0x05, 0x00},
		"anc":          {0x01, 0x05, 0x00},
		"indoor":       {0x01, 0x01, 0x02},
		"commuting":    {0x01, 0x02, 0x02},
		"noisy":        {0x01, 0x03, 0x02},
		"antiwind":     {0x01, 0x04, 0x00},
		"transparency": {0x03, 0x01, 0x04},
	}

	// Toggle opcodes: param [0x01]=on, [0x02]=off.
	toggles = map[string]byte{
		"game":       protocol.OpGame,
		"sleep":      protocol.OpSleep,
		"ldac":       protocol.OpLDAC,
		"multipoint": protocol.OpMultipoint,
		"wind":       protocol.OpWind,
	}
)

// debugEnabled gates protocol tracing (GATT writes, notifications, retries).
// It is off by default so stdout stays clean for machine-readable output;
// set QCY_DEBUG=1 to trace to stderr.
var debugEnabled = os.Getenv("QCY_DEBUG") != ""

// dbg writes protocol tracing to stderr when QCY_DEBUG is set.
func dbg(format string, args ...any) {
	if debugEnabled {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

func mustUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic("bad UUID " + s + ": " + err.Error())
	}
	return u
}

// daemonClient talks to qcyctld via Unix socket.
type daemonClient struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
}

// daemonRequestTimeout bounds one request/response round trip. The daemon may
// have to connect (up to 30 s) and run a full status sweep first, so this is
// generous; its job is only to stop a wedged daemon hanging the CLI forever.
const daemonRequestTimeout = 90 * time.Second

// hardwareAllowed is the structural guard against tests reaching the real
// earbuds: TestMain sets it to false, and every path that opens the daemon
// socket or the BLE adapter refuses first (refuseHardware).
var hardwareAllowed = true

var errHardwareDisabled = errors.New("hardware access disabled in tests")

func refuseHardware() error {
	if !hardwareAllowed {
		return errHardwareDisabled
	}
	return nil
}

func dialDaemon(timeout time.Duration) (*daemonClient, error) {
	if err := refuseHardware(); err != nil {
		return nil, err
	}
	sock, err := daemonSocketPath()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return nil, err
	}
	return &daemonClient{
		conn: conn,
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(conn),
	}, nil
}

func daemonSocketPath() (string, error) {
	return qcycore.SocketPath(false)
}

func (dc *daemonClient) Close() error {
	return dc.conn.Close()
}

func (dc *daemonClient) request(cmd string, args map[string]interface{}) (interface{}, error) {
	req := map[string]interface{}{
		"cmd":  cmd,
		"args": args,
	}
	// Re-armed per round trip because the connection is reused.
	_ = dc.conn.SetDeadline(time.Now().Add(daemonRequestTimeout))
	if err := dc.enc.Encode(req); err != nil {
		return nil, err
	}
	var resp map[string]interface{}
	if err := dc.dec.Decode(&resp); err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		msg, _ := resp["error"].(string)
		if code, _ := resp["code"].(string); isEqCode(code) {
			// Keep the daemon's classification: callers match it with
			// errors.Is(err, qcycore.Err*) / errors.As(*qcycore.EqError).
			ee := &qcycore.EqError{Code: code, Msg: msg}
			ee.Backup, _ = resp["backup"].(string)
			if h, _ := resp["sent"].(string); h != "" {
				ee.Sent, _ = hex.DecodeString(h)
			}
			if h, _ := resp["deviceReported"].(string); h != "" {
				ee.Got, _ = hex.DecodeString(h)
			}
			return nil, ee
		}
		switch {
		case strings.HasPrefix(msg, "unknown command"):
			msg += " (the running qcyctld is older than this qcyctl; restart it)"
		case msg == "":
			msg = "daemon reported failure without a message"
		}
		return nil, errors.New(msg)
	}
	return resp["data"], nil
}

// isEqCode reports whether code is one of the EQ write classes the daemon
// sends (qcycore.Code*).
func isEqCode(code string) bool {
	switch code {
	case qcycore.CodeProtectedSlot, qcycore.CodeFullLengthRequired, qcycore.CodeUnconfirmed:
		return true
	}
	return false
}

func (dc *daemonClient) status() (map[string]interface{}, error) {
	data, err := dc.request("status", nil)
	if err != nil {
		return nil, err
	}
	raw, ok := data.(map[string]interface{})
	if !ok {
		return nil, errors.New("daemon returned a non-object status")
	}
	rep := statusFromDaemon(raw)
	return structToMap(rep)
}

// statusFromDaemon turns the daemon's raw per-opcode dump into the same schema
// the direct client produces, so the UI cannot tell which path served it. The
// daemon deliberately returns unparsed bytes; all decoding lives here, beside
// the helpers the direct path already uses.
func statusFromDaemon(raw map[string]interface{}) *statusReport {
	rep := &statusReport{Toggles: map[string]bool{}}
	rep.Stale = boolOf(raw["stale"])
	rep.AgeSeconds = intOf(raw["ageSeconds"])

	if b, ok := raw["battery"].(map[string]interface{}); ok {
		rep.Battery = &batteryReport{
			Left: intOf(b["left"]), Right: intOf(b["right"]), Case: intOf(b["case"]),
			LeftCharging: boolOf(b["leftCharging"]), RightCharging: boolOf(b["rightCharging"]),
			CaseCharging: boolOf(b["caseCharging"]), CasePresent: boolOf(b["casePresent"]),
		}
	}
	if p := bytesOf(raw["firmwareBytes"]); len(p) > 0 {
		l, r, err := parseVersion(p)
		if err == nil {
			rep.Firmware = &fwReport{Left: l, Right: r}
		}
	}
	if a, ok := raw["anc"].(map[string]interface{}); ok {
		mr, sr, v := intOf(a["modeRaw"]), intOf(a["sceneRaw"]), intOf(a["value"])
		rep.Noise = &noiseReport{
			Mode: noiseModeName(byte(mr)), Scene: noiseSceneName(byte(mr), byte(sr)),
			ModeRaw: mr, SceneRaw: sr, Value: v,
		}
	}
	if t, ok := raw["toggles"].(map[string]interface{}); ok {
		for k, v := range t {
			rep.Toggles[k] = boolOf(v)
		}
	}
	if p := bytesOf(raw["promptVol"]); len(p) >= 1 {
		pr := &promptReport{Value: int(p[0])}
		if len(p) >= 2 {
			pr.Max = int(p[1])
		}
		rep.PromptVol = pr
	}
	if p := bytesOf(raw["autoOff"]); len(p) >= 2 {
		ao := &autoOffReport{}
		if v := u16le(p, 0); v != 0xFFFF {
			m := int(v)
			ao.Minutes = &m
		}
		rep.AutoOff = ao
	}
	if p := bytesOf(raw["wear"]); len(p) >= 1 {
		rep.Wear = &wearReport{Enabled: p[0] == 0x01}
	}
	if p := bytesOf(raw["balance"]); len(p) >= 1 {
		b := int(p[0])
		rep.Balance = &b
	}
	if c, ok := raw["capabilities"].(map[string]interface{}); ok {
		rep.Caps = map[string]bool{}
		for k, v := range c {
			if b, ok := v.(bool); ok {
				rep.Caps[k] = b
			}
		}
	}
	if m, ok := raw["model"].(map[string]interface{}); ok {
		rep.Model = m
	}
	if f, ok := raw["functions"].(map[string]interface{}); ok {
		rep.Functions = map[string]string{}
		for k, v := range f {
			if sv, ok := v.(string); ok {
				rep.Functions[k] = sv
			}
		}
	}
	if v := intOf(raw["wearSensitivity"]); v >= 1 && v <= 3 {
		rep.WearSensitivity = &v
	}
	if p := bytesOf(raw["spatial"]); len(p) >= 1 {
		rep.Spatial = &spatialReport{Enabled: p[0] == 0x01}
	}
	if p := bytesOf(raw["keys"]); len(p) > 0 {
		k := &keysReport{Raw: fmt.Sprintf("%x", p)}
		for i := 0; i+1 < len(p); i += 2 {
			k.Pairs = append(k.Pairs, fmt.Sprintf("%02x:%02x", p[i], p[i+1]))
		}
		rep.Keys = k
	}
	if p := bytesOf(raw["eq"]); len(p) > 0 {
		if eq, err := eqReportFrom(p); err == nil {
			rep.EQ = eq
		}
	}
	return rep
}

func intOf(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func boolOf(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

// bytesOf converts a JSON array of numbers back into the raw opcode payload.
func bytesOf(v interface{}) []byte {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]byte, 0, len(arr))
	for _, e := range arr {
		if f, ok := e.(float64); ok {
			out = append(out, byte(f))
		}
	}
	return out
}

func (dc *daemonClient) battery() (map[string]interface{}, error) {
	data, err := dc.request("battery", nil)
	if err != nil {
		return nil, err
	}
	m, ok := data.(map[string]interface{})
	if !ok {
		return nil, errors.New("daemon returned a non-object battery")
	}
	return m, nil
}

func (dc *daemonClient) setAction(action string, params []byte) error {
	// params must go over the wire as a JSON array of numbers. A []byte would
	// marshal to a base64 string, which the daemon's []interface{} assertion
	// rejects — the setter then goes out with an empty body and the earbuds
	// silently ignore it.
	list := make([]int, len(params))
	for i, b := range params {
		list[i] = int(b)
	}
	args := map[string]interface{}{
		"action": action,
		"params": list,
	}
	_, err := dc.request("set", args)
	return err
}

func (dc *daemonClient) find() error {
	return dc.findTone(0x00)
}

func (dc *daemonClient) findTone(tone byte) error {
	_, err := dc.request("find", map[string]interface{}{"params": numList([]byte{tone})})
	return err
}

func (dc *daemonClient) setActionExpect(action string, params, expect []byte) ([]byte, error) {
	data, err := dc.request("set", map[string]interface{}{
		"action": action,
		"params": numList(params),
		"expect": numList(expect),
	})
	if err != nil {
		return nil, err
	}
	if m, ok := data.(map[string]interface{}); ok {
		if msg, _ := m["indeterminate"].(string); msg != "" {
			return nil, &indeterminateError{msg: msg}
		}
	}
	return bytesOf(data), nil
}

// indeterminateError: the write was accepted but its outcome is not known yet
// (ldac echoing the old value while the restart is pending). Not a failure,
// and never retried by writing again.
type indeterminateError struct{ msg string }

func (e *indeterminateError) Error() string { return e.msg }

func (dc *daemonClient) keys() ([]byte, error) {
	data, err := dc.request("keys", nil)
	if err != nil {
		return nil, err
	}
	return bytesOf(data), nil
}

func (dc *daemonClient) keysSet(raw []byte) error {
	_, err := dc.request("keysSet", map[string]interface{}{"raw": numList(raw)})
	return err
}

// remarshal converts a decoded JSON value into a typed struct.
func remarshal(data interface{}, out interface{}) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// eqRead asks the daemon for the current 0x22 payload, decoded.
func (dc *daemonClient) eqRead() (*qcycore.EqSnapshot, error) {
	data, err := dc.request("eq", nil)
	if err != nil {
		return nil, err
	}
	var snap qcycore.EqSnapshot
	if err := remarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("daemon returned a malformed eq snapshot: %w", err)
	}
	return &snap, nil
}

// eqApply has the daemon read the current payload, build the one to write,
// back it up and write it, all under one transaction lock.
func (dc *daemonClient) eqApply(op qcycore.EqOp) (*qcycore.EqResult, error) {
	args := map[string]interface{}{}
	if op.Gains != nil {
		args["gains"] = op.Gains[:]
	}
	if op.Bands != nil {
		args["bands"] = op.Bands
	}
	if op.AllowProtected {
		args["allowProtected"] = true
	}
	if op.AllowFullLength {
		args["allowFull"] = true
	}
	if op.MasterCenti != nil {
		args["masterGain"] = *op.MasterCenti
	}
	if op.Slot != nil {
		args["slot"] = *op.Slot
	}
	data, err := dc.request("eqApply", args)
	if err != nil {
		return nil, err
	}
	var res qcycore.EqResult
	if err := remarshal(data, &res); err != nil {
		return nil, fmt.Errorf("daemon returned a malformed eq result: %w", err)
	}
	return &res, nil
}

// wearSet has the daemon read the 0x2C payload, set byte 0 and write it back.
func (dc *daemonClient) wearSet(state byte) ([]byte, error) {
	data, err := dc.request("wear", map[string]interface{}{"state": int(state)})
	if err != nil {
		return nil, err
	}
	return bytesOf(data), nil
}

// wearOffByte is the diagnostic wear write: exactly byte b (0 or 2), no
// default-off mapping on the daemon side.
func (dc *daemonClient) wearOffByte(b byte) ([]byte, error) {
	data, err := dc.request("wear", map[string]interface{}{"state": int(b), "literal": true})
	if err != nil {
		return nil, err
	}
	return bytesOf(data), nil
}

func (dc *daemonClient) windOffByte(b byte) ([]byte, error) {
	data, err := dc.request("windOffByte", map[string]interface{}{"byte": int(b)})
	if err != nil {
		return nil, err
	}
	return bytesOf(data), nil
}

// fitResult is the outcome of the ear-tip fit test: per ear 3 sealed, 2 good,
// 1 fair, 0 poor.
type fitResult struct {
	Status int `json:"status"`
	Left   int `json:"left"`
	Right  int `json:"right"`
}

func (dc *daemonClient) fitTest(seconds int) (fitResult, error) {
	data, err := dc.request("fitTest", map[string]interface{}{"seconds": seconds})
	if err != nil {
		return fitResult{}, err
	}
	var r fitResult
	if err := remarshal(data, &r); err != nil {
		return fitResult{}, fmt.Errorf("daemon returned a malformed fit result: %w", err)
	}
	return r, nil
}

func (dc *daemonClient) fitStop() error {
	_, err := dc.request("fitStop", nil)
	return err
}

// noiseRead returns the raw 0x17 payload [mode, scene, level].
func (dc *daemonClient) noiseRead() ([]byte, error) {
	data, err := dc.request("noise", nil)
	if err != nil {
		return nil, err
	}
	return bytesOf(data), nil
}

// numList renders a byte slice as a JSON array of numbers. A []byte marshals to
// a base64 string, which the daemon's array decoder rejects.
func numList(b []byte) []int {
	out := make([]int, len(b))
	for i, c := range b {
		out[i] = int(c)
	}
	return out
}

// daemonAPI defines the interface for both daemon client and direct client wrapper.
type daemonAPI interface {
	status() (map[string]interface{}, error)
	battery() (map[string]interface{}, error)
	setAction(action string, params []byte) error
	find() error
	// setActionExpect writes a setter and waits for the device to report the
	// expected payload, for the setters whose end state must be known
	// (balance, prompt volume, auto-off).
	setActionExpect(action string, params, expect []byte) ([]byte, error)
	findTone(tone byte) error
	keys() ([]byte, error)
	keysSet(raw []byte) error
	// eqRead returns the current 0x22 payload, decoded (dB).
	eqRead() (*qcycore.EqSnapshot, error)
	// eqApply reads the raw 0x22 payload, patches only what op names, saves the
	// pre-write payload as a backup (aborting the write if that fails), writes
	// and confirms by readback.
	eqApply(op qcycore.EqOp) (*qcycore.EqResult, error)
	// wearSet reads 0x2C, sets byte 0 to state (1 on / 2 off), writes, confirms.
	wearSet(state byte) ([]byte, error)
	// windOffByte is a diagnostic: writes wind detection "off" (0x2A) with
	// the given byte (0 or 2), accepts 0x00 or 0x02 on readback and returns
	// the raw payload read.
	windOffByte(b byte) ([]byte, error)
	// wearOffByte is the same diagnostic for wear (byte 0 of 0x2C): writes
	// exactly b, where wearSet(2) lets the daemon pick the model's off byte.
	wearOffByte(b byte) ([]byte, error)
	// noiseRead returns the raw 0x17 payload.
	noiseRead() ([]byte, error)
	// fitTest starts the fit test, waits up to seconds for the final result and
	// always sends the stop frame at the end. fitStop sends the stop frame (and
	// cancels a running test).
	fitTest(seconds int) (fitResult, error)
	fitStop() error
}

// runViaDaemon tries the daemon first; on failure falls back to direct mode.
func runViaDaemon(scanSeconds int, fn func(daemonAPI) error) error {
	if err := refuseHardware(); err != nil {
		return err
	}
	dc, err := dialDaemonStart(500 * time.Millisecond)
	if err != nil {
		// Daemon not reachable (and could not be started) → direct mode
		return runClient(scanSeconds, func(c *client) error {
			return fn(&daemonClientWrapper{client: c})
		})
	}
	defer dc.Close()
	return fn(dc)
}

// daemonClientWrapper adapts *client to the daemonClient read interface.
type daemonClientWrapper struct {
	*client
}

func (w *daemonClientWrapper) status() (map[string]interface{}, error) {
	rep := gatherStatus(w.client)
	return structToMap(rep)
}

func (w *daemonClientWrapper) battery() (map[string]interface{}, error) {
	left, right, box, lc, rc, cc, err := readBattery(w.client.q)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"left": left, "right": right, "case": box,
		"leftCharging": lc, "rightCharging": rc, "caseCharging": cc,
		"casePresent": box != 0x00 && box != 0x7f,
	}, nil
}

func (w *daemonClientWrapper) setAction(action string, params []byte) error {
	opcode, err := opcodeFor(action)
	if err != nil {
		return err
	}
	w.drain()
	if err := w.send(opcode, params); err != nil {
		return err
	}
	_, _ = w.wait(opcode, 3*time.Second)
	return nil
}

func (w *daemonClientWrapper) find() error {
	return w.findTone(0x01)
}

// findTone starts the indicator blink (0x05). In direct mode there is no daemon
// to auto-stop it, so the caller is expected to run `qcyctl find off` if the
// blink should end early.
func (w *daemonClientWrapper) findTone(arg byte) error {
	opcode, err := opcodeFor("find")
	if err != nil {
		return err
	}
	w.drain()
	if err := w.send(opcode, []byte{arg}); err != nil {
		return err
	}
	_, _ = w.wait(opcode, 250*time.Millisecond)
	return nil
}

func (w *daemonClientWrapper) setActionExpect(action string, params, expect []byte) ([]byte, error) {
	opcode, err := opcodeFor(action)
	if err != nil {
		return nil, err
	}
	// setConfirm writes, then keeps re-querying until the device reports the
	// wanted value — these earbuds answer with the pre-change state first.
	got, ok, err := w.setConfirm(opcode, params, 3*time.Second, func(p []byte) bool {
		return bytes.HasPrefix(p, expect)
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("readback mismatch for %s: want %x got %x (the device may nevertheless have applied the value; re-read to check)", action, expect, got)
	}
	return got, nil
}

// opcodeFor maps a CLI action name to its opcode, refusing the ones that are
// known-bad on this hardware. The daemon carries the same guard.
func opcodeFor(action string) (byte, error) {
	// 0x11 (fit) starts/stops the ear-tip fit check, verified on the test device
	// (an N70, 2026-10-13); find maps to the indicator blink, see the daemon.
	// ldac (0x23) restarts the earbuds and is writable on verified models
	// only. Direct mode cannot tell the model, so it leaves the write to the
	// daemon, which can.
	if action == "ldac" {
		return 0, errors.New("ldac needs the daemon (qcyctld): it is only written on a verified model, and direct mode cannot check the model")
	}
	// 0x22 is only ever written through eqApply, which reads, patches and backs
	// the payload up first. A raw write here would bypass all of that.
	if action == "eq" {
		return 0, errors.New("eq cannot be written as a raw action: use `qcyctl eq set` or `qcyctl master-gain`, which back up the slot first")
	}
	// promptVol (0x1D): frame FF 04 1D 02 <v> 00 (two data bytes, v in
	// 4/6/8/10), verified on the test device (N70), 2026-10-13. A one-byte write was
	// ignored. See PROTOCOL.md, "Prompt volume".
	if action == "promptVol" && !promptVolWritable {
		return 0, errors.New("prompt-vol is read-only on this device: promptVolWritable is false")
	}
	opcode, ok := protocol.ActionOpcodes[action]
	if !ok {
		return 0, fmt.Errorf("unknown action: %s", action)
	}
	return opcode, nil
}

func (w *daemonClientWrapper) eqRead() (*qcycore.EqSnapshot, error) {
	p, err := w.query(qcycore.EqOpcode, 3*time.Second)
	if err != nil {
		return nil, err
	}
	snap, err := qcycore.DecodeEq(p)
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

func (w *daemonClientWrapper) eqApply(op qcycore.EqOp) (*qcycore.EqResult, error) {
	return eqApplyVia(w.client, op)
}

// eqLink is the part of *client eqApplyVia needs; a test substitutes a fake.
type eqLink interface {
	query(opcode byte, timeout time.Duration) ([]byte, error)
	setConfirm(opcode byte, params []byte, timeout time.Duration, match func([]byte) bool) ([]byte, bool, error)
}

// eqApplyVia is the direct-mode 0x22 write: read, ApplyEq (which enforces the
// protected-slot and write-length rules), backup, write, confirm.
func eqApplyVia(l eqLink, op qcycore.EqOp) (*qcycore.EqResult, error) {
	cur, err := l.query(qcycore.EqOpcode, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("eq: read current payload: %w", err)
	}
	next, err := qcycore.ApplyEq(cur, op)
	if err != nil {
		return nil, err
	}
	// Backup first, and refuse to write if it did not land: the payload read
	// above is the only copy of whatever is on the device now.
	bk, err := qcycore.SaveEqBackup(cur)
	if err != nil {
		return nil, fmt.Errorf("eq: backup failed, nothing was written: %w", err)
	}
	got, ok, err := l.setConfirm(qcycore.EqOpcode, next, 4*time.Second, qcycore.EqMatch(next, op))
	if err != nil {
		return nil, fmt.Errorf("eq: write: %w (previous payload saved at %s)", err, bk.History)
	}
	if !ok {
		return nil, qcycore.EqWriteFailure(next, got, bk.History)
	}
	snap, err := qcycore.DecodeEq(got)
	if err != nil {
		return nil, err
	}
	return &qcycore.EqResult{
		EqSnapshot:    snap,
		FullLength:    qcycore.FullLength(next),
		Previous:      hex.EncodeToString(cur),
		BackupFirst:   bk.First,
		BackupHistory: bk.History,
	}, nil
}

func (w *daemonClientWrapper) wearSet(state byte) ([]byte, error) {
	cur, err := w.query(protocol.OpWear, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("wear: read current: %w", err)
	}
	if len(cur) < 3 {
		return nil, fmt.Errorf("wear: short current response %x", cur)
	}
	if qcycore.IsAllFF(cur) {
		return nil, fmt.Errorf("wear: device returned an all-0xFF payload; nothing was written")
	}
	set := append([]byte{state}, cur[1:]...)
	got, ok, err := w.setConfirm(protocol.OpWear, set, 3*time.Second, func(p []byte) bool {
		return len(p) >= len(set) && wearByteMatches(state, p[0]) && bytes.Equal(p[1:len(set)], set[1:])
	})
	if err != nil {
		return nil, err
	}
	if !ok || len(got) < 1 || !wearByteMatches(state, got[0]) {
		return nil, fmt.Errorf("wear readback mismatch: want 0x%02x got %x", state, got)
	}
	return got, nil
}

// errOffByteZeroDirect: byte 0 is only written on a verified model, which
// direct mode cannot tell (same rule as ldac in opcodeFor).
var errOffByteZeroDirect = errors.New("--off-byte 0 needs the daemon (qcyctld): it is only written on a verified model, and direct mode cannot check the model")

func (w *daemonClientWrapper) wearOffByte(b byte) ([]byte, error) {
	if b == 0 {
		return nil, errOffByteZeroDirect
	}
	return w.wearSet(b)
}

func (w *daemonClientWrapper) windOffByte(b byte) ([]byte, error) {
	if b == 0 {
		return nil, errOffByteZeroDirect
	}
	got, ok, err := w.setConfirm(protocol.OpWind, []byte{b}, 3*time.Second, func(p []byte) bool {
		return len(p) >= 1 && (p[0] == 0 || p[0] == 2)
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("wind: readback mismatch: want 00 or 02, got %x", got)
	}
	return got, nil
}

func (w *daemonClientWrapper) fitTest(seconds int) (fitResult, error) {
	defer func() { _ = w.send(protocol.OpFit, fitStopParams) }()
	w.drain()
	if err := w.send(protocol.OpFit, fitStartParams); err != nil {
		return fitResult{}, err
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		p, err := w.wait(protocol.OpFit, time.Until(deadline))
		if err != nil {
			break
		}
		if len(p) >= 3 && p[0] == 2 {
			return fitResult{Status: 2, Left: int(p[1]), Right: int(p[2])}, nil
		}
	}
	return fitResult{}, fmt.Errorf("fit: no final result within %d s", seconds)
}

func (w *daemonClientWrapper) fitStop() error {
	return w.send(protocol.OpFit, fitStopParams)
}

func (w *daemonClientWrapper) noiseRead() ([]byte, error) {
	return w.query(protocol.OpANC, 3*time.Second)
}

func (w *daemonClientWrapper) keys() ([]byte, error) {
	if w.q.keys == nil {
		return nil, errors.New("keys characteristic 000D not available on this device")
	}
	var buf [64]byte
	n, err := w.q.keys.Read(buf[:])
	if err != nil {
		return nil, fmt.Errorf("read keys 000D: %w", err)
	}
	return append([]byte(nil), buf[:n]...), nil
}

func (w *daemonClientWrapper) keysSet(raw []byte) error {
	if w.q.keys == nil {
		return errors.New("keys characteristic 000D not available on this device")
	}
	if len(raw) == 0 || len(raw)%2 != 0 {
		return fmt.Errorf("keys set: payload must be non-empty keyId:funId pairs, got %d bytes", len(raw))
	}
	if _, err := w.q.keys.WriteWithoutResponse(raw); err != nil {
		return fmt.Errorf("keys set write 000D: %w", err)
	}
	time.Sleep(150 * time.Millisecond)
	got, err := w.keys()
	if err != nil {
		return err
	}
	if !protocol.KeysConfirmed(raw, got) {
		return fmt.Errorf("keys set readback mismatch: want %x got %x", raw, got)
	}
	return nil
}

func structToMap(v interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	return m, json.Unmarshal(b, &m)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "scan":
		err = cmdScan(args)
	case "info":
		err = cmdInfo(args)
	case "status":
		err = cmdStatus(args)
	case "battery":
		err = cmdBattery(args)
	case "noise":
		err = cmdNoise(args)
	case "fit":
		err = cmdFit(args)
	case "spatial":
		err = cmdSpatial(args)
	case "anc":
		err = cmdAnc(args)
	case "game", "sleep", "ldac", "multipoint", "wind":
		err = cmdToggle(cmd, args)
	case "wear":
		err = cmdWear(args)
	case "wear-sensitivity":
		err = cmdWearSensitivity(args)
	case "find":
		err = cmdFind(args)
	case "prompt-vol":
		err = cmdPromptVol(args)
	case "auto-off":
		err = cmdAutoOff(args)
	case "balance":
		err = cmdBalance(args)
	case "eq":
		err = cmdEq(args)
	case "master-gain":
		err = cmdMasterGain(args)
	case "keys":
		err = cmdKeys(args)
	case "library":
		err = cmdLibrary(args)
	case "i18n":
		err = cmdI18n(args)
	case "model":
		err = cmdModel(args)
	case "hearing":
		err = cmdHearing(args)
	case "fn":
		err = cmdFn(args)
	case "daemon":
		err = cmdDaemon(args)
	case "version", "--version":
		err = cmdVersion(args)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "qcyctl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  qcyctl scan [seconds]
  qcyctl info [seconds]
  qcyctl status [--json] [seconds]
  qcyctl battery [--json] [seconds]
  qcyctl noise [seconds]
  qcyctl anc <off|adaptive|indoor|commuting|noisy|antiwind|transparency> [seconds]  (anc = adaptive)
  qcyctl <game|sleep|multipoint|wind> <on|off> [seconds]
  qcyctl ldac <on|off> --yes [seconds]   (restarts the earbuds; verified models only; not yet verified on hardware)
  qcyctl wind off --off-byte <0|2> --yes [seconds]   (DIAGNOSTIC: which byte means "off"; default 0 on verified models, 2 otherwise)
  qcyctl wear <on|off> [seconds]
  qcyctl wear off --off-byte <0|2> --yes [seconds]   (DIAGNOSTIC: byte 0 of 0x2C; default 0 on verified models, 2 otherwise)
  qcyctl find [--sink NAME] [--start-volume 100%] [--tone-level -3] [--max-volume 100%] [--timeout 60] [--yes] [--events]
      plays a beeping tone on the earbuds' audio sink, raising the volume gently (no BLE command)
  qcyctl prompt-vol <4|6|8|10|low|medium|high|max> [seconds]
  qcyctl wear-sensitivity <medium|normal|high|1|2|3> [seconds]
  qcyctl fit <start|stop|begin> [--json] [seconds]   (start waits for the result; frames verified on the test device)
  qcyctl auto-off <minutes|never> [seconds]
  qcyctl balance <left|center|right|0-100> [seconds]
  qcyctl eq get [--json] [seconds]
  qcyctl eq set <g1,g2,...,g10> --yes [--preset N] [--master-gain dB] [--allow-protected] [--full-length] [--json] [seconds]
        (gains -12..+12 dB, master -12..+12; patches the live payload, keeps freq/Q/type; backs the slot up first;
         slots 6 and 11 are protected: --preset 6|11, or any --preset while one of them is active, needs --allow-protected;
         a payload with bands at positions 10-19 needs a 143-byte write, which needs --full-length)
  qcyctl eq preset <n> [seconds]   (unsupported on models without characteristic 000B)
  qcyctl master-gain <dB> [--full-length] [--json] [seconds]
        (-12..+12; only the master gain changes; --full-length is needed when the active slot has bands 10-19, e.g. slot 11)
  qcyctl library <list|show|save|delete|export|import|seed|import-device|apply|compose> ...   (curve library; see README)
  qcyctl i18n <list|bundle|template|set> ...   (plugin languages: list [--json], bundle [<lang>] [--json], template, set <lang|auto>; offline)
  qcyctl model image [--vendor N] [--color <name|index>] [--refresh] [--no-network] [--json]   (the earbuds' images, downloaded once from the QCY CDN into the cache)
  qcyctl model images <on|off|status> [--json]   (allow or forbid those downloads)
  qcyctl hearing <plan|result|run --dry-run>   (hearing test phase 1: offline plan, curve, master gain and frame bytes; sends nothing)
  qcyctl fn list [--json]   (generic functions the model's panel declares and no other command covers; needs the daemon)
  qcyctl fn set <cmdId> <value|on|off> [--yes]   (writes the panel's own bytes; a restart warning needs --yes, a reset-type command --yes --yes)
  qcyctl daemon <start|stop|status [--json]|install-service [--json]|uninstall-service [--json]>
      the daemon (qcyctld) is started on demand by commands that need it (QCYCTL_NO_AUTOSTART=1 disables that);
      install-service keeps it under systemd --user instead
  qcyctl version [--json]
  qcyctl keys get [seconds]
  qcyctl keys set <keyId:funId> [<keyId:funId> ...] [seconds]
All setters read back by querying the opcode (FF 03 FE 01 <opcode>) and fail non-zero on mismatch.`)
}

func parseSeconds(args []string, def int) (int, error) {
	if len(args) == 0 {
		return def, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid seconds %q", args[0])
	}
	return n, nil
}

// wearByteMatches: a read-back byte 0 of 0x2C confirms a write of state. 0x00
// and 0x02 both mean "off" (the app writes 0x00, this tool 0x02).
func wearByteMatches(state, got byte) bool {
	if state == 0 || state == 2 {
		return got == 0 || got == 2
	}
	return got == state
}

// diagArgs are the flags of the on/off commands that take --yes and, for
// wear/wind, the diagnostic --off-byte.
type diagArgs struct {
	rest    []string
	yes     bool
	offByte int // -1 = not given (default 2)
}

func parseDiagArgs(args []string, allowOffByte bool) (diagArgs, error) {
	out := diagArgs{offByte: -1}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--yes":
			out.yes = true
		case allowOffByte && (a == "--off-byte" || strings.HasPrefix(a, "--off-byte=")):
			v := strings.TrimPrefix(a, "--off-byte=")
			if a == "--off-byte" {
				if i+1 >= len(args) {
					return out, errors.New("--off-byte needs a value (0 or 2)")
				}
				i++
				v = args[i]
			}
			if v != "0" && v != "2" {
				return out, fmt.Errorf("--off-byte: invalid value %q (0 or 2)", v)
			}
			out.offByte = int(v[0] - '0')
		default:
			out.rest = append(out.rest, a)
		}
	}
	return out, nil
}

// cmdRunner opens the link; tests substitute it.
var cmdRunner = runViaDaemon

func parseOnOff(s string) (byte, error) {
	switch s {
	case "on":
		return 0x01, nil
	case "off":
		return 0x02, nil
	}
	return 0, fmt.Errorf("expected on|off, got %q", s)
}

// parseByte accepts decimal or hex (0x prefix or hex letters) key/fun ids.
func parseByte(s string) (byte, error) {
	s = strings.TrimSpace(s)
	base := 10
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		base = 16
		s = s[2:]
	} else if strings.ContainsAny(s, "abcdefABCDEF") {
		base = 16
	}
	v, err := strconv.ParseUint(s, base, 8)
	if err != nil {
		return 0, err
	}
	return byte(v), nil
}

// ---------- framing ----------

func buildFrame(opcode byte, params []byte) []byte {
	return protocol.Block(opcode, params...)
}

type frame struct {
	cmd    byte
	params []byte
}

// parseFrames parses one or more FF-framed blocks (protocol.ParseBlocks).
func parseFrames(p []byte) ([]frame, error) {
	blocks, err := protocol.ParseBlocks(p)
	if len(blocks) == 0 {
		return nil, err
	}
	out := make([]frame, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, frame{cmd: b.Cmd, params: b.Params})
	}
	return out, err
}

func u16le(b []byte, off int) uint16 {
	return uint16(b[off]) | uint16(b[off+1])<<8
}

func le16(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

// ---------- scan ----------

func cmdScan(args []string) error {
	seconds, err := parseSeconds(args, 10)
	if err != nil {
		return err
	}

	if err := refuseHardware(); err != nil {
		return err
	}
	if err := bluetooth.DefaultAdapter.Enable(); err != nil {
		return fmt.Errorf("enable adapter: %w", err)
	}
	scanner := quicky.NewScanner()

	stop := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() {
		stopOnce.Do(func() {
			close(stop)
			if err := scanner.StopScan(); err != nil {
				fmt.Fprintf(os.Stderr, "stop scan: %v\n", err)
			}
		})
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-sigCh:
		}
		requestStop()
	}()

	scanErr := scanner.Scan(func(r quicky.ScanResult) {
		printScanResult(r)
	})

	select {
	case <-stop:
		return nil
	default:
		if scanErr != nil {
			return fmt.Errorf("scan: %w", scanErr)
		}
		return nil
	}
}

func printScanResult(r quicky.ScanResult) {
	fmt.Printf("device %s rssi=%d name=%q\n", r.Address, r.RSSI, r.Name)
	if ad := r.Advertisement; ad != nil {
		fmt.Printf("  vendorID=0x%04x color=%d\n", ad.VendorID, ad.ColorIndex)
		fmt.Printf("  battery left=%d%% charging=%v right=%d%% charging=%v box=%d%% charging=%v\n",
			ad.LeftBattery, ad.IsLeftCharging, ad.RightBattery, ad.IsRightCharging, ad.BoxBattery, ad.IsBoxCharging)
	}
	if p, ok := r.GetProductInfo(); ok {
		fmt.Printf("  product: %s / %s\n", p.Title, p.SubTitle)
	}
}

// ---------- connection ----------

type qcy struct {
	dev      bluetooth.Device
	cmd      bluetooth.DeviceCharacteristic // 1001 write-without-response
	notify   bluetooth.DeviceCharacteristic // 1002 read+notify
	battery  *bluetooth.DeviceCharacteristic
	version  *bluetooth.DeviceCharacteristic
	keys     *bluetooth.DeviceCharacteristic
	eqPreset *bluetooth.DeviceCharacteristic
}

func (q *qcy) Close() {
	if err := q.dev.Disconnect(); err != nil {
		fmt.Fprintf(os.Stderr, "disconnect LE: %v\n", err)
	}
}

// connectQCY reaches the earbuds. Scanning the LE adapter is the slow part
// (up to scanSeconds), so the last address that worked is cached and tried
// first with a short deadline; only if that fails do we fall back to a scan.
func connectQCY(scanSeconds int) (*qcy, error) {
	if err := refuseHardware(); err != nil {
		return nil, err
	}
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable adapter: %w", err)
	}

	// Fast path: reconnect straight to the address that worked last time.
	if cached, ok := loadCachedAddr(); ok {
		if dev, err := dialLE(adapter, cached, 2*time.Second); err == nil {
			if q, err := buildQCY(dev); err == nil {
				return q, nil
			}
			_ = dev.Disconnect()
		}
		dbg("cached address %s unreachable, scanning\n", cached)
	}

	scanner := quicky.NewScanner()
	addrCh := make(chan bluetooth.Address, 1)
	scanErrCh := make(chan error, 1)
	go func() {
		scanErrCh <- scanner.Scan(func(r quicky.ScanResult) {
			select {
			case addrCh <- r.Address:
			default:
			}
		})
	}()

	var addr bluetooth.Address
	found := false
	timer := time.NewTimer(time.Duration(scanSeconds) * time.Second)
	defer timer.Stop()
	select {
	case addr = <-addrCh:
		found = true
	case err := <-scanErrCh:
		return nil, fmt.Errorf("scan: %w", err)
	case <-timer.C:
	}
	if !found {
		_ = scanner.StopScan()
		return nil, errors.New("no QCY device found during scan")
	}

	var dev bluetooth.Device
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		dev, err = adapter.Connect(addr, bluetooth.ConnectionParams{})
		if err == nil {
			break
		}
		dbg("connect attempt %d to %s failed: %v\n", attempt, addr, err)
		select {
		case addr = <-addrCh:
		default:
		}
		time.Sleep(700 * time.Millisecond)
	}
	_ = scanner.StopScan()
	if err != nil {
		return nil, fmt.Errorf("connect LE %s: %w", addr, err)
	}

	q, err := buildQCY(dev)
	if err != nil {
		return nil, err
	}
	saveCachedAddr(addr)
	return q, nil
}

// dialLE connects to a specific address, giving up after timeout so a stale or
// out-of-range address cannot hang the caller.
func dialLE(adapter *bluetooth.Adapter, addr bluetooth.Address, timeout time.Duration) (bluetooth.Device, error) {
	type res struct {
		dev bluetooth.Device
		err error
	}
	ch := make(chan res, 1)
	go func() {
		dev, err := adapter.Connect(addr, bluetooth.ConnectionParams{})
		ch <- res{dev, err}
	}()
	select {
	case r := <-ch:
		return r.dev, r.err
	case <-time.After(timeout):
		return bluetooth.Device{}, errors.New("connect timed out")
	}
}

// buildQCY discovers the QCY service and its characteristics on a device that
// is already connected.
func buildQCY(dev bluetooth.Device) (*qcy, error) {
	services, err := dev.DiscoverServices([]bluetooth.UUID{svcUUID})
	if err != nil {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("discover service a001: %w", err)
	}
	if len(services) == 0 {
		_ = dev.Disconnect()
		return nil, errors.New("QCY service a001 not found on LE device")
	}

	q := &qcy{dev: dev}
	req, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{cmdUUID, notifyUUID})
	if err != nil {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("required characteristics 1001/1002: %w", err)
	}
	q.cmd = req[0]
	q.notify = req[1]

	q.battery = findChar(services[0], battUUID)
	q.version = findChar(services[0], verUUID)
	q.keys = findChar(services[0], keysUUID)
	q.eqPreset = findChar(services[0], eqPresetUUID)
	return q, nil
}

func cacheFilePath() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "qcyctl", "le-address")
}

func loadCachedAddr() (bluetooth.Address, bool) {
	p := cacheFilePath()
	if p == "" {
		return bluetooth.Address{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return bluetooth.Address{}, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return bluetooth.Address{}, false
	}
	var addr bluetooth.Address
	addr.Set(fields[0])
	if len(fields) > 1 && fields[1] == "1" {
		addr.SetRandom(true)
	}
	if addr.MAC == (bluetooth.MAC{}) {
		return bluetooth.Address{}, false
	}
	return addr, true
}

func saveCachedAddr(addr bluetooth.Address) {
	p := cacheFilePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	random := "0"
	if addr.IsRandom() {
		random = "1"
	}
	_ = os.WriteFile(p, []byte(addr.String()+" "+random+"\n"), 0o644)
}

func findChar(s bluetooth.DeviceService, uuid bluetooth.UUID) *bluetooth.DeviceCharacteristic {
	c, err := s.DiscoverCharacteristics([]bluetooth.UUID{uuid})
	if err != nil || len(c) != 1 {
		return nil
	}
	return &c[0]
}

// ---------- command/response client ----------

type client struct {
	q      *qcy
	frames chan frame
}

func runClient(scanSeconds int, fn func(*client) error) error {
	q, err := connectQCY(scanSeconds)
	if err != nil {
		return err
	}
	defer q.Close()

	c := &client{q: q, frames: make(chan frame, 64)}
	if err := c.start(); err != nil {
		return err
	}
	return fn(c)
}

func (c *client) start() error {
	err := c.q.notify.EnableNotifications(func(buf []byte) {
		dbg("notify[1002] raw=%x\n", buf)
		frames, perr := parseFrames(buf)
		if perr != nil {
			dbg("  (unparsed: %v)\n", perr)
			return
		}
		for _, f := range frames {
			dbg("  frame cmd=0x%02x params=%x\n", f.cmd, f.params)
			cp := frame{cmd: f.cmd, params: append([]byte(nil), f.params...)}
			select {
			case c.frames <- cp:
			default:
			}
		}
	})
	if err != nil {
		return fmt.Errorf("enable notifications 1002: %w", err)
	}
	dbg("subscribed notify[1002]\n")
	if c.q.battery != nil {
		if err := c.q.battery.EnableNotifications(func(buf []byte) {
			dbg("notify[0008] raw=%x\n", buf)
		}); err != nil {
			dbg("subscribe battery 0008: %v\n", err)
		} else {
			dbg("subscribed notify[0008]\n")
		}
	}
	return nil
}

func (c *client) drain() {
	for {
		select {
		case <-c.frames:
		default:
			return
		}
	}
}

// send writes a frame to 1001 using write-without-response (the only supported
// write type on that characteristic), falling back to with-response on error.
func (c *client) send(opcode byte, params []byte) error {
	pkt := buildFrame(opcode, params)
	n, err := c.q.cmd.WriteWithoutResponse(pkt)
	dbg("tx %x (without-response n=%d err=%v)\n", pkt, n, err)
	if err == nil {
		return nil
	}
	n2, err2 := c.q.cmd.Write(pkt)
	dbg("tx %x (with-response n=%d err=%v)\n", pkt, n2, err2)
	if err2 != nil {
		return fmt.Errorf("write 0x%02x: without-response=%v with-response=%v", opcode, err, err2)
	}
	return nil
}

func (c *client) wait(opcode byte, timeout time.Duration) ([]byte, error) {
	deadline := time.After(timeout)
	for {
		select {
		case f := <-c.frames:
			if f.cmd == opcode {
				return f.params, nil
			}
		case <-deadline:
			return nil, fmt.Errorf("no response for opcode 0x%02x within %s", opcode, timeout)
		}
	}
}

// query sends FF 03 FE 01 <opcode> and waits for the opcode response.
func (c *client) query(opcode byte, timeout time.Duration) ([]byte, error) {
	c.drain()
	if err := c.send(protocol.OpQuery, []byte{opcode}); err != nil {
		return nil, err
	}
	return c.wait(opcode, timeout)
}

// setConfirm writes a setter frame then readback-confirms. The device often
// emits the pre-change state first (as the immediate query reply) and the
// post-change state afterwards (as an unsolicited notification); so we keep
// reading frames for the opcode, re-querying periodically, until one matches.
func (c *client) setConfirm(opcode byte, params []byte, timeout time.Duration, match func([]byte) bool) ([]byte, bool, error) {
	c.drain()
	if err := c.send(opcode, params); err != nil {
		return nil, false, err
	}
	deadline := time.After(timeout)
	requery := time.After(500 * time.Millisecond)
	queries := 0
	var last []byte
	for {
		select {
		case f := <-c.frames:
			if f.cmd != opcode {
				continue
			}
			last = f.params
			if match(f.params) {
				return f.params, true, nil
			}
		case <-requery:
			if queries < 5 {
				_ = c.send(protocol.OpQuery, []byte{opcode})
				queries++
				requery = time.After(500 * time.Millisecond)
			}
		case <-deadline:
			if last == nil {
				return nil, false, fmt.Errorf("no response for opcode 0x%02x within %s", opcode, timeout)
			}
			return last, false, nil
		}
	}
}

// ---------- info / status ----------

func cmdInfo(args []string) error {
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	q, err := connectQCY(scanSeconds)
	if err != nil {
		return err
	}
	defer q.Close()

	printBattery(q)
	printVersion(q)
	return nil
}

// readBattery reads characteristic 0008 and returns the parsed L/R/case levels.
func readBattery(q *qcy) (left, right, box byte, lc, rc, cc bool, err error) {
	if q.battery == nil {
		return 0, 0, 0, false, false, false, errors.New("battery characteristic 0008 not available on this device")
	}
	var buf [4]byte
	n, err := q.battery.Read(buf[:])
	if err != nil {
		return 0, 0, 0, false, false, false, fmt.Errorf("read battery 0008: %w", err)
	}
	return parseBattery(buf[:n])
}

// readVersion reads characteristic 0007 and returns the left/right firmware.
func readVersion(q *qcy) (string, string, error) {
	if q.version == nil {
		return "", "", errors.New("version characteristic 0007 not available on this device")
	}
	var buf [8]byte
	n, err := q.version.Read(buf[:])
	if err != nil {
		return "", "", fmt.Errorf("read version 0007: %w", err)
	}
	return parseVersion(buf[:n])
}

func printBattery(q *qcy) {
	left, right, box, lc, rc, cc, err := readBattery(q)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Printf("battery left=%d%% charging=%v right=%d%% charging=%v box=%d%% charging=%v\n",
		left, lc, right, rc, box, cc)
}

func printVersion(q *qcy) {
	l, r, err := readVersion(q)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Printf("version left=%s right=%s\n", l, r)
}

// statusReport is the machine-readable snapshot of the earbuds. Every field is
// optional: a query that fails is recorded in Errors and left out, so a partial
// snapshot is still emitted.
type statusReport struct {
	Address   string          `json:"address,omitempty"`
	Firmware  *fwReport       `json:"firmware,omitempty"`
	Battery   *batteryReport  `json:"battery,omitempty"`
	Noise     *noiseReport    `json:"noise,omitempty"`
	Toggles   map[string]bool `json:"toggles"`
	PromptVol *promptReport   `json:"promptVolume,omitempty"`
	AutoOff   *autoOffReport  `json:"autoOff,omitempty"`
	Wear      *wearReport     `json:"wear,omitempty"`
	Balance   *int            `json:"balance,omitempty"`
	// WearSensitivity is 1 medium, 2 normal, 3 high; absent when unanswered.
	WearSensitivity *int           `json:"wearSensitivity,omitempty"`
	EQ              *eqReport      `json:"eq,omitempty"`
	Keys            *keysReport    `json:"keys,omitempty"`
	Spatial         *spatialReport `json:"spatial,omitempty"`
	// Caps lists which optional commands this unit answered, so the panel can
	// show only what the connected earbuds actually have.
	Caps map[string]bool `json:"capabilities,omitempty"`
	// Model is the daemon's model detection: modelId, displayName, verified,
	// and vendorId/productTitle when known.
	Model map[string]interface{} `json:"model,omitempty"`
	// Functions is the daemon's reading of the generic panel functions
	// (cmdId -> "on", "off", an option value or "raw:<hex>"); see `fn list`.
	Functions map[string]string `json:"functions,omitempty"`
	Errors    []string          `json:"errors,omitempty"`
	// Stale is set when the daemon served its last cached snapshot because the
	// earbuds were busy; AgeSeconds is how old that snapshot is.
	Stale      bool `json:"stale,omitempty"`
	AgeSeconds int  `json:"ageSeconds,omitempty"`
}

type fwReport struct {
	Left  string `json:"left"`
	Right string `json:"right"`
}

type batteryReport struct {
	Left          int  `json:"left"`
	Right         int  `json:"right"`
	Case          int  `json:"case"`
	LeftCharging  bool `json:"leftCharging"`
	RightCharging bool `json:"rightCharging"`
	CaseCharging  bool `json:"caseCharging"`
	CasePresent   bool `json:"casePresent"`
}

type noiseReport struct {
	Mode     string `json:"mode"`
	Scene    string `json:"scene"`
	ModeRaw  int    `json:"modeRaw"`
	SceneRaw int    `json:"sceneRaw"`
	Value    int    `json:"value"`
}

type promptReport struct {
	Value int `json:"value"`
	Max   int `json:"max"`
}

// autoOffReport.Minutes is null when auto-off is disabled ("never").
type autoOffReport struct {
	Minutes *int `json:"minutes"`
}

// spatialReport is the spatial-audio toggle (cmdId 0x2D). Name and shape are
// provisional: the device answers the query, but nothing on this hardware has
// confirmed what the byte toggles.
type spatialReport struct {
	Enabled bool `json:"enabled"`
}

type wearReport struct {
	Enabled bool `json:"enabled"`
}

type eqBandReport struct {
	Index int     `json:"index"`
	Freq  int     `json:"freq"`
	Gain  float64 `json:"gain"`
	Q     int     `json:"q"`
	Type  int     `json:"type"`
}

// eqReport is the EQ section of `status --json`.
//
//	preset        active slot (byte 0 of the raw 0x22 payload)
//	effect        effect name of that slot (qcycore.EffectName), omitted when unknown
//	masterGain    master gain in dB (int16 centi-dB / 100), -12..+12
//	bands[]       every band position with freq != 0 (up to 20), in order:
//	              index (0-19), freq in Hz as stored on the device, gain in dB, q, type
type eqReport struct {
	Preset     int            `json:"preset"`
	Effect     string         `json:"effect,omitempty"` // measured effect name of the slot, when known
	MasterGain float64        `json:"masterGain"`
	Bands      []eqBandReport `json:"bands"`
}

// eqReportFrom decodes a raw 0x22 payload into the status schema.
func eqReportFrom(p []byte) (*eqReport, error) {
	s, err := qcycore.DecodeEq(p)
	if err != nil {
		return nil, err
	}
	eq := &eqReport{Preset: s.Slot, Effect: s.Effect, MasterGain: s.MasterGain}
	for _, b := range s.Bands {
		eq.Bands = append(eq.Bands, eqBandReport{Index: b.Index, Freq: b.Freq, Gain: b.Gain, Q: b.Q, Type: b.Type})
	}
	return eq, nil
}

type keysReport struct {
	Raw   string   `json:"raw"`
	Pairs []string `json:"pairs"`
}

func cmdStatus(args []string) error {
	jsonOut := false
	var rest []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	scanSeconds, err := parseSeconds(rest, 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		data, err := dc.status()
		if err != nil {
			return err
		}
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(data)
		}
		// Human format from map
		printStatusHumanFromMap(data)
		return nil
	})
}

// cmdBattery reads only the battery characteristic, not the full query chain.
// It exists so the panel can poll battery live while open without paying for a
// full status read.
func cmdBattery(args []string) error {
	jsonOut := false
	var rest []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	scanSeconds, err := parseSeconds(rest, 15)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		data, err := dc.battery()
		if err != nil {
			return err
		}
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(data)
		}
		fmt.Printf("battery left=%d%% charging=%v right=%d%% charging=%v box=%d%% charging=%v\n",
			data["left"], data["leftCharging"], data["right"], data["rightCharging"], data["case"], data["caseCharging"])
		return nil
	})
}

// gatherStatus queries every readable opcode once and assembles a snapshot.
// It never writes anything to the device.
func gatherStatus(c *client) *statusReport {
	q := c.q
	rep := &statusReport{Toggles: map[string]bool{}}
	addErr := func(err error) {
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		}
	}

	if left, right, box, lc, rc, cc, err := readBattery(q); err != nil {
		addErr(err)
	} else {
		rep.Battery = &batteryReport{
			Left: int(left), Right: int(right), Case: int(box),
			LeftCharging: lc, RightCharging: rc, CaseCharging: cc,
			CasePresent: box != 0x00 && box != 0x7f,
		}
	}
	if l, r, err := readVersion(q); err != nil {
		addErr(err)
	} else {
		rep.Firmware = &fwReport{Left: l, Right: r}
	}

	q16 := func(op byte) ([]byte, error) { return c.query(op, 1500*time.Millisecond) }

	if p, err := q16(protocol.OpANC); err != nil {
		addErr(fmt.Errorf("anc: %w", err))
	} else if len(p) >= 3 {
		rep.Noise = &noiseReport{
			Mode: noiseModeName(p[0]), Scene: noiseSceneName(p[0], p[1]),
			ModeRaw: int(p[0]), SceneRaw: int(p[1]), Value: int(p[2]),
		}
	}

	for _, name := range []string{"game", "sleep", "ldac", "multipoint", "wind"} {
		p, err := q16(toggles[name])
		if err != nil {
			addErr(fmt.Errorf("%s: %w", name, err))
			continue
		}
		rep.Toggles[name] = len(p) >= 1 && p[0] == 0x01
	}

	if p, err := q16(protocol.OpPromptVolume); err != nil {
		addErr(fmt.Errorf("prompt-vol: %w", err))
	} else if len(p) >= 1 {
		pr := &promptReport{Value: int(p[0])}
		if len(p) >= 2 {
			pr.Max = int(p[1])
		}
		rep.PromptVol = pr
	}

	if p, err := q16(protocol.OpAutoOff); err != nil {
		addErr(fmt.Errorf("auto-off: %w", err))
	} else if len(p) >= 2 {
		ao := &autoOffReport{}
		if v := u16le(p, 0); v != 0xFFFF {
			m := int(v)
			ao.Minutes = &m
		}
		rep.AutoOff = ao
	}

	if p, err := q16(protocol.OpWear); err != nil {
		addErr(fmt.Errorf("wear: %w", err))
	} else if len(p) >= 1 {
		rep.Wear = &wearReport{Enabled: p[0] == 0x01}
	}

	if p, err := q16(protocol.OpBalance); err != nil {
		addErr(fmt.Errorf("balance: %w", err))
	} else if len(p) >= 1 {
		b := int(p[0])
		rep.Balance = &b
	}

	if p, err := q16(protocol.OpEQ); err != nil {
		addErr(fmt.Errorf("eq: %w", err))
	} else if eq, perr := eqReportFrom(p); perr != nil {
		addErr(fmt.Errorf("eq: %w", perr))
	} else {
		rep.EQ = eq
	}

	if p, err := q16(protocol.OpSpatial); err != nil {
		addErr(fmt.Errorf("spatial: %w", err))
	} else if len(p) >= 1 {
		rep.Spatial = &spatialReport{Enabled: p[0] == 0x01}
	}

	if q.keys != nil {
		var buf [64]byte
		if n, err := q.keys.Read(buf[:]); err != nil {
			addErr(fmt.Errorf("keys: %w", err))
		} else {
			k := &keysReport{Raw: fmt.Sprintf("%x", buf[:n])}
			for i := 0; i+1 < n; i += 2 {
				k.Pairs = append(k.Pairs, fmt.Sprintf("%02x:%02x", buf[i], buf[i+1]))
			}
			rep.Keys = k
		}
	}

	return rep
}

func printStatusHuman(rep *statusReport) {
	fmt.Println("=== status ===")
	if v := rep.Firmware; v != nil {
		fmt.Printf("%-18s left=%s right=%s\n", "version", v.Left, v.Right)
	}
	if b := rep.Battery; b != nil {
		fmt.Printf("%-18s left=%d%% charging=%v right=%d%% charging=%v box=%d%% charging=%v\n",
			"battery", b.Left, b.LeftCharging, b.Right, b.RightCharging, b.Case, b.CaseCharging)
	}
	if n := rep.Noise; n != nil {
		fmt.Printf("%-18s mode=0x%02x(%s) scene=0x%02x(%s) value=0x%02x\n",
			"anc (0x17)", n.ModeRaw, n.Mode, n.SceneRaw, n.Scene, n.Value)
	}
	for _, name := range []string{"game", "sleep", "ldac", "multipoint", "wind"} {
		v, ok := rep.Toggles[name]
		if !ok {
			fmt.Printf("%-18s <no response>\n", fmt.Sprintf("%s (0x%02x)", name, toggles[name]))
			continue
		}
		fmt.Printf("%-18s %s\n", fmt.Sprintf("%s (0x%02x)", name, toggles[name]), boolName(v))
	}
	if p := rep.PromptVol; p != nil {
		fmt.Printf("%-18s value=%d max=%d\n", "prompt-vol (0x1d)", p.Value, p.Max)
	}
	if a := rep.AutoOff; a != nil {
		if a.Minutes == nil {
			fmt.Printf("%-18s never\n", "auto-off (0x14)")
		} else {
			fmt.Printf("%-18s %dmin\n", "auto-off (0x14)", *a.Minutes)
		}
	}
	if w := rep.Wear; w != nil {
		fmt.Printf("%-18s %s\n", "wear (0x2c)", boolName(w.Enabled))
	}
	if b := rep.Balance; b != nil {
		fmt.Printf("%-18s value=%d\n", "balance (0x16)", *b)
	}
	if eq := rep.EQ; eq != nil {
		var parts []string
		for _, b := range eq.Bands {
			parts = append(parts, fmt.Sprintf("%dHz:%.2fdB", b.Freq, b.Gain))
		}
		fmt.Printf("%-18s preset=%d masterGain=%+.2fdB bands=%s\n", "eq (0x22)", eq.Preset, eq.MasterGain, strings.Join(parts, ","))
	}
	if k := rep.Keys; k != nil {
		fmt.Printf("%-18s raw=%s\n", "keys (0x0d)", k.Raw)
	}
	for _, e := range rep.Errors {
		fmt.Fprintf(os.Stderr, "warning: %s\n", e)
	}
}

func boolName(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// printStatusHumanFromMap renders the map produced by status() (the same schema
// as statusReport) through the typed printer, so both paths print alike.
func printStatusHumanFromMap(data map[string]interface{}) {
	b, err := json.Marshal(data)
	if err == nil {
		var rep statusReport
		if err = json.Unmarshal(b, &rep); err == nil {
			printStatusHuman(&rep)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "status: cannot render: %v\n", err)
}

func printQuery(label string, q func(byte) ([]byte, error), opcode byte, fmtFn func([]byte) string) {
	p, err := q(opcode)
	if err != nil {
		fmt.Printf("%-18s <no response: %v>\n", label, err)
		return
	}
	fmt.Printf("%-18s %s\n", label, fmtFn(p))
}

func onOffName(v byte) string {
	switch v {
	case 0x01:
		return "on(0x01)"
	case 0x02:
		return "off(0x02)"
	case 0x00:
		return "off(0x00)"
	}
	return fmt.Sprintf("0x%02x", v)
}

// onOffConfirmed reports whether a readback matches the requested on/off.
// The device reports "on" as 0x01, but reports "off" as either 0x02 or 0x00
// depending on the opcode (e.g. 0x24 multipoint reports off as 0x00).
func onOffConfirmed(state byte, got []byte) bool {
	if len(got) < 1 {
		return false
	}
	if state == 0x01 {
		return got[0] == 0x01
	}
	return got[0] == 0x02 || got[0] == 0x00
}

func minutesName(v uint16) string {
	if v == 0xFFFF {
		return "never"
	}
	return fmt.Sprintf("%dmin", v)
}

// ---------- ANC / noise ----------

func cmdAnc(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl anc <off|adaptive|indoor|commuting|noisy|antiwind|transparency> [--level 0-2] [seconds]")
	}
	modeName := args[0]
	params, ok := ancModes[modeName]
	if !ok {
		return fmt.Errorf("anc: unknown mode %q", modeName)
	}

	// The level is taken behind an explicit flag rather than as a bare second
	// argument. Both it and the trailing scan-seconds value are numbers, so a
	// positional form is ambiguous — and the panel always appends scan seconds
	// last, which silently turned the scan time into a level and failed the
	// write with "invalid intensity".
	level := int(params[2])
	rest := args[1:]
	for len(rest) > 0 {
		switch rest[0] {
		case "--level", "-l":
			if len(rest) < 2 {
				return errors.New("anc: --level needs a value (0-2)")
			}
			n, err := strconv.Atoi(rest[1])
			// Measured on this firmware: 0-2 are accepted, anything higher is
			// clamped to 0 by the earbuds. Adaptive and anti-wind ignore the
			// value and always report 0.
			if err != nil || n < 0 || n > 2 {
				return fmt.Errorf("anc: invalid level %q (0-2)", rest[1])
			}
			level = n
			rest = rest[2:]
		default:
			rest = rest[1:]
		}
	}

	withLevel := [3]byte{params[0], params[1], byte(level)}
	scanSeconds, err := parseSeconds(rest, 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		if err := dc.setAction("anc", withLevel[:]); err != nil {
			return err
		}
		fmt.Printf("anc=%s level=%d sent\n", modeName, level)
		return nil
	})
}

func cmdNoise(args []string) error {
	if len(args) > 0 && (args[0] == "off" || isAncMode(args[0])) {
		return cmdAnc(args)
	}
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		p, err := dc.noiseRead()
		if err != nil {
			return fmt.Errorf("noise: %w", err)
		}
		if len(p) < 3 {
			return fmt.Errorf("noise: short response %x", p)
		}
		fmt.Printf("noise response: mode=0x%02x(%s) scene=0x%02x(%s) value=0x%02x\n",
			p[0], noiseModeName(p[0]), p[1], noiseSceneName(p[0], p[1]), p[2])
		return nil
	})
}

func isAncMode(s string) bool {
	_, ok := ancModes[s]
	return ok
}

func noiseModeName(mode byte) string {
	switch mode {
	case 0x01:
		return "ANC"
	case 0x02:
		return "Normal"
	case 0x03:
		return "Transparency"
	default:
		return "?"
	}
}

func noiseSceneName(mode, scene byte) string {
	if mode == 0x02 {
		return "off"
	}
	if mode == 0x03 {
		return "transparency"
	}
	switch scene {
	case 0x01:
		return "indoor"
	case 0x02:
		return "commuting"
	case 0x03:
		return "noisy"
	case 0x04:
		return "antiwind"
	case 0x05:
		return "adaptive"
	default:
		return "?"
	}
}

// ---------- toggles ----------

func frameHex(opcode byte, params []byte) string {
	return fmt.Sprintf("% X", buildFrame(opcode, params))
}

func cmdToggle(name string, args []string) error {
	da, err := parseDiagArgs(args, name == "wind")
	if err != nil {
		return err
	}
	args = da.rest
	if len(args) < 1 {
		return fmt.Errorf("usage: qcyctl %s <on|off> [seconds]", name)
	}
	state, err := parseOnOff(args[0])
	if err != nil {
		return err
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	if name == "ldac" && !da.yes {
		return errors.New("ldac restarts the earbuds (the vendor app says so: turning on LDAC will restart the device); add --yes to proceed")
	}
	if da.offByte >= 0 {
		if args[0] != "off" {
			return errors.New("--off-byte only applies to `wind off`")
		}
		if !da.yes {
			return errors.New("wind off --off-byte is a diagnostic write: add --yes to proceed")
		}
		b := byte(da.offByte)
		return cmdRunner(scanSeconds, func(dc daemonAPI) error {
			fmt.Printf("frame: %s\n", frameHex(protocol.OpWind, []byte{b}))
			got, err := dc.windOffByte(b)
			if err != nil {
				return err
			}
			fmt.Printf("wind=off read back raw=%x (0x00 and 0x02 both mean off)\n", got)
			return nil
		})
	}
	return cmdRunner(scanSeconds, func(dc daemonAPI) error {
		if name == "ldac" {
			// A single write, never repeated: the earbuds may restart right
			// after accepting it, and then the readback has nothing to say.
			got, err := dc.setActionExpect("ldac", []byte{state}, []byte{state})
			var ind *indeterminateError
			if errors.As(err, &ind) {
				fmt.Printf("ldac=%s %s\n", args[0], ind.msg)
				return nil
			}
			if err != nil {
				return err
			}
			if len(got) == 0 {
				fmt.Printf("ldac=%s applied; the earbuds restarted (expected, not a failure; not yet verified on hardware)\n", args[0])
			} else {
				fmt.Printf("ldac=%s confirmed\n", args[0])
			}
			return nil
		}
		if err := dc.setAction(name, []byte{state}); err != nil {
			return err
		}
		fmt.Printf("%s=%s sent\n", name, args[0])
		return nil
	})
}

func cmdWear(args []string) error {
	da, err := parseDiagArgs(args, true)
	if err != nil {
		return err
	}
	args = da.rest
	if len(args) < 1 {
		return errors.New("usage: qcyctl wear <on|off> [seconds]")
	}
	state, err := parseOnOff(args[0])
	if err != nil {
		return err
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	diag := da.offByte >= 0
	if diag {
		if args[0] != "off" {
			return errors.New("--off-byte only applies to `wear off`")
		}
		if !da.yes {
			return errors.New("wear off --off-byte is a diagnostic write: add --yes to proceed")
		}
		state = byte(da.offByte)
	}
	return cmdRunner(scanSeconds, func(dc daemonAPI) error {
		var got []byte
		var err error
		if diag {
			got, err = dc.wearOffByte(state)
		} else {
			got, err = dc.wearSet(state)
		}
		if err != nil {
			return err
		}
		if len(got) < 1 {
			return errors.New("wear: empty readback")
		}
		if diag {
			// The rest of the payload is written back as read, so the frame
			// sent is the written byte followed by the readback's tail.
			fmt.Printf("frame: %s\n", frameHex(protocol.OpWear, append([]byte{state}, got[1:]...)))
			fmt.Printf("wear=off read back raw byte0=0x%02x payload=%x (0x00 and 0x02 both mean off)\n", got[0], got)
			return nil
		}
		fmt.Printf("wear=%s confirmed (enabled=0x%02x actions=%x)\n", args[0], got[0], got[1:])
		return nil
	})
}

// promptVolWritable mirrors the daemon's switch: false restores the old
// read-only behaviour of prompt-vol. The 2-byte frame is verified on the test device (N70, 2026-10-13).
const promptVolWritable = true

// promptVolSteps are the values the vendor server lists for verified models, with the
// app's labels (Quieter / Medium / Louder / Maximum).
var promptVolSteps = []struct {
	Value byte
	Names []string
}{
	{4, []string{"low"}}, {6, []string{"medium"}}, {8, []string{"high"}}, {10, []string{"max"}},
}

func parsePromptVol(s string) (byte, error) {
	for _, st := range promptVolSteps {
		if strconv.Itoa(int(st.Value)) == s {
			return st.Value, nil
		}
		for _, n := range st.Names {
			if strings.EqualFold(n, s) {
				return st.Value, nil
			}
		}
	}
	return 0, fmt.Errorf("prompt-vol: invalid value %q (4|6|8|10 or low|medium|high|max)", s)
}

func parseWearSensitivity(s string) (byte, error) {
	switch strings.ToLower(s) {
	case "1", "medium":
		return 1, nil
	case "2", "normal":
		return 2, nil
	case "3", "high":
		return 3, nil
	}
	return 0, fmt.Errorf("wear-sensitivity: invalid value %q (medium|normal|high or 1|2|3)", s)
}

// Fit test frames (FF 05 11 03 01 01 01 / FF 05 11 03 00 00 00), verified on
// the test device (N70) 2026-10-13. The result notification is [status 2, left, right].
var (
	fitStartParams = []byte{1, 1, 1}
	fitStopParams  = []byte{0, 0, 0}
)

// fitSealName names a per-ear fit grade: 3 sealed, 2 good, 1 fair, 0 poor.
func fitSealName(n int) string {
	switch n {
	case 3:
		return "selado"
	case 2:
		return "bom"
	case 1:
		return "razoável"
	case 0:
		return "ruim"
	}
	return "desconhecido"
}

// cmdFit drives the ear-tip fit check. `fit start [seconds]` runs the test and
// waits for the result; `fit stop` sends the stop frame; `fit begin` only sends
// the start frame, for debugging.
func cmdFit(args []string) error {
	const usageMsg = "usage: qcyctl fit <start|stop|begin> [--json] [seconds]"
	jsonOut := false
	var rest []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) < 1 {
		return errors.New(usageMsg)
	}
	action := rest[0]
	if action != "start" && action != "stop" && action != "begin" {
		return fmt.Errorf("fit: expected start|stop|begin, got %q", action)
	}
	scanSeconds, err := parseSeconds(rest[1:], 30)
	if err != nil {
		return err
	}
	switch action {
	case "stop":
		return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
			if err := dc.fitStop(); err != nil {
				return err
			}
			fmt.Println("fit: stop sent")
			return nil
		})
	case "begin":
		return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
			if err := dc.setAction("fit", fitStartParams); err != nil {
				return err
			}
			fmt.Println("fit: start sent (no wait; use `fit stop` to end)")
			return nil
		})
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		// Ctrl-C: have the daemon cancel the test, which sends the stop frame.
		// A second connection, because this one is blocked on the result.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sig)
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-sig:
				if c2, err := dialDaemon(500 * time.Millisecond); err == nil {
					_ = c2.fitStop()
					c2.Close()
				} else {
					_ = dc.fitStop()
				}
			case <-done:
			}
		}()
		fmt.Fprintln(os.Stderr, "fit: aguardando o resultado (os fones tocam um som)...")
		r, err := dc.fitTest(scanSeconds)
		if err != nil {
			return err
		}
		return printFitResult(os.Stdout, r, jsonOut)
	})
}

func printFitResult(w io.Writer, r fitResult, jsonOut bool) error {
	if jsonOut {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	fmt.Fprintf(w, "esquerdo: %s (%d)\n", fitSealName(r.Left), r.Left)
	fmt.Fprintf(w, "direito: %s (%d)\n", fitSealName(r.Right), r.Right)
	return nil
}

func cmdWearSensitivity(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl wear-sensitivity <medium|normal|high|1|2|3> [seconds]")
	}
	v, err := parseWearSensitivity(args[0])
	if err != nil {
		return err
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		got, err := dc.setActionExpect("wearSensitivity", []byte{v}, []byte{v})
		if err != nil {
			return err
		}
		fmt.Printf("wear-sensitivity=%d confirmed (%x)\n", v, got)
		return nil
	})
}

func cmdSpatial(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl spatial <on|off> [seconds]")
	}
	state, err := parseOnOff(args[0])
	if err != nil {
		return err
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		if _, err := dc.setActionExpect("spatial", []byte{state}, []byte{state}); err != nil {
			return err
		}
		fmt.Printf("spatial=%s confirmed\n", args[0])
		return nil
	})
}

// ---------- prompt volume / auto-off / balance ----------

func cmdPromptVol(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl prompt-vol <4|6|8|10|low|medium|high|max> [seconds]")
	}
	if !promptVolWritable {
		return errors.New("prompt-vol is read-only on this device: promptVolWritable is false")
	}
	v, err := parsePromptVol(args[0])
	if err != nil {
		return err
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		// Two parameter bytes, the second 0, as the vendor app writes it.
		got, err := dc.setActionExpect("promptVol", []byte{v, 0}, []byte{v})
		if err != nil {
			return err
		}
		if len(got) >= 2 {
			fmt.Printf("prompt-vol=%d confirmed (max=%d)\n", got[0], got[1])
		} else {
			fmt.Printf("prompt-vol=%d confirmed\n", got[0])
		}
		return nil
	})
}

func cmdAutoOff(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl auto-off <minutes|never> [seconds]")
	}
	var want uint16 = 0xFFFF
	if args[0] != "never" {
		n, err := strconv.Atoi(args[0])
		if err == nil && n == 0 {
			return errors.New("auto-off: 0 is refused: the app sends 0 to power the earbuds off right now, not to disable the timer (use `never`)")
		}
		if err != nil || n < 1 || n > 0xFFFE {
			return fmt.Errorf("auto-off: invalid value %q (1-65534 minutes or never)", args[0])
		}
		want = uint16(n)
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return cmdRunner(scanSeconds, func(dc daemonAPI) error {
		// The vendor app sends four bytes here — {lo, hi, 0, 0} — and the
		// device echoes all four back (ffff0000 means "never"). This used to
		// send only the two value bytes, which happened to confirm because
		// confirmation is a prefix match, but it is not what the firmware is
		// documented to expect.
		le := append(le16(want), 0x00, 0x00)
		got, err := dc.setActionExpect("autoOff", le, le16(want))
		if err != nil {
			return err
		}
		cur := ""
		if len(got) >= 4 {
			cur = fmt.Sprintf(" current=%s", minutesName(u16le(got, 2)))
		}
		fmt.Printf("auto-off set=%s confirmed%s\n", minutesName(u16le(got, 0)), cur)
		return nil
	})
}

func cmdBalance(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl balance <left|center|right|0-100> [seconds]")
	}
	var val byte
	switch args[0] {
	case "left":
		val = 0x00
	case "center":
		val = 0x32
	case "right":
		val = 0x64
	default:
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 || n > 100 {
			return fmt.Errorf("balance: invalid value %q (left|center|right|0-100)", args[0])
		}
		val = byte(n)
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		// Readback-confirmed: the end state is known, so wait for the device
		// to report it rather than assuming the write landed.
		got, err := dc.setActionExpect("balance", []byte{val}, []byte{val})
		if err != nil {
			return err
		}
		fmt.Printf("balance=%d%% confirmed\n", got[0])
		return nil
	})
}

// ---------- EQ ----------
//
// 0x22 is read-modify-write. Every write starts from the payload the device
// reports right now, patches only the bytes the command names, saves the
// pre-write payload as a backup (and aborts if that fails), then writes and
// confirms by readback. Frequencies, Q and band types are never synthesised:
// the hearing-test slot carries non-standard values that a fixed table would
// destroy.

func cmdEq(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl eq <get|set <g1..g10>|preset <n>> [seconds]")
	}
	switch args[0] {
	case "get":
		return cmdEqGet(args[1:])
	case "set":
		return cmdEqSet(args[1:])
	case "preset":
		return cmdEqPreset(args[1:])
	}
	return fmt.Errorf("eq: unknown subcommand %q", args[0])
}

func printJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdEqGet(args []string) error {
	jsonOut := false
	var rest []string
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--preset" || strings.HasPrefix(a, "--preset="):
			// The old implementation wrote the index to characteristic 000B,
			// which this model does not have, and never restored it. There is no
			// select-without-write command, so reading another slot is not
			// possible; `eq get` always reports the active slot.
			return errors.New("eq get: --preset is not supported (this model has no characteristic 000B and a slot can only be read while it is active)")
		default:
			rest = append(rest, a)
		}
	}
	scanSeconds, err := parseSeconds(rest, 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		snap, err := dc.eqRead()
		if err != nil {
			return fmt.Errorf("eq get: %w", err)
		}
		if jsonOut {
			return printJSON(snap)
		}
		fmt.Printf("eq (preset %d): %s\n", snap.Slot, fmtEq(snap))
		return nil
	})
}

// parseDBArg parses a dB value for the master gain, rejecting NaN/Inf and
// anything outside +/-12 dB before it is ever converted to an integer.
func parseDBArg(what, s string) (int, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid value %q", what, s)
	}
	c, err := qcycore.MasterCentiFromDB(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	return c, nil
}

// eqSetArgs is the parsed command line of `eq set`.
type eqSetArgs struct {
	op        qcycore.EqOp
	jsonOut   bool
	confirmed bool
	secArgs   []string
}

const eqSetUsage = "usage: qcyctl eq set <g1,g2,...,g10> --yes [--preset N] [--master-gain dB] [--allow-protected] [--full-length] [--json] [seconds]"

// parseEqSetArgs parses and validates everything `eq set` needs before any
// Bluetooth is touched. The write-safety rules themselves (protected slots,
// 143-byte format) live in qcycore.ApplyEq; the flags only set the op fields.
func parseEqSetArgs(args []string) (eqSetArgs, error) {
	var a eqSetArgs
	if len(args) < 1 {
		return a, errors.New(eqSetUsage)
	}
	gainsArg := args[0]
	// value returns the value of a "--flag v" / "--flag=v" option.
	value := func(i *int, name string) (string, error) {
		arg := args[*i]
		if arg == name {
			if *i+1 >= len(args) {
				return "", fmt.Errorf("eq set: %s needs a value", name)
			}
			*i++
			return args[*i], nil
		}
		return strings.TrimPrefix(arg, name+"="), nil
	}
	for i := 1; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--yes":
			a.confirmed = true
		case arg == "--json":
			a.jsonOut = true
		case arg == "--allow-protected":
			a.op.AllowProtected = true
		case arg == "--full-length":
			a.op.AllowFullLength = true
		case arg == "--preset" || strings.HasPrefix(arg, "--preset="):
			v, err := value(&i, "--preset")
			if err != nil {
				return a, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 || n > 255 {
				return a, fmt.Errorf("eq set: invalid --preset value %q (0-255)", v)
			}
			a.op.Slot = &n
		case arg == "--master-gain" || strings.HasPrefix(arg, "--master-gain="):
			v, err := value(&i, "--master-gain")
			if err != nil {
				return a, err
			}
			c, err := parseDBArg("eq set: --master-gain", v)
			if err != nil {
				return a, err
			}
			a.op.MasterCenti = &c
		default:
			a.secArgs = append(a.secArgs, arg)
		}
	}
	gains, err := qcycore.ParseGains(gainsArg)
	if err != nil {
		return a, fmt.Errorf("eq set: %w", err)
	}
	a.op.Gains = &gains
	return a, nil
}

func cmdEqSet(args []string) error {
	a, err := parseEqSetArgs(args)
	if err != nil {
		return err
	}
	// Writing 0x22 replaces the curve of the target slot and makes it active.
	// The active slot's payload is backed up, not the target slot's old content (unreadable while inactive); the write is still a deliberate act.
	if !a.confirmed {
		return errors.New("eq set: this overwrites the EQ curve of the target slot; pass --yes to confirm (the payload of the currently ACTIVE slot is backed up first; the previous contents of the target slot cannot be read while it is inactive, so they are not covered)")
	}
	scanSeconds, err := parseSeconds(a.secArgs, 30)
	if err != nil {
		return err
	}
	op, jsonOut := a.op, a.jsonOut
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		res, err := dc.eqApply(op)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(res)
		}
		fmt.Printf("eq set confirmed: preset=%d masterGain=%+.2fdB %s\n", res.Slot, res.MasterGain, fmtEq(&res.EqSnapshot))
		warnFullLength(res)
		fmt.Printf("backup: %s\n", res.BackupHistory)
		return nil
	})
}

// cmdMasterGain changes ONLY the master gain (bytes 1-2 of 0x22). The slot,
// every band's freq/gain/Q/type and the rest of the payload are carried over
// from what the device reports.
func cmdMasterGain(args []string) error {
	const usage = "usage: qcyctl master-gain <dB> [--full-length] [--json] [seconds]   (dB in -12..+12)"
	jsonOut := false
	var allowFull bool
	var pos []string
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		case "--full-length":
			allowFull = true
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New(usage)
	}
	centi, err := parseDBArg("master-gain", pos[0])
	if err != nil {
		return err
	}
	scanSeconds, err := parseSeconds(pos[1:], 30)
	if err != nil {
		return err
	}
	op := qcycore.EqOp{MasterCenti: &centi, AllowFullLength: allowFull}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		res, err := dc.eqApply(op)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(res)
		}
		fmt.Printf("master-gain=%+.2fdB slot=%d confirmed\n", res.MasterGain, res.Slot)
		warnFullLength(res)
		fmt.Printf("backup: %s\n", res.BackupHistory)
		return nil
	})
}

// cmdEqPreset writes the preset index to characteristic 000B. This model does
// not have it, so the command fails instead of pretending to have switched.
func cmdEqPreset(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl eq preset <n> [seconds]")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 0 || n > 255 {
		return fmt.Errorf("eq preset: invalid value %q", args[0])
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		if c.q.eqPreset == nil {
			return errors.New("eq preset: not supported on this model (characteristic 000B absent); use `qcyctl eq set ... --preset N`")
		}
		if _, err := c.q.eqPreset.WriteWithoutResponse([]byte{byte(n)}); err != nil {
			return fmt.Errorf("eq preset write 000B: %w", err)
		}
		var buf [4]byte
		cnt, err := c.q.eqPreset.Read(buf[:])
		if err != nil {
			return fmt.Errorf("eq preset read 000B: %w", err)
		}
		if cnt < 1 || buf[0] != byte(n) {
			return fmt.Errorf("eq preset readback mismatch: want 0x%02x got %x", n, buf[:cnt])
		}
		fmt.Printf("eq preset=%d confirmed\n", buf[0])
		return nil
	})
}

// warnFullLength tells the user the write used the full-length 0x22 format,
// which (unlike the 73-byte one) has not been proven on the device.
func warnFullLength(res *qcycore.EqResult) {
	if res.FullLength {
		fmt.Fprintf(os.Stderr, "warning: fullLength=true: wrote the full 143-byte payload (slot 255 takes it without --full-length, as the phone app does; any other slot needs the flag); this format is NOT proven for the factory slots (readback was confirmed byte for byte)\n")
	}
}

func fmtEq(s *qcycore.EqSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "preset=%d masterGain=%+.2fdB raw=%s bands=", s.Slot, s.MasterGain, s.Payload)
	for i, band := range s.Bands {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%dHz:%.2fdB", band.Freq, band.Gain)
	}
	return b.String()
}

// ---------- key functions (touch remap) ----------

func cmdKeys(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl keys <get|set <keyId:funId>...> [seconds]")
	}
	switch args[0] {
	case "get":
		return cmdKeysGet(args[1:])
	case "set":
		return cmdKeysSet(args[1:])
	}
	return fmt.Errorf("keys: unknown subcommand %q", args[0])
}

func cmdKeysGet(args []string) error {
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		got, err := dc.keys()
		if err != nil {
			return err
		}
		fmt.Printf("keys raw=%x\n", got)
		fmt.Printf("keys pairs=%s\n", strings.Join(keyPairs(got), " "))
		if len(got)%2 == 1 {
			fmt.Printf("keys note: odd trailing byte 0x%02x\n", got[len(got)-1])
		}
		return nil
	})
}

func keyPairs(raw []byte) []string {
	var pairs []string
	for i := 0; i+1 < len(raw); i += 2 {
		pairs = append(pairs, fmt.Sprintf("%02x:%02x", raw[i], raw[i+1]))
	}
	return pairs
}

func cmdKeysSet(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl keys set <keyId:funId> [<keyId:funId> ...] [seconds]")
	}
	var raw []byte
	var secArgs []string
	for _, a := range args {
		if !strings.Contains(a, ":") {
			secArgs = append(secArgs, a)
			continue
		}
		parts := strings.SplitN(a, ":", 2)
		k, err := parseByte(parts[0])
		if err != nil {
			return fmt.Errorf("keys set: invalid keyId %q", parts[0])
		}
		f, err := parseByte(parts[1])
		if err != nil {
			return fmt.Errorf("keys set: invalid funId %q", parts[1])
		}
		raw = append(raw, k, f)
	}
	if len(raw) == 0 {
		return errors.New("keys set: no pairs given")
	}
	scanSeconds, err := parseSeconds(secArgs, 30)
	if err != nil {
		return err
	}
	return runViaDaemon(scanSeconds, func(dc daemonAPI) error {
		if err := dc.keysSet(raw); err != nil {
			return err
		}
		fmt.Printf("keys set confirmed raw=%x\n", raw)
		return nil
	})
}

// ---------- parsers ----------

func parseBattery(b []byte) (left, right, box byte, lc, rc, cc bool, err error) {
	if len(b) < 3 {
		return 0, 0, 0, false, false, false, fmt.Errorf("battery: need 3 bytes, got %d", len(b))
	}
	left, lc = b[0]&0x7f, b[0]&0x80 != 0
	right, rc = b[1]&0x7f, b[1]&0x80 != 0
	box, cc = b[2]&0x7f, b[2]&0x80 != 0
	return left, right, box, lc, rc, cc, nil
}

func parseVersion(b []byte) (string, string, error) {
	switch {
	case len(b) >= 6:
		return fmt.Sprintf("%d.%d.%d", b[0], b[1], b[2]), fmt.Sprintf("%d.%d.%d", b[3], b[4], b[5]), nil
	case len(b) >= 3:
		return fmt.Sprintf("%d.%d.%d", b[0], b[1], b[2]), "", nil
	}
	return "", "", fmt.Errorf("version: need 3 or 6 bytes, got %d", len(b))
}
