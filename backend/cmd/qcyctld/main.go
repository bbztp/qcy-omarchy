package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	FeatureANC           FeatureFlag = "anc"           // Adaptive ANC with scenes
	FeatureGameMode      FeatureFlag = "gameMode"      // Low latency game mode
	FeatureSleepMode     FeatureFlag = "sleepMode"     // Sleep timer
	FeatureLDAC          FeatureFlag = "ldac"          // LDAC codec toggle
	FeatureMultipoint    FeatureFlag = "multipoint"    // Multipoint toggle
	FeatureWindNoise     FeatureFlag = "windNoise"     // Wind noise reduction
	FeatureWearDetect    FeatureFlag = "wearDetect"    // In-ear detection
	FeatureAutoOff       FeatureFlag = "autoOff"       // Auto power-off timer
	FeatureBalance       FeatureFlag = "balance"       // L/R balance
	FeaturePromptVolume  FeatureFlag = "promptVolume"  // Prompt volume
	FeatureFind          FeatureFlag = "find"          // Find my earbuds
	FeatureBatteryCase   FeatureFlag = "batteryCase"   // Case battery level
	FeatureKeysRemap     FeatureFlag = "keysRemap"     // Touch key remapping
	FeatureEQ            FeatureFlag = "eq"            // EQ presets (read-only)
)

// ActionSpec describes a settable action for the UI.
type ActionSpec struct {
	ID          string      `json:"id"`                    // unique action ID
	Label       string      `json:"label"`                 // user-visible label
	Type        string      `json:"type"`                  // "toggle" | "enum" | "trigger" | "slider"
	Options     []string    `json:"options,omitempty"`     // for enum: ["off", "adaptive", ...]
	Min         int         `json:"min,omitempty"`         // for slider
	Max         int         `json:"max,omitempty"`
	Step        int         `json:"step,omitempty"`
	Opcode      byte        `json:"-"`                     // internal opcode
	ParamMap    map[string]byte `json:"-"`                 // option -> param byte
	ReadOpcode  byte        `json:"-"`                     // opcode to read current state
	ReadParser  string      `json:"-"`                     // parser name for read response
}

// Capabilities describes what a model supports and how to present it.
type Capabilities struct {
	ModelID      string         `json:"modelId"`      // e.g., "melobuds-n70"
	DisplayName  string         `json:"displayName"`  // e.g., "QCY MeloBuds N70"
	Features     []FeatureFlag  `json:"features"`     // supported features
	Actions      []ActionSpec   `json:"actions"`      // settable actions
	BatteryChars []string       `json:"batteryChars"` // "left", "right", "case"
}

// ModelHandler defines the interface each model implements.
type ModelHandler interface {
	Capabilities() Capabilities
	// HandleSet executes a setter action. Return nil on success.
	HandleSet(d *daemon, actionID string, params []byte) error
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
	return Capabilities{
		ModelID:     "generic",
		DisplayName: "QCY Earbuds (Generic)",
		Features:    []FeatureFlag{FeatureBatteryCase},
		Actions: []ActionSpec{
			{ID: "find", Label: "Find Earbuds", Type: "trigger", Opcode: 0x05},
		},
		BatteryChars: []string{"left", "right", "case"},
	}
}

func (GenericHandler) HandleSet(d *daemon, actionID string, params []byte) error {
	if actionID == "find" {
		return d.setAction("find", []byte{0x01})
	}
	return fmt.Errorf("generic handler: unknown action %s", actionID)
}

func (GenericHandler) HandleRead(d *daemon, actionID string) (interface{}, error) {
	return nil, fmt.Errorf("generic handler: unknown read %s", actionID)
}

func (GenericHandler) OnConnect(d *daemon) error  { return nil }
func (GenericHandler) OnDisconnect(d *daemon)     {}

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
				Opcode: 0x17,
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
			{ID: "promptVolume", Label: "Prompt Volume", Type: "slider", Min: 0, Max: 255, Step: 1, Opcode: 0x1D, ReadOpcode: 0x1D},
			{ID: "find", Label: "Find Earbuds", Type: "trigger", Opcode: 0x05},
		},
		BatteryChars: []string{"left", "right", "case"},
	}
}

func (meloBudsN70Handler) HandleSet(d *daemon, actionID string, params []byte) error {
	// Map handler action IDs to daemon action IDs
	daemonAction := map[string]string{
		"gameMode":      "game",
		"sleepMode":     "sleep",
		"windNoise":     "wind",
		"wearDetect":    "wear",
		"autoOff":       "autoOff",
		"promptVolume":  "promptVol",
		"balance":       "balance",
		"anc":           "anc",
		"ldac":          "ldac",
		"multipoint":    "multipoint",
		"find":          "find",
	}[actionID]
	if daemonAction == "" {
		daemonAction = actionID
	}
	return d.setAction(daemonAction, params)
}

func (meloBudsN70Handler) HandleRead(d *daemon, actionID string) (interface{}, error) {
	return nil, nil // handled by generic gatherStatus
}

func (meloBudsN70Handler) OnConnect(d *daemon) error {
	// Subscribe to battery notifications (0008) for live updates
	if d.batteryChar != nil {
		return d.batteryChar.EnableNotifications(func(buf []byte) {
			// Could push to clients via WebSocket/event stream in future
			_ = buf
		})
	}
	return nil
}

func (meloBudsN70Handler) OnDisconnect(d *daemon) {}

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

func saveCachedAddr(addr bluetooth.Address) {
	p := cacheFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	random := "0"
	if addr.IsRandom() {
		random = "1"
	}
	_ = os.WriteFile(p, []byte(addr.String()+" "+random+"\n"), 0o644)
}

func (d *daemon) connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.connected {
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
			if err := d.setupDevice(dev); err == nil {
				saveCachedAddr(addr)
				d.connected = true
				return nil
			}
			_ = dev.Disconnect()
		}
	}

	// Scan path
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

	if err := d.setupDevice(dev); err != nil {
		_ = dev.Disconnect()
		return err
	}
	saveCachedAddr(addr)
	d.connected = true
	return nil
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

	d.dev = dev
	d.notifyCh = make(chan []byte, 16)
	d.frames = make(chan frame, 64)

	if err := d.notifyChar.EnableNotifications(func(buf []byte) {
		d.notifyCh <- append([]byte(nil), buf...)
	}); err != nil {
		return fmt.Errorf("enable notifications 1002: %w", err)
	}

	go d.readLoop()

	// Detect model and attach handler
	d.detectAndAttachModel()
	return nil
}

func (d *daemon) detectAndAttachModel() {
	// Try firmware query (0x07) for model string
	modelID := d.queryModelFromFirmware()
	if modelID == "" {
		// Fallback: try BlueZ device name (would need D-Bus; skip for now)
		modelID = "generic"
	}
	d.modelID = modelID
	d.handler = GetHandler(modelID)
	_ = d.handler.OnConnect(d)
}

func (d *daemon) queryModelFromFirmware() string {
	if d.versionChar == nil {
		return ""
	}
	// Query firmware version (opcode 0x07)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan string, 1)
	go func() {
		p, err := d.query(0x07, 1500*time.Millisecond)
		if err != nil || len(p) == 0 {
			done <- ""
			return
		}
		// Parse firmware string for model hints
		fw := string(p)
		model := detectModelFromFirmware(fw)
		done <- model
	}()
	select {
	case model := <-done:
		return model
	case <-ctx.Done():
		return ""
	}
}

func detectModelFromFirmware(fw string) string {
	fw = strings.ToLower(fw)
	switch {
	case strings.Contains(fw, "melobuds") || strings.Contains(fw, "n70"):
		return "melobuds-n70"
	case strings.Contains(fw, "t13") || strings.Contains(fw, "ht10"):
		return "t13"
	case strings.Contains(fw, "ht03") || strings.Contains(fw, "a10"):
		return "ht03"
	case strings.Contains(fw, "t20") || strings.Contains(fw, "melobuds pro"):
		return "melobuds-pro"
	}
	return ""
}

func findChar(s bluetooth.DeviceService, uuid bluetooth.UUID) *bluetooth.DeviceCharacteristic {
	c, err := s.DiscoverCharacteristics([]bluetooth.UUID{uuid})
	if err != nil || len(c) != 1 {
		return nil
	}
	return &c[0]
}

func (d *daemon) markDisconnected() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.connected {
		d.connected = false
		if d.notifyCh != nil {
			close(d.notifyCh)
			d.notifyCh = nil
		}
	}
}

func (d *daemon) readLoop() {
	for buf := range d.notifyCh {
		frames, _ := parseFrames(buf)
		for _, f := range frames {
			d.frames <- f
		}
	}
	d.markDisconnected()
}

func parseFrames(buf []byte) ([]frame, error) {
	var out []frame
	i := 0
	for i < len(buf) {
		if i+1 >= len(buf) {
			return out, errors.New("truncated frame")
		}
		if buf[i] != 0xFF {
			return out, fmt.Errorf("expected 0xFF at %d, got 0x%02x", i, buf[i])
		}
		bodyLen := int(buf[i+1])
		if i+2+bodyLen > len(buf) {
			return out, errors.New("frame exceeds buffer")
		}
		opcode := buf[i+2]
		paramLen := bodyLen - 2
		var params []byte
		if paramLen > 0 {
			params = buf[i+3 : i+3+paramLen]
		}
		out = append(out, frame{cmd: opcode, params: params})
		i += 2 + bodyLen
	}
	return out, nil
}

func buildFrame(opcode byte, params []byte) []byte {
	bodyLen := len(params) + 2
	pkt := make([]byte, 3+bodyLen)
	pkt[0] = 0xFF
	pkt[1] = byte(bodyLen)
	pkt[2] = 0xFE
	pkt[3] = opcode
	copy(pkt[4:], params)
	return pkt
}

func (d *daemon) send(opcode byte, params []byte) error {
	pkt := buildFrame(opcode, params)
	_, err := d.cmdChar.WriteWithoutResponse(pkt)
	if err == nil {
		return nil
	}
	_, err2 := d.cmdChar.Write(pkt)
	if err2 != nil {
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

func (d *daemon) query(opcode byte, timeout time.Duration) ([]byte, error) {
	d.drain()
	if err := d.send(0xFE, []byte{opcode}); err != nil {
		return nil, err
	}
	return d.wait(opcode, timeout)
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
	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.connect(ctx); err != nil {
			return nil, err
		}
	} else {
		d.mu.Unlock()
	}

	switch req.Cmd {
	case "status":
		return d.gatherStatus()
	case "battery":
		return d.readBattery()
	case "capabilities":
		return d.handler.Capabilities(), nil
	case "set":
		action, _ := req.Args["action"].(string)
		paramsRaw, _ := req.Args["params"].([]interface{})
		var params []byte
		for _, p := range paramsRaw {
			if f, ok := p.(float64); ok {
				params = append(params, byte(f))
			}
		}
		// Route through model handler for validation/special handling
		return nil, d.handler.HandleSet(d, action, params)
	case "find":
		return nil, d.handler.HandleSet(d, "find", []byte{0x01})
	case "disconnect":
		d.mu.Lock()
		if d.connected {
			d.handler.OnDisconnect(d)
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
	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		return nil, errors.New("not connected")
	}
	d.mu.Unlock()

	rep := map[string]interface{}{}
	if left, right, box, lc, rc, cc, err := d.readBatteryRaw(); err == nil {
		rep["battery"] = map[string]interface{}{
			"left": left, "right": right, "case": box,
			"leftCharging": lc, "rightCharging": rc, "caseCharging": cc,
			"casePresent": box != 0x00 && box != 0x7f,
		}
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
			rep[name] = p
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

func (d *daemon) setAction(action string, params []byte) error {
	opcodeMap := map[string]byte{
		"anc": 0x17, "game": 0x09, "sleep": 0x10, "ldac": 0x23, "multipoint": 0x24,
		"wind": 0x2A, "promptVol": 0x1D, "autoOff": 0x14, "wear": 0x2C, "balance": 0x16,
		"eq": 0x22, "find": 0x05,
	}
	opcode, ok := opcodeMap[action]
	if !ok {
		return fmt.Errorf("unknown action: %s", action)
	}

	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		return errors.New("not connected")
	}
	d.mu.Unlock()

	d.drain()
	if err := d.send(opcode, params); err != nil {
		return err
	}

	// For setters that need confirmation (most do), wait for the opcode response
	// Use a short timeout; if no confirmation, assume it went through (optimistic)
	_, _ = d.wait(opcode, 3*time.Second)
	return nil
}

func socketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, "qcyctld.sock")
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

	sock := socketPath()
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
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