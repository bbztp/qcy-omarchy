package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	quicky "github.com/hui1601/Quicky/lib"
	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore"
	"tinygo.org/x/bluetooth"
)

const (
	svcUUID      = "0000a001-0000-1000-8000-00805f9b34fb"
	cmdUUID      = "00001001-0000-1000-8000-00805f9b34fb"
	notifyUUID   = "00001002-0000-1000-8000-00805f9b34fb"
	battUUID     = "00000008-0000-1000-8000-00805f9b34fb"
	verUUID      = "00000007-0000-1000-8000-00805f9b34fb"
	keysUUID     = "0000000d-0000-1000-8000-00805f9b34fb"
	eqPresetUUID = "0000000b-0000-1000-8000-00805f9b34fb"

	// How long the indicator blinks for a find-my-earbuds request before the
	// daemon turns it back off by itself.
	findBlinkDuration = 15 * time.Second
)

var (
	svcUUIDParsed      = mustUUID(svcUUID)
	cmdUUIDParsed      = mustUUID(cmdUUID)
	notifyUUIDParsed   = mustUUID(notifyUUID)
	battUUIDParsed     = mustUUID(battUUID)
	verUUIDParsed      = mustUUID(verUUID)
	keysUUIDParsed     = mustUUID(keysUUID)
	eqPresetUUIDParsed = mustUUID(eqPresetUUID)
)

type daemon struct {
	// connMu serialises connect attempts so that a slow scan does not have to
	// hold mu: status reads, markDisconnected and the capability snapshot only
	// need mu, and must not stall behind a 15-30 s scan.
	connMu sync.Mutex
	// mu guards connection state (connected, notifyCh, caps, handler, modelID).
	mu           sync.Mutex
	adapter      *bluetooth.Adapter
	dev          bluetooth.Device
	cmdChar      bluetooth.DeviceCharacteristic
	notifyChar   bluetooth.DeviceCharacteristic
	batteryChar  *bluetooth.DeviceCharacteristic
	versionChar  *bluetooth.DeviceCharacteristic
	keysChar     *bluetooth.DeviceCharacteristic
	eqPresetChar *bluetooth.DeviceCharacteristic
	notifyCh     chan []byte
	frames       chan frame
	connected    bool
	scanSeconds  int

	// Model detection & routing
	deviceName string
	caps       map[string]bool
	// txMu serialises BLE transactions. The frame channel is shared, so a
	// background capability probe interleaving with a status read would drain
	// the other one's replies and both would time out.
	txMu    sync.Mutex
	modelID string
	handler ModelHandler
}

type frame struct {
	cmd    byte
	params []byte
}

type request struct {
	Cmd  string                 `json:"cmd"`
	Args map[string]interface{} `json:"args,omitempty"`
}

type response struct {
	OK    bool        `json:"ok"`
	Data  interface{} `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
}

func mustUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic("bad UUID " + s + ": " + err.Error())
	}
	return u
}

// ============================================================
// Capability-driven model registry
// ============================================================

// FeatureFlag represents a user-facing feature the model supports.
type FeatureFlag string

const (
	FeatureANC          FeatureFlag = "anc"          // Adaptive ANC with scenes
	FeatureGameMode     FeatureFlag = "gameMode"     // Low latency game mode
	FeatureSleepMode    FeatureFlag = "sleepMode"    // Sleep timer
	FeatureLDAC         FeatureFlag = "ldac"         // LDAC codec toggle
	FeatureMultipoint   FeatureFlag = "multipoint"   // Multipoint toggle
	FeatureWindNoise    FeatureFlag = "windNoise"    // Wind noise reduction
	FeatureWearDetect   FeatureFlag = "wearDetect"   // In-ear detection
	FeatureAutoOff      FeatureFlag = "autoOff"      // Auto power-off timer
	FeatureBalance      FeatureFlag = "balance"      // L/R balance
	FeaturePromptVolume FeatureFlag = "promptVolume" // Prompt volume
	FeatureFind         FeatureFlag = "find"         // Find my earbuds
	FeatureBatteryCase  FeatureFlag = "batteryCase"  // Case battery level
	FeatureKeysRemap    FeatureFlag = "keysRemap"    // Touch key remapping
	FeatureEQ           FeatureFlag = "eq"           // EQ curve + master gain (read/write via eqApply, backed up)
)

// ActionSpec describes a settable action for the UI.
type ActionSpec struct {
	ID         string          `json:"id"`                // unique action ID
	Label      string          `json:"label"`             // user-visible label
	Type       string          `json:"type"`              // "toggle" | "enum" | "trigger" | "slider"
	Options    []string        `json:"options,omitempty"` // for enum: ["off", "adaptive", ...]
	Min        int             `json:"min,omitempty"`     // for slider
	Max        int             `json:"max,omitempty"`
	Step       int             `json:"step,omitempty"`
	Opcode     byte            `json:"-"` // internal opcode
	ParamMap   map[string]byte `json:"-"` // option -> param byte
	ReadOpcode byte            `json:"-"` // opcode to read current state
	ReadParser string          `json:"-"` // parser name for read response
}

// Capabilities describes what a model supports and how to present it.
type Capabilities struct {
	ModelID      string        `json:"modelId"`      // e.g., "melobuds-n70"
	DisplayName  string        `json:"displayName"`  // e.g., "QCY MeloBuds N70"
	Features     []FeatureFlag `json:"features"`     // supported features
	Actions      []ActionSpec  `json:"actions"`      // settable actions
	BatteryChars []string      `json:"batteryChars"` // "left", "right", "case"
}

// ModelHandler defines the interface each model implements.
type ModelHandler interface {
	Capabilities() Capabilities
	// HandleSet executes a setter action. When expect is non-empty the daemon
	// waits for the device to report that payload, so setters with a known end
	// state (balance, prompt volume, auto-off) can be confirmed; toggles pass
	// nil and return as soon as the write lands.
	HandleSet(d *daemon, actionID string, params, expect []byte) ([]byte, error)
	// HandleRead reads a specific state (optional, for non-standard reads).
	HandleRead(d *daemon, actionID string) (interface{}, error)
	// OnConnect called after successful connection (subscribe extra chars, etc).
	OnConnect(d *daemon) error
	// OnDisconnect called when connection drops.
	OnDisconnect(d *daemon)
}

// GenericHandler is the fallback for unknown models.
type GenericHandler struct{}

func (GenericHandler) Capabilities() Capabilities {
	// Mirror the shared QCY opcode set rather than advertising a stripped-down
	// UI: these are the actions the daemon actually accepts for any model.
	return Capabilities{
		ModelID:     "generic",
		DisplayName: "QCY Earbuds",
		Features: []FeatureFlag{
			FeatureANC, FeatureGameMode, FeatureSleepMode,
			FeatureLDAC, FeatureMultipoint, FeatureWindNoise,
			FeatureWearDetect, FeatureAutoOff, FeatureBalance,
			FeaturePromptVolume, FeatureFind, FeatureBatteryCase,
		},
		Actions: []ActionSpec{
			{ID: "anc", Label: "ANC", Type: "enum", Opcode: 0x17},
			{ID: "gameMode", Label: "Game Mode", Type: "toggle", Opcode: 0x09},
			{ID: "sleepMode", Label: "Sleep Mode", Type: "toggle", Opcode: 0x10},
			{ID: "ldac", Label: "LDAC", Type: "toggle", Opcode: 0x23},
			{ID: "multipoint", Label: "Multipoint", Type: "toggle", Opcode: 0x24},
			{ID: "windNoise", Label: "Wind Noise Reduction", Type: "toggle", Opcode: 0x2A},
			{ID: "wearDetect", Label: "Wear Detection", Type: "toggle", Opcode: 0x2C},
			{ID: "autoOff", Label: "Auto Off", Type: "slider", Opcode: 0x14},
			{ID: "balance", Label: "Balance", Type: "slider", Opcode: 0x16},
			{ID: "promptVolume", Label: "Prompt Volume", Type: "slider", Opcode: 0x1D},
		},
		BatteryChars: []string{"left", "right", "case"},
	}
}

// bytesJSON renders a raw opcode payload as a JSON array of numbers. A []byte
// would marshal to a base64 string, which the client's []interface{} decoder
// rejects — the field would silently vanish from the response.
func bytesJSON(b []byte) []int {
	out := make([]int, len(b))
	for i, c := range b {
		out[i] = int(c)
	}
	return out
}

// decodeParams accepts the setter body either as a JSON array of numbers (the
// current wire format) or as a base64 string (older clients marshalled []byte
// directly). A missing body is legitimate for opcodes that take no argument.
func decodeParams(v interface{}) []byte {
	switch p := v.(type) {
	case []interface{}:
		out := make([]byte, 0, len(p))
		for _, e := range p {
			if f, ok := e.(float64); ok {
				out = append(out, byte(f))
			}
		}
		return out
	case string:
		b, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return nil
		}
		return b
	}
	return nil
}

// HandleSet falls back to the daemon's opcode table. Unknown models must still
// be able to drive the shared QCY opcode set — rejecting here would break every
// setter the moment detection lands on an unrecognised string.
func (GenericHandler) HandleSet(d *daemon, actionID string, params, expect []byte) ([]byte, error) {
	return d.setAction(actionID, params, expect)
}

func (GenericHandler) HandleRead(d *daemon, actionID string) (interface{}, error) {
	return nil, fmt.Errorf("generic handler: unknown read %s", actionID)
}

func (GenericHandler) OnConnect(d *daemon) error { return nil }
func (GenericHandler) OnDisconnect(d *daemon)    {}

// ============================================================
// MeloBuds N70 handler
// ============================================================

type meloBudsN70Handler struct{}

func (meloBudsN70Handler) Capabilities() Capabilities {
	return Capabilities{
		ModelID:     "melobuds-n70",
		DisplayName: "QCY MeloBuds N70",
		Features: []FeatureFlag{
			FeatureANC, FeatureGameMode, FeatureSleepMode,
			FeatureLDAC, FeatureMultipoint, FeatureWindNoise,
			FeatureWearDetect, FeatureAutoOff, FeatureBalance,
			FeaturePromptVolume, FeatureFind, FeatureBatteryCase,
			FeatureEQ, FeatureKeysRemap,
		},
		Actions: []ActionSpec{
			{
				ID: "anc", Label: "ANC Mode", Type: "enum",
				Options: []string{"off", "adaptive", "indoor", "commuting", "noisy", "antiwind", "transparency"},
				Opcode:  0x17,
				ParamMap: map[string]byte{
					"off": 0x00, "adaptive": 0x01, "indoor": 0x01, "commuting": 0x02,
					"noisy": 0x03, "antiwind": 0x04, "transparency": 0x01,
				},
				ReadOpcode: 0x17, ReadParser: "anc",
			},
			{ID: "gameMode", Label: "Game Mode", Type: "toggle", Opcode: 0x09, ReadOpcode: 0x09},
			{ID: "sleepMode", Label: "Sleep Mode", Type: "toggle", Opcode: 0x10, ReadOpcode: 0x10},
			{ID: "ldac", Label: "LDAC", Type: "toggle", Opcode: 0x23, ReadOpcode: 0x23},
			{ID: "multipoint", Label: "Multipoint", Type: "toggle", Opcode: 0x24, ReadOpcode: 0x24},
			{ID: "windNoise", Label: "Wind Noise Reduction", Type: "toggle", Opcode: 0x2A, ReadOpcode: 0x2A},
			{ID: "wearDetect", Label: "Wear Detection", Type: "toggle", Opcode: 0x2C, ReadOpcode: 0x2C},
			{ID: "autoOff", Label: "Auto Off", Type: "slider", Min: 0, Max: 180, Step: 5, Opcode: 0x14, ReadOpcode: 0x14},
			{ID: "balance", Label: "Balance", Type: "slider", Min: 0, Max: 100, Step: 1, Opcode: 0x16, ReadOpcode: 0x16},
			// Max is the measured ceiling (0x1D reports max=15), not the byte
			// width — the payload is one byte but the scale is 0..15.
			{ID: "promptVolume", Label: "Prompt Volume", Type: "slider", Min: 0, Max: 15, Step: 1, Opcode: 0x1D, ReadOpcode: 0x1D},
		},
		BatteryChars: []string{"left", "right", "case"},
	}
}

func (meloBudsN70Handler) HandleSet(d *daemon, actionID string, params, expect []byte) ([]byte, error) {
	// Map handler action IDs to daemon action IDs
	daemonAction := map[string]string{
		"gameMode":     "game",
		"sleepMode":    "sleep",
		"windNoise":    "wind",
		"wearDetect":   "wear",
		"autoOff":      "autoOff",
		"promptVolume": "promptVol",
		"balance":      "balance",
		"anc":          "anc",
		"ldac":         "ldac",
		"multipoint":   "multipoint",
		"find":         "find",
	}[actionID]
	if daemonAction == "" {
		daemonAction = actionID
	}
	return d.setAction(daemonAction, params, expect)
}

func (meloBudsN70Handler) HandleRead(d *daemon, actionID string) (interface{}, error) {
	return nil, nil // handled by generic gatherStatus
}

func (meloBudsN70Handler) OnConnect(d *daemon) error { return nil }
func (meloBudsN70Handler) OnDisconnect(d *daemon)    {}

// Register at init
func init() {
	RegisterModel("melobuds-n70", meloBudsN70Handler{})
}

// Registry maps model ID -> handler.
var modelRegistry = map[string]ModelHandler{}

func RegisterModel(modelID string, h ModelHandler) {
	modelRegistry[modelID] = h
}

func GetHandler(modelID string) ModelHandler {
	if h, ok := modelRegistry[modelID]; ok {
		return h
	}
	return GenericHandler{}
}

func cacheFilePath() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "qcyctl", "le-address")
}

func loadCachedAddr() (bluetooth.Address, bool) {
	p := cacheFilePath()
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

func saveCachedAddr(addr bluetooth.Address, name string) {
	p := cacheFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	random := "0"
	if addr.IsRandom() {
		random = "1"
	}
	// Third field is the advertised name, used for model detection on the fast
	// path (which never scans and therefore never sees an advertisement).
	line := addr.String() + " " + random
	if name != "" {
		line += " " + strings.ReplaceAll(name, " ", "_")
	}
	_ = os.WriteFile(p, []byte(line+"\n"), 0o644)
}

// loadCachedName returns the name stored alongside a cached address.
func loadCachedName() string {
	b, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return ""
	}
	return strings.ReplaceAll(fields[2], "_", " ")
}

func (d *daemon) connect(ctx context.Context) error {
	d.connMu.Lock()
	defer d.connMu.Unlock()

	d.mu.Lock()
	already := d.connected
	d.mu.Unlock()
	if already {
		return nil
	}

	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return fmt.Errorf("enable adapter: %w", err)
	}
	d.adapter = adapter

	// Fast path: cached address
	if addr, ok := loadCachedAddr(); ok {
		if dev, err := d.dialLE(adapter, addr, 2*time.Second); err == nil {
			d.deviceName = loadCachedName()
			if err := d.setupDevice(dev); err == nil {
				saveCachedAddr(addr, d.deviceName)
				d.setConnected()
				return nil
			}
			_ = dev.Disconnect()
		}
	}

	// Scan path
	scanner := quicky.NewScanner()
	addrCh := make(chan bluetooth.Address, 1)
	nameCh := make(chan string, 1)
	scanErrCh := make(chan error, 1)
	go func() {
		scanErrCh <- scanner.Scan(func(r quicky.ScanResult) {
			select {
			case addrCh <- r.Address:
			default:
			}
			if r.Name != "" {
				select {
				case nameCh <- r.Name:
				default:
				}
			}
		})
	}()

	var addr bluetooth.Address
	found := false
	timer := time.NewTimer(time.Duration(d.scanSeconds) * time.Second)
	defer timer.Stop()
	select {
	case addr = <-addrCh:
		found = true
	case err := <-scanErrCh:
		return fmt.Errorf("scan: %w", err)
	case <-timer.C:
	}
	if !found {
		_ = scanner.StopScan()
		return errors.New("no QCY device found during scan")
	}

	var dev bluetooth.Device
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		dev, err = adapter.Connect(addr, bluetooth.ConnectionParams{})
		if err == nil {
			break
		}
		select {
		case addr = <-addrCh:
		default:
		}
		time.Sleep(700 * time.Millisecond)
	}
	_ = scanner.StopScan()
	if err != nil {
		return fmt.Errorf("connect LE %s: %w", addr, err)
	}

	d.deviceName = loadScanName(nameCh)
	if err := d.setupDevice(dev); err != nil {
		_ = dev.Disconnect()
		return err
	}
	saveCachedAddr(addr, d.deviceName)
	d.setConnected()
	return nil
}

func (d *daemon) setConnected() {
	d.mu.Lock()
	d.connected = true
	d.mu.Unlock()
}

// loadScanName picks up the advertised name observed during the scan, if any.
func loadScanName(ch chan string) string {
	select {
	case n := <-ch:
		return n
	default:
		return ""
	}
}

func (d *daemon) dialLE(adapter *bluetooth.Adapter, addr bluetooth.Address, timeout time.Duration) (bluetooth.Device, error) {
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

func (d *daemon) setupDevice(dev bluetooth.Device) error {
	services, err := dev.DiscoverServices([]bluetooth.UUID{svcUUIDParsed})
	if err != nil {
		return fmt.Errorf("discover service a001: %w", err)
	}
	if len(services) == 0 {
		return errors.New("QCY service a001 not found on LE device")
	}
	svc := services[0]

	req, err := svc.DiscoverCharacteristics([]bluetooth.UUID{cmdUUIDParsed, notifyUUIDParsed})
	if err != nil {
		return fmt.Errorf("discover characteristics: %w", err)
	}
	if len(req) < 2 {
		return errors.New("missing command/notify characteristics")
	}
	d.cmdChar = req[0]
	d.notifyChar = req[1]

	d.batteryChar = findChar(svc, battUUIDParsed)
	d.versionChar = findChar(svc, verUUIDParsed)
	d.keysChar = findChar(svc, keysUUIDParsed)
	d.eqPresetChar = findChar(svc, eqPresetUUIDParsed)
	probeCharacteristics(svc)

	notifyCh := make(chan []byte, 16)
	d.mu.Lock()
	d.dev = dev
	d.notifyCh = notifyCh
	d.frames = make(chan frame, 64)
	d.mu.Unlock()

	if err := d.notifyChar.EnableNotifications(func(buf []byte) {
		// A non-blocking send: the callback must never wedge the BLE stack,
		// and it must not panic on a channel markDisconnected has closed.
		defer func() { _ = recover() }()
		select {
		case notifyCh <- append([]byte(nil), buf...):
		default:
		}
	}); err != nil {
		return fmt.Errorf("enable notifications 1002: %w", err)
	}

	go d.readLoop(notifyCh)

	// Detect model and attach handler
	d.detectAndAttachModel()
	return nil
}

func (d *daemon) detectAndAttachModel() {
	// The advertised name is the authoritative signal; the 0007 firmware string
	// is only a corroborating hint (it is a bare "1.2.3" on this hardware).
	modelID := detectModelFromName(d.deviceName)
	if modelID == "generic" {
		modelID = detectModelFromVersion(d.readVersionString())
	}
	h := GetHandler(modelID)
	d.mu.Lock()
	d.modelID = modelID
	d.handler = h
	d.mu.Unlock()
	_ = h.OnConnect(d)

	// Probing costs a few round-trips and three of those commands go
	// unanswered on some models, so it must not sit in front of the first
	// status read the panel is waiting on. Run it after the connection is
	// reported and let the next status pick up the result.
	go d.probeCapabilities()
}

// readVersionRaw reads characteristic 0007 (firmware) as raw bytes. It is a
// single GATT read — no command round-trip — so it is cheap enough to run on
// every connect. Returns nil when the characteristic is absent or unreadable.
func (d *daemon) readVersionRaw() []byte {
	if d.versionChar == nil {
		return nil
	}
	var buf [8]byte
	n, err := d.versionChar.Read(buf[:])
	if err != nil || n == 0 {
		return nil
	}
	return append([]byte(nil), buf[:n]...)
}

// readVersionString returns the firmware as a printable string, for model
// detection. The bytes are not text, so they are only useful as an opaque key.
func (d *daemon) readVersionString() string {
	return string(d.readVersionRaw())
}

// detectModelFromVersion maps the firmware string onto a registered model id.
// Unknown strings fall back to the generic handler.
func detectModelFromVersion(fw string) string {
	fw = strings.ToLower(fw)
	switch {
	case strings.Contains(fw, "melobuds") || strings.Contains(fw, "n70"):
		return "melobuds-n70"
	}
	return "generic"
}

// detectModelFromName maps the advertised Bluetooth name onto a registered model
// id. This is the only source that actually identifies a model: the 0007
// characteristic holds a bare numeric firmware ("1.2.3") with no model string in
// it, so it cannot be used to tell an N70 from anything else.
func detectModelFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "melobuds n70"), strings.Contains(n, "n70"):
		return "melobuds-n70"
	}
	return "generic"
}

func findChar(s bluetooth.DeviceService, uuid bluetooth.UUID) *bluetooth.DeviceCharacteristic {
	c, err := s.DiscoverCharacteristics([]bluetooth.UUID{uuid})
	if err != nil || len(c) != 1 {
		dbg("findChar %s: err=%v n=%d\n", uuid.String(), err, len(c))
		return nil
	}
	return &c[0]
}

// probeCharacteristics is a QCYCTL_PROBE diagnostic: lists every characteristic
// of the a001 service with its properties and whether it answers a read. It
// exists because the touch-control characteristic 0x000D returns zero bytes on
// this firmware, and the only way to tell "absent" from "silent" is to enumerate.
func probeCharacteristics(svc bluetooth.DeviceService) {
	if os.Getenv("QCYCTL_PROBE") == "" {
		return
	}
	all, err := svc.DiscoverCharacteristics(nil)
	if err != nil {
		dbg("PROBE discover all: %v\n", err)
		return
	}
	dbg("PROBE %d characteristics on a001\n", len(all))
	for i := range all {
		c := all[i]
		var buf [64]byte
		n, rerr := c.Read(buf[:])
		dbg("PROBE  %s read=%d err=%v\n", c.UUID().String(), n, rerr)
	}
}

func (d *daemon) markDisconnected() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.connected {
		d.connected = false
		dbg("DISCONNECTED: readLoop ended, notify channel closed\n")
		if d.notifyCh != nil {
			close(d.notifyCh)
			d.notifyCh = nil
		}
	}
}

func (d *daemon) readLoop(notifyCh chan []byte) {
	for buf := range notifyCh {
		dbg("notify raw=%x\n", buf)
		frames, err := parseFrames(buf)
		if err != nil {
			dbg("notify unparsed: %v\n", err)
			continue
		}
		for _, f := range frames {
			d.frames <- f
		}
	}
	d.markDisconnected()
}

// dbg writes diagnostics to stderr when QCYCTL_DEBUG is set. Off by default:
// the per-frame chatter is far too noisy for a service log.
func dbg(format string, args ...interface{}) {
	if os.Getenv("QCYCTL_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

// parseFrames decodes a notification payload into frames. Layout per block is
// <opcode> <paramLen> <params...>, where the block body (opcode onwards) is
// bodyLen bytes long. A single notification may carry several blocks, so this
// loops until the buffer is consumed.
func parseFrames(buf []byte) ([]frame, error) {
	var out []frame
	off := 0
	for off < len(buf) {
		if buf[off] != 0xff {
			return out, fmt.Errorf("bad SOF 0x%02x at offset %d", buf[off], off)
		}
		if off+2 > len(buf) {
			return out, errors.New("truncated header")
		}
		bodyLen := int(buf[off+1])
		end := off + 2 + bodyLen
		if end > len(buf) {
			return out, errors.New("truncated body")
		}
		body := buf[off+2 : end]
		bo := 0
		for bo < len(body) {
			if bo+2 > len(body) {
				return out, errors.New("truncated block header")
			}
			op := body[bo]
			n := int(body[bo+1])
			bo += 2
			if bo+n > len(body) {
				return out, errors.New("truncated block params")
			}
			out = append(out, frame{cmd: op, params: append([]byte(nil), body[bo:bo+n]...)})
			bo += n
		}
		off = end
	}
	if len(out) == 0 {
		return nil, errors.New("no frames")
	}
	return out, nil
}

// buildFrame encodes a command as FF <bodyLen> <opcode> <paramLen> <params...>.
// A query is simply a command carrying the reserved 0xFE opcode whose single
// parameter is the opcode being asked about — the same encoding the proven
// direct client uses. This must stay byte-identical to parseFrames above and to
// the client in the qcyctl backend.
func buildFrame(opcode byte, params []byte) []byte {
	p := make([]byte, 0, 4+len(params))
	p = append(p, 0xff, byte(len(params)+2), opcode, byte(len(params)))
	p = append(p, params...)
	return p
}

func (d *daemon) send(opcode byte, params []byte) error {
	pkt := buildFrame(opcode, params)
	dbg("TX 0x%02x %x\n", opcode, pkt)
	_, err := d.cmdChar.WriteWithoutResponse(pkt)
	if err == nil {
		return nil
	}
	_, err2 := d.cmdChar.Write(pkt)
	if err2 != nil {
		dbg("SET-FAIL 0x%02x: without-response=%v with-response=%v\n", opcode, err, err2)
		d.markDisconnected()
		return fmt.Errorf("write 0x%02x: %v", opcode, err2)
	}
	return nil
}

func (d *daemon) wait(opcode byte, timeout time.Duration) ([]byte, error) {
	deadline := time.After(timeout)
	for {
		select {
		case f := <-d.frames:
			if f.cmd == opcode {
				return f.params, nil
			}
		case <-deadline:
			return nil, fmt.Errorf("no response for opcode 0x%02x within %s", opcode, timeout)
		}
	}
}

// query sends a query frame and waits for the matching reply. These earbuds
// frequently emit the pre-change state as the immediate reply and only settle
// on a later, unsolicited notification, so a single send+wait times out on most
// opcodes. Mirrors the proven client.setConfirm loop in the qcyctl backend:
// re-query periodically and return the first frame that matches.
func (d *daemon) query(opcode byte, timeout time.Duration) ([]byte, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	return d.queryLocked(opcode, timeout)
}

// queryLocked is query for callers that already hold txMu (read-modify-write
// transactions). Go mutexes are not reentrant, so they must not call query.
func (d *daemon) queryLocked(opcode byte, timeout time.Duration) ([]byte, error) {
	d.drain()
	if err := d.send(0xFE, []byte{opcode}); err != nil {
		return nil, err
	}
	deadline := time.After(timeout)
	requery := time.After(500 * time.Millisecond)
	queries := 0
	var last []byte
	for {
		select {
		case f := <-d.frames:
			if f.cmd != opcode {
				continue
			}
			last = f.params
			// A reply that carries real data is the settled state. Replies that
			// echo the pre-change value keep arriving, so treat an all-0xFF or
			// empty body as "not yet" and let the re-query continue.
			if len(f.params) > 0 && !allFF(f.params) {
				return f.params, nil
			}
		case <-requery:
			if queries < 5 {
				_ = d.send(0xFE, []byte{opcode})
				queries++
				requery = time.After(500 * time.Millisecond)
			}
		case <-deadline:
			if last != nil {
				return last, nil
			}
			return nil, fmt.Errorf("no response for opcode 0x%02x within %s", opcode, timeout)
		}
	}
}

func allFF(b []byte) bool {
	for _, c := range b {
		if c != 0xff {
			return false
		}
	}
	return true
}

func (d *daemon) drain() {
	for {
		select {
		case <-d.frames:
		default:
			return
		}
	}
}

func (d *daemon) handleRequest(req request) (interface{}, error) {
	if !d.isConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.connect(ctx); err != nil {
			return nil, err
		}
	}
	d.mu.Lock()
	handler := d.handler
	d.mu.Unlock()
	if handler == nil {
		handler = GenericHandler{}
	}

	switch req.Cmd {
	case "status":
		return d.gatherStatus()
	case "battery":
		return d.readBattery()
	case "capabilities":
		return handler.Capabilities(), nil
	case "set":
		action, _ := req.Args["action"].(string)
		params := decodeParams(req.Args["params"])
		expect := decodeParams(req.Args["expect"])
		dbg("SET action=%q params=%x expect=%x\n", action, params, expect)
		// Route through the model handler for validation/special handling
		got, err := handler.HandleSet(d, action, params, expect)
		if err != nil {
			return nil, err
		}
		if len(got) == 0 {
			return nil, nil
		}
		return bytesJSON(got), nil
	case "find":
		// Start the indicator blink, then stop it by itself. Leaving the LED on
		// forever would drain the case and be confusing, and there is no user
		// watching the panel to press a second button.
		if _, err := handler.HandleSet(d, "find", []byte{0x01}, nil); err != nil {
			return nil, err
		}
		go func() {
			time.Sleep(findBlinkDuration)
			d.mu.Lock()
			still := d.connected
			d.mu.Unlock()
			if !still {
				return
			}
			if _, err := d.setAction("find", []byte{0x00}, nil); err != nil {
				dbg("find auto-stop failed: %v\n", err)
			}
		}()
		return nil, nil
	case "eq":
		// Current 0x22 payload, decoded: {slot, masterGain (dB), bands, payload (hex)}.
		return d.eqRead()
	case "eqApply":
		// Read-modify-write of 0x22 under one txMu, with a backup of the
		// pre-write payload. Args, all optional (none = rewrite unchanged):
		//   gains      [10]int  band gains, centi-dB
		//   masterGain int      master gain, centi-dB
		//   slot       int      target slot byte
		op, err := eqOpFromArgs(req.Args)
		if err != nil {
			return nil, err
		}
		return d.eqApply(op)
	case "eqMasterGain":
		// Convenience over eqApply: {masterGain: centi-dB} is required.
		if _, ok := req.Args["masterGain"]; !ok {
			return nil, errors.New("eqMasterGain: masterGain is required")
		}
		op, err := eqOpFromArgs(map[string]interface{}{"masterGain": req.Args["masterGain"]})
		if err != nil {
			return nil, err
		}
		return d.eqApply(op)
	case "wear":
		// Read 0x2C, set byte 0 (state 1 = on, 2 = off), keep the remaining
		// bytes, write, confirm. Returns the confirmed payload.
		st, ok := req.Args["state"].(float64)
		if !ok || (st != 1 && st != 2) {
			return nil, errors.New("wear: state must be 1 (on) or 2 (off)")
		}
		got, err := d.wearSet(byte(st))
		if err != nil {
			return nil, err
		}
		return bytesJSON(got), nil
	case "noise":
		// Raw 0x17 payload [mode, scene, level].
		p, err := d.query(0x17, 3*time.Second)
		if err != nil {
			return nil, err
		}
		return bytesJSON(p), nil
	case "keys":
		k, err := d.readKeys()
		if err != nil {
			return nil, err
		}
		return bytesJSON(k), nil
	case "keysSet":
		raw := decodeParams(req.Args["raw"])
		return nil, d.writeKeys(raw)
	case "disconnect":
		d.mu.Lock()
		if d.connected {
			handler.OnDisconnect(d)
			_ = d.dev.Disconnect()
			d.connected = false
			d.modelID = ""
			d.handler = GenericHandler{}
		}
		d.mu.Unlock()
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown command: %s", req.Cmd)
	}
}

func (d *daemon) gatherStatus() (map[string]interface{}, error) {
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}

	rep := map[string]interface{}{}
	if left, right, box, lc, rc, cc, err := d.readBatteryRaw(); err == nil {
		rep["battery"] = map[string]interface{}{
			"left": left, "right": right, "case": box,
			"leftCharging": lc, "rightCharging": rc, "caseCharging": cc,
			"casePresent": box != 0x00 && box != 0x7f,
		}
	}
	if fw := d.readVersionRaw(); len(fw) > 0 {
		rep["firmwareBytes"] = bytesJSON(fw)
	}
	if k, err := d.readKeys(); err == nil && len(k) > 0 {
		rep["keys"] = bytesJSON(k)
	}
	if snap := d.capabilitiesSnapshot(); len(snap) > 0 {
		caps := map[string]interface{}{}
		for k, v := range snap {
			caps[k] = v
		}
		rep["capabilities"] = caps
	}
	if p, err := d.query(0x2D, 800*time.Millisecond); err == nil && len(p) >= 1 {
		rep["spatial"] = bytesJSON(p)
	}

	// ANC
	if p, err := d.query(0x17, 1500*time.Millisecond); err == nil && len(p) >= 3 {
		rep["anc"] = map[string]interface{}{"modeRaw": int(p[0]), "sceneRaw": int(p[1]), "value": int(p[2])}
	}

	// Toggles
	toggles := map[string]byte{"game": 0x09, "sleep": 0x10, "ldac": 0x23, "multipoint": 0x24, "wind": 0x2A}
	togRep := map[string]bool{}
	for name, op := range toggles {
		if p, err := d.query(op, 1500*time.Millisecond); err == nil && len(p) >= 1 {
			togRep[name] = p[0] == 0x01
		}
	}
	if len(togRep) > 0 {
		rep["toggles"] = togRep
	}

	// Other queries (prompt-vol, auto-off, wear, balance, EQ)
	other := map[string]byte{
		"promptVol": 0x1D, "autoOff": 0x14, "wear": 0x2C, "balance": 0x16, "eq": 0x22,
	}
	for name, op := range other {
		if p, err := d.query(op, 1500*time.Millisecond); err == nil {
			rep[name] = bytesJSON(p)
		}
	}
	return rep, nil
}

func (d *daemon) readBatteryRaw() (left, right, box byte, lc, rc, cc bool, err error) {
	if d.batteryChar == nil {
		return 0, 0, 0, false, false, false, errors.New("battery characteristic not available")
	}
	// Battery notifies on 0008; we read the characteristic directly for a snapshot
	var buf [64]byte
	n, err := d.batteryChar.Read(buf[:])
	if err != nil {
		return 0, 0, 0, false, false, false, err
	}
	if n < 3 {
		return 0, 0, 0, false, false, false, errors.New("battery data too short")
	}
	data := buf[:n]
	return data[0], data[1], data[2], data[0]&0x80 != 0, data[1]&0x80 != 0, data[2]&0x80 != 0, nil
}

func (d *daemon) readBattery() (map[string]interface{}, error) {
	left, right, box, lc, rc, cc, err := d.readBatteryRaw()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"left": left, "right": right, "case": box,
		"leftCharging": lc, "rightCharging": rc, "caseCharging": cc,
		"casePresent": box != 0x00 && box != 0x7f,
	}, nil
}

func (d *daemon) setAction(action string, params, expect []byte) ([]byte, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	opcodeMap := map[string]byte{
		"anc": 0x17, "game": 0x09, "sleep": 0x10, "ldac": 0x23, "multipoint": 0x24,
		"wind": 0x2A, "promptVol": 0x1D, "autoOff": 0x14, "wear": 0x2C, "balance": 0x16,
		// Wired so a model whose probe answers these can actually be driven by
		// them; on units that stay silent the capability is false and the UI
		// never offers the control.
		"spatial": 0x2D, "fit": 0x11,
		// Find my earbuds = make the indicator LED blink. An audible tone is
		// useless here: a lost earbud is by definition not in your ear, so it
		// could never play anything you could hear. 0x05 is Quicky's
		// NewLightFlashCommand and is what actually lets you spot them.
		"find": 0x05,
	}
	// Refused on purpose — writing 0x23 REBOOTS the MeloBuds N70, and unlike
	// multipoint below no documentation sanctions that restart, so it is a
	// firmware fault rather than expected behaviour. Multipoint (0x24) also
	// restarts the earbuds and that one is documented: the product notes state
	// the earphones restart each time dual-device connection is toggled. Do not
	// "fix" that.
	//
	// ldac stays out of optionalCmds too — probing it would advertise a control
	// whose only effect is rebooting the user's hardware. It is pinned false by
	// readOnlyCmds, while gatherStatus still reads its state for display.
	if action == "ldac" {
		return nil, errors.New("ldac is disabled: writing it reboots this N70 and, unlike multipoint, the restart is not documented behaviour")
	}
	// Answering a query is not the same as honouring a write. promptVol is in
	// this list because the earbuds report a perfectly plausible maximum and
	// then discard every value sent to it; writing it would leave the user's
	// volume changed on screen and unchanged in the hardware.
	if reason, readOnly := readOnlyCmds[action]; readOnly {
		return nil, fmt.Errorf("%s is read-only on this device: %s", action, reason)
	}
	// 0x22 has exactly one write path, eqApply, which backs the slot up first.
	if action == "eq" {
		return nil, errors.New("eq cannot be written as a raw action: use eqApply, which reads, patches and backs the payload up first")
	}
	opcode, ok := opcodeMap[action]
	if !ok {
		return nil, fmt.Errorf("unknown action: %s", action)
	}

	if !d.isConnected() {
		return nil, errors.New("not connected")
	}

	d.drain()
	if err := d.send(opcode, params); err != nil {
		return nil, err
	}

	// Two modes:
	//
	//   expect == nil — apply-and-go. Toggles and ANC do not reliably echo an
	//     ack, and blocking the client for seconds on every click is what made
	//     the panel feel stuck, so give the ack a short grace period and return.
	//
	//   expect != nil — readback-confirm, for the setters whose whole point is
	//     a known end state (balance, prompt volume, auto-off). Mirrors the
	//     direct client's setConfirm: keep re-querying until the device reports
	//     the wanted value, because it often emits the pre-change state first.
	if len(expect) > 0 {
		got, err := d.awaitValue(opcode, expect, 3*time.Second)
		if err != nil {
			return nil, err
		}
		return got, nil
	}
	_, _ = d.wait(opcode, 250*time.Millisecond)
	return nil, nil
}

// awaitValue re-queries an opcode until the device reports the expected payload
// or the deadline passes. These earbuds answer a query right after a write with
// the value from BEFORE the change, so a single read is not a confirmation.
//
// The match is a PREFIX comparison: the device echoes the bytes we wrote
// followed by extra state (prompt volume returns value+max, auto-off returns
// reserve+current), so demanding an exact length would never confirm.
func (d *daemon) awaitValue(opcode byte, want []byte, timeout time.Duration) ([]byte, error) {
	return d.awaitMatch(opcode, func(p []byte) bool { return bytes.HasPrefix(p, want) },
		fmt.Sprintf("%x", want), timeout)
}

// awaitMatch is awaitValue with an arbitrary predicate; wantDesc is only used in
// the mismatch error. Callers hold txMu.
func (d *daemon) awaitMatch(opcode byte, match func([]byte) bool, wantDesc string, timeout time.Duration) ([]byte, error) {
	deadline := time.After(timeout)
	requery := time.After(400 * time.Millisecond)
	queries := 0
	var last []byte
	for {
		select {
		case f := <-d.frames:
			if f.cmd == opcode && match(f.params) {
				return f.params, nil
			} else if f.cmd == opcode {
				last = f.params
			}
		case <-requery:
			if queries < 5 {
				_ = d.send(0xFE, []byte{opcode})
				queries++
				requery = time.After(400 * time.Millisecond)
			}
		case <-deadline:
			return nil, fmt.Errorf("readback mismatch for 0x%02x: want %s, device reported %x (the device may nevertheless have applied the value; re-read to check)", opcode, wantDesc, last)
		}
	}
}

func (d *daemon) isConnected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

// eqOpFromArgs decodes the eqApply arguments. Values are validated again by
// qcycore.ApplyEq; this only rejects shapes JSON cannot express safely.
func eqOpFromArgs(args map[string]interface{}) (qcycore.EqOp, error) {
	var op qcycore.EqOp
	whole := func(v interface{}, what string) (int, error) {
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 1e6 {
			return 0, fmt.Errorf("eq: %s must be a whole number", what)
		}
		return int(f), nil
	}
	if v, ok := args["masterGain"]; ok {
		c, err := whole(v, "masterGain")
		if err != nil {
			return op, err
		}
		op.MasterCenti = &c
	}
	if v, ok := args["slot"]; ok {
		n, err := whole(v, "slot")
		if err != nil {
			return op, err
		}
		op.Slot = &n
	}
	if v, ok := args["gains"]; ok {
		arr, ok := v.([]interface{})
		if !ok || len(arr) != qcycore.EqBands {
			return op, fmt.Errorf("eq: gains must be an array of %d numbers", qcycore.EqBands)
		}
		var g [qcycore.EqBands]int
		for i, e := range arr {
			c, err := whole(e, fmt.Sprintf("gains[%d]", i))
			if err != nil {
				return op, err
			}
			g[i] = c
		}
		op.Gains = &g
	}
	return op, nil
}

// eqRead returns the active slot's payload, decoded.
func (d *daemon) eqRead() (*qcycore.EqSnapshot, error) {
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}
	p, err := d.query(qcycore.EqOpcode, 3*time.Second)
	if err != nil {
		return nil, err
	}
	snap, err := qcycore.DecodeEq(p)
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// eqApply is the only path that writes 0x22. Under ONE txMu it reads the live
// payload, patches only what op names, saves the pre-write payload as a backup
// (the write is aborted if that fails), writes, and confirms by readback.
// Holding the lock across the whole sequence is what keeps a concurrent status
// sweep or probe from draining the echo, or from interleaving its own write
// between our read and our write.
func (d *daemon) eqApply(op qcycore.EqOp) (*qcycore.EqResult, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}
	cur, err := d.queryLocked(qcycore.EqOpcode, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("eq: read current payload: %w", err)
	}
	next, err := qcycore.ApplyEq(cur, op)
	if err != nil {
		return nil, err
	}
	bk, err := qcycore.SaveEqBackup(cur)
	if err != nil {
		return nil, fmt.Errorf("eq: backup failed, nothing was written: %w", err)
	}
	d.drain()
	if err := d.send(qcycore.EqOpcode, next); err != nil {
		return nil, err
	}
	got, err := d.awaitMatch(qcycore.EqOpcode, qcycore.EqMatch(next, op), fmt.Sprintf("%x", next), 4*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w (previous payload saved at %s)", err, bk.History)
	}
	snap, err := qcycore.DecodeEq(got)
	if err != nil {
		return nil, err
	}
	return &qcycore.EqResult{
		EqSnapshot:    snap,
		Previous:      hex.EncodeToString(cur),
		BackupFirst:   bk.First,
		BackupHistory: bk.History,
	}, nil
}

// wearSet reads 0x2C, replaces byte 0 with state and writes the rest back
// unchanged, under one txMu.
func (d *daemon) wearSet(state byte) ([]byte, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}
	cur, err := d.queryLocked(0x2C, 2*time.Second)
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
	d.drain()
	if err := d.send(0x2C, set); err != nil {
		return nil, err
	}
	return d.awaitMatch(0x2C, func(p []byte) bool {
		return len(p) >= len(set) && p[0] == state && bytes.Equal(p[1:len(set)], set[1:])
	}, fmt.Sprintf("%x", set), 3*time.Second)
}

// probeCapabilities asks the device about the optional commands once per
// connection and records which ones it answers.
//
// The command space belongs to the protocol rather than to any one model, so a
// fixed per-model feature list would be wrong for every earbud that is not the
// one it was written against. Asking is both more accurate and how the
// hardware itself reports: a unit that does not have a feature simply does not
// reply to its query.
//
// Only read-only queries are sent. Nothing here writes to the earbuds.
//
// validate, when set, rejects a reply that answers but does not describe a
// usable setting — so a command that replies with a shape we cannot drive is
// reported as absent rather than advertised as a dead control.
var optionalCmds = []struct {
	action   string
	opcode   byte
	validate func([]byte) bool
}{
	{"spatial", 0x2D, nil},
	{"wear", 0x2C, nil},
	{"fit", 0x11, nil},
	{"ancLevel", 0x07, nil},
	{"promptVol", 0x1D, sanePromptVolume},
}

// readOnlyCmds are commands that answer a query yet must not be written.
//
// A query cannot tell us whether a setter works: the earbuds happily report
// their prompt-volume maximum of 15 and then ignore every value written to it.
// That reply is indistinguishable from a working setter that simply happens to
// be at 6, so the only sound read-only test is liveness, and liveness is not
// writability.
//
// Deciding by writing a probe value is not an option — a probe that changes
// user state on every connection is worse than the broken control it replaces.
// So writability is recorded here from the measured behaviour of this hardware
// rather than inferred, and the query is left to answer the question it can
// actually answer: does this unit have the command at all.
var readOnlyCmds = map[string]string{
	// 0x1D reads back 06 0f (value 6, max 15) for every value written. See
	// PROTOCOL.md, "Prompt volume".
	"promptVol": "reports a sane maximum but ignores every value written",
	// 0x23 reboots the earbuds on write and nothing documents the restart.
	"ldac": "writing it reboots the earbuds (undocumented firmware fault)",
}

// sanePromptVolume checks the 0x1D reply shape: [value, max]. This is NOT a
// writability test — the hardware passes it while ignoring every write — it
// only rejects a reply so malformed that there would be no slider range to
// draw.
func sanePromptVolume(p []byte) bool {
	if len(p) < 2 {
		return false
	}
	max := int(p[1])
	return max > 0 && max <= 255 && int(p[0]) <= max
}

// capabilitiesSnapshot copies the probe results under the lock. Reading the map
// directly raced the background probe, which writes it entry by entry while a
// status request is walking it.
func (d *daemon) capabilitiesSnapshot() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.caps) == 0 {
		return nil
	}
	out := make(map[string]bool, len(d.caps))
	for k, v := range d.caps {
		out[k] = v
	}
	return out
}

// Each probe query takes txMu itself, so it serialises against status reads
// without holding the lock across the whole sweep — Go mutexes are not
// reentrant, so holding it here and then calling query would deadlock.
func (d *daemon) probeCapabilities() {
	d.mu.Lock()
	if d.caps == nil {
		d.caps = map[string]bool{}
	}
	d.caps["anc"] = true
	d.caps["game"] = true
	d.caps["sleep"] = true
	d.caps["multipoint"] = true
	d.caps["balance"] = true
	d.caps["autoOff"] = true
	d.caps["eq"] = true
	d.caps["wind"] = true
	d.caps["keys"] = d.keysChar != nil

	// Commands that exist but must not be driven. Set false up front so a
	// failure to answer below cannot leave them looking available.
	for action := range readOnlyCmds {
		d.caps[action] = false
	}
	d.mu.Unlock()

	// Read-only queries only. The timeout is short on purpose: these are
	// best-effort capability hints, and a model without the feature just stays
	// silent, so there is no reason to wait long for an answer that is not
	// coming.
	for _, c := range optionalCmds {
		if _, readOnly := readOnlyCmds[c.action]; readOnly {
			continue // answering the query is not what makes it usable
		}
		p, err := d.query(c.opcode, 350*time.Millisecond)
		ok := err == nil
		if ok && c.validate != nil {
			ok = c.validate(p)
		}

		d.mu.Lock()
		d.caps[c.action] = ok
		d.mu.Unlock()
	}
}

func (d *daemon) readKeys() ([]byte, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	return d.readKeysLocked()
}

func (d *daemon) readKeysLocked() ([]byte, error) {
	if d.keysChar == nil {
		return nil, errors.New("keys characteristic 000D not available on this device")
	}
	var buf [64]byte
	n, err := d.keysChar.Read(buf[:])
	if err != nil {
		return nil, fmt.Errorf("read keys 000D: %w", err)
	}
	return append([]byte(nil), buf[:n]...), nil
}

// writeKeys writes the remap table and reads it back, because the device sends
// no ack for this characteristic — the readback is the only confirmation.
func (d *daemon) writeKeys(raw []byte) error {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if d.keysChar == nil {
		return errors.New("keys characteristic 000D not available on this device")
	}
	if len(raw) == 0 || len(raw)%2 != 0 {
		return fmt.Errorf("keys set: payload must be non-empty keyId:funId pairs, got %d bytes", len(raw))
	}
	if _, err := d.keysChar.WriteWithoutResponse(raw); err != nil {
		return fmt.Errorf("keys set write 000D: %w", err)
	}
	time.Sleep(150 * time.Millisecond)
	got, err := d.readKeysLocked()
	if err != nil {
		return err
	}
	if len(got) < len(raw) || !bytes.Equal(got[:len(raw)], raw) {
		return fmt.Errorf("keys set readback mismatch: want %x got %x", raw, got)
	}
	return nil
}

// listenSocket binds the daemon socket. A live daemon is never displaced: if
// anything answers on the path, it returns an error. A leftover socket file
// nobody answers on is removed first. The socket is chmod 0600.
func listenSocket() (net.Listener, string, error) {
	sock, err := qcycore.SocketPath(true)
	if err != nil {
		return nil, "", err
	}
	if c, err := net.DialTimeout("unix", sock, 500*time.Millisecond); err == nil {
		_ = c.Close()
		return nil, "", fmt.Errorf("qcyctld is already running (%s answers); not taking it over", sock)
	}
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, "", fmt.Errorf("%s exists and is not a socket; refusing to remove it", sock)
		}
		if err := os.Remove(sock); err != nil {
			return nil, "", fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, "", err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = l.Close()
		_ = os.Remove(sock)
		return nil, "", fmt.Errorf("chmod socket: %w", err)
	}
	return l, sock, nil
}

func handleConn(conn net.Conn, d *daemon) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(response{OK: false, Error: "invalid JSON: " + err.Error()})
			continue
		}
		data, err := d.handleRequest(req)
		if err != nil {
			_ = enc.Encode(response{OK: false, Error: err.Error()})
		} else {
			_ = enc.Encode(response{OK: true, Data: data})
		}
	}
}

func main() {
	scanSeconds := 15
	if len(os.Args) > 1 {
		if v, err := strconv.Atoi(os.Args[1]); err == nil {
			scanSeconds = v
		}
	}

	l, sock, err := listenSocket()
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(sock)
	defer l.Close()

	d := &daemon{scanSeconds: scanSeconds}

	// NO auto-connect on startup. Lazy connect on first request.
	// NO periodic reconnect ticker — plugin drives via refresh() on connect.

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go handleConn(conn, d)
	}
}
