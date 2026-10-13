package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
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

	"github.com/bbztp/qcy-omarchy/internal/protocol"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
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
	// connMu serialises connect/close of the BLE session so that a slow scan
	// does not have to hold mu: status reads, markDisconnected and the
	// capability snapshot only need mu, and must not stall behind a scan.
	connMu sync.Mutex
	// mu guards connection state (connected, sess, caps, handler, modelID, the
	// idle timer and the status cache). Never held across BLE I/O or a sleep.
	mu         sync.Mutex
	dialer     dialer
	sess       session
	frames     chan frame
	connected  bool
	deviceName string
	// fitCancel aborts a running fit test (guarded by mu).
	fitCancel chan struct{}
	// vendorID is the manufacturer-data vendorId of the connected unit, when
	// the dialer could learn it (scan or cache). It decides the model.
	vendorID   uint16
	haveVendor bool

	// Model detection & routing
	caps   map[string]bool
	probed bool
	// probing is true while the background capability sweep runs; a status
	// read waits for it so the first report is never missing the probe results.
	probing bool
	// txMu serialises BLE transactions. The frame channel is shared, so a
	// background capability probe interleaving with a status read would drain
	// the other one's replies and both would time out.
	txMu    sync.Mutex
	modelID string
	handler ModelHandler

	// Connection lifecycle (see lifecycle.go).
	cfg         lifecycleConfig
	now         func() time.Time
	sleep       func(time.Duration)
	afterFunc   func(time.Duration, func()) idleStopper // time.AfterFunc unless a test injects one
	inflight    int                                     // requests and background holds in progress
	idleGen     uint64
	idleTimer   idleStopper
	busyUntil   time.Time
	closing     bool   // set at shutdown: no new link may be opened (guarded by mu)
	roundSeq    uint64 // connect rounds finished; lets queued requests share a failed round
	lastErr     error  // error of the last failed round (nil after a success)
	writeGen    uint64 // bumped by every write; guards the status cache
	lastStatus  *snapshot
	lastBattery *snapshot

	// Generic panel functions (fn.go): readings cached per session, guarded by mu.
	fnSess  session
	fnCache map[int]fnReading
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
	// Code is a stable machine-readable error class ("busy"); empty for
	// ordinary errors, which clients tell apart by Error alone.
	Code string `json:"code,omitempty"`
	// Sent, DeviceReported and Backup accompany code "unconfirmed": the payload
	// written and the one the device reported (hex), and the path of the
	// pre-write payload backup.
	Sent           string `json:"sent,omitempty"`
	DeviceReported string `json:"deviceReported,omitempty"`
	Backup         string `json:"backup,omitempty"`
}

// errorResponse builds the failure response for err, carrying the machine
// code and, for an unconfirmed EQ write, the extra fields.
func errorResponse(err error) response {
	r := response{OK: false, Error: err.Error(), Code: responseCode(err)}
	var ee *qcycore.EqError
	if errors.As(err, &ee) && ee.Code == qcycore.CodeUnconfirmed {
		r.Sent = hex.EncodeToString(ee.Sent)
		r.DeviceReported = hex.EncodeToString(ee.Got)
		r.Backup = ee.Backup
	}
	return r
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
	FeatureFit          FeatureFlag = "fit"          // Ear-tip fit test (verified models only)
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
	// Verified is true only for models this tool was exercised against (the
	// verified-model set, see verifiedVendorIDs). Anything else runs on the
	// shared, conservative command set.
	Verified     bool   `json:"verified"`
	VendorID     int    `json:"vendorId,omitempty"`     // manufacturer-data vendorId, when known
	ProductTitle string `json:"productTitle,omitempty"` // product name from the Quicky database
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
			FeatureFind, FeatureBatteryCase,
		},
		Actions: []ActionSpec{
			{ID: "anc", Label: "ANC", Type: "enum", Opcode: protocol.OpANC},
			{ID: "gameMode", Label: "Game Mode", Type: "toggle", Opcode: protocol.OpGame},
			{ID: "sleepMode", Label: "Sleep Mode", Type: "toggle", Opcode: protocol.OpSleep},
			{ID: "ldac", Label: "LDAC", Type: "toggle", Opcode: protocol.OpLDAC},
			{ID: "multipoint", Label: "Multipoint", Type: "toggle", Opcode: protocol.OpMultipoint},
			{ID: "windNoise", Label: "Wind Noise Reduction", Type: "toggle", Opcode: protocol.OpWind},
			{ID: "wearDetect", Label: "Wear Detection", Type: "toggle", Opcode: protocol.OpWear},
			{ID: "autoOff", Label: "Auto Off", Type: "slider", Opcode: protocol.OpAutoOff},
			{ID: "balance", Label: "Balance", Type: "slider", Opcode: protocol.OpBalance},
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
// MeloBuds N70 handler. These behaviours were derived and verified on a QCY
// MeloBuds N70, the device used as the test model for this code; other models
// are unverified.
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
			FeatureEQ, FeatureKeysRemap, FeatureFit,
		},
		Actions: []ActionSpec{
			{
				ID: "anc", Label: "ANC Mode", Type: "enum",
				Options: []string{"off", "adaptive", "indoor", "commuting", "noisy", "antiwind", "transparency"},
				Opcode:  protocol.OpANC,
				ParamMap: map[string]byte{
					"off": 0x00, "adaptive": 0x01, "indoor": 0x01, "commuting": 0x02,
					"noisy": 0x03, "antiwind": 0x04, "transparency": 0x01,
				},
				ReadOpcode: protocol.OpANC, ReadParser: "anc",
			},
			{ID: "gameMode", Label: "Game Mode", Type: "toggle", Opcode: protocol.OpGame, ReadOpcode: protocol.OpGame},
			{ID: "sleepMode", Label: "Sleep Mode", Type: "toggle", Opcode: protocol.OpSleep, ReadOpcode: protocol.OpSleep},
			{ID: "ldac", Label: "LDAC", Type: "toggle", Opcode: protocol.OpLDAC, ReadOpcode: protocol.OpLDAC},
			{ID: "multipoint", Label: "Multipoint", Type: "toggle", Opcode: protocol.OpMultipoint, ReadOpcode: protocol.OpMultipoint},
			{ID: "windNoise", Label: "Wind Noise Reduction", Type: "toggle", Opcode: protocol.OpWind, ReadOpcode: protocol.OpWind},
			{ID: "wearDetect", Label: "Wear Detection", Type: "toggle", Opcode: protocol.OpWear, ReadOpcode: protocol.OpWear},
			{ID: "autoOff", Label: "Auto Off", Type: "slider", Min: 0, Max: 180, Step: 5, Opcode: protocol.OpAutoOff, ReadOpcode: protocol.OpAutoOff},
			{ID: "balance", Label: "Balance", Type: "slider", Min: 0, Max: 100, Step: 1, Opcode: protocol.OpBalance, ReadOpcode: protocol.OpBalance},
			// Max is the measured ceiling (0x1D reports max=15), not the byte
			// width — the payload is one byte but the scale is 0..15.
			{ID: "promptVolume", Label: "Prompt Volume", Type: "slider", Min: 4, Max: 10, Step: 2, Opcode: protocol.OpPromptVolume, ReadOpcode: protocol.OpPromptVolume},
			{ID: "wearSensitivity", Label: "Wear Sensitivity", Type: "slider", Min: 1, Max: 3, Step: 1, Opcode: protocol.OpWearSensitivity, ReadOpcode: protocol.OpWearSensitivity},
		},
		BatteryChars: []string{"left", "right", "case"},
		Verified:     true,
	}
}

// daemonActionID maps a handler action id onto the daemon's opcode-table key.
func daemonActionID(actionID string) string {
	if a, ok := map[string]string{
		"gameMode":     "game",
		"sleepMode":    "sleep",
		"windNoise":    "wind",
		"wearDetect":   "wear",
		"promptVolume": "promptVol",
	}[actionID]; ok {
		return a
	}
	return actionID
}

func (meloBudsN70Handler) HandleSet(d *daemon, actionID string, params, expect []byte) ([]byte, error) {
	return d.setAction(daemonActionID(actionID), params, expect)
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
	// "qcy-<vendorId>" ids are resolved against the product database on demand
	// rather than registered one by one.
	if v, ok := vendorFromModelID(modelID); ok {
		if h, ok := newProfileHandler(v); ok {
			return h
		}
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

// cacheEntry is one line of the le-address cache:
//
//	<addr> <random 0|1> [<name with _ for spaces>|-] [<vendorId decimal>]
//
// Older files stop after the address, or after the name; both still parse. The
// vendorId is last because the fast path never scans and so never sees the
// advertisement that carries it.
type cacheEntry struct {
	Addr       string
	Random     bool
	Name       string
	VendorID   uint16
	HaveVendor bool
}

func parseCacheEntry(content string) (cacheEntry, bool) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return cacheEntry{}, false
	}
	e := cacheEntry{Addr: fields[0]}
	if len(fields) > 1 && fields[1] == "1" {
		e.Random = true
	}
	if len(fields) > 2 && fields[2] != "-" {
		e.Name = strings.ReplaceAll(fields[2], "_", " ")
	}
	if len(fields) > 3 {
		if n, err := strconv.ParseUint(fields[3], 10, 16); err == nil {
			e.VendorID, e.HaveVendor = uint16(n), true
		}
	}
	return e, true
}

func formatCacheEntry(e cacheEntry) string {
	random := "0"
	if e.Random {
		random = "1"
	}
	line := e.Addr + " " + random
	name := strings.ReplaceAll(strings.Join(strings.Fields(e.Name), " "), " ", "_")
	if name != "" || e.HaveVendor {
		if name == "" {
			name = "-"
		}
		line += " " + name
	}
	if e.HaveVendor {
		line += " " + strconv.FormatUint(uint64(e.VendorID), 10)
	}
	return line + "\n"
}

func loadCacheEntry() (cacheEntry, bool) {
	b, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return cacheEntry{}, false
	}
	return parseCacheEntry(string(b))
}

func loadCachedAddr() (bluetooth.Address, bool) {
	e, ok := loadCacheEntry()
	if !ok {
		return bluetooth.Address{}, false
	}
	var addr bluetooth.Address
	addr.Set(e.Addr)
	if e.Random {
		addr.SetRandom(true)
	}
	if addr.MAC == (bluetooth.MAC{}) {
		return bluetooth.Address{}, false
	}
	return addr, true
}

// saveCachedAddr stores the address, the advertised name and (when the scan
// saw it) the vendorId, which is what model detection uses on the fast path.
func saveCachedAddr(addr bluetooth.Address, name string, vendor uint16, haveVendor bool) {
	p := cacheFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	line := formatCacheEntry(cacheEntry{
		Addr: addr.String(), Random: addr.IsRandom(), Name: name,
		VendorID: vendor, HaveVendor: haveVendor,
	})
	_ = os.WriteFile(p, []byte(line), 0o644)
}

// loadCachedName returns the name stored alongside a cached address.
func loadCachedName() string {
	e, _ := loadCacheEntry()
	return e.Name
}

// loadCachedVendor returns the vendorId stored alongside a cached address.
func loadCachedVendor() (uint16, bool) {
	e, _ := loadCacheEntry()
	return e.VendorID, e.HaveVendor
}

func (d *daemon) detectAndAttachModel() {
	// The advertised name is the authoritative signal; the 0007 firmware string
	// is only a corroborating hint (it is a bare "1.2.3" on this hardware).
	d.mu.Lock()
	name, vendor, haveVendor := d.deviceName, d.vendorID, d.haveVendor
	d.mu.Unlock()
	modelID := detectModel(vendor, haveVendor, name, "")
	if modelID == "generic" {
		modelID = detectModel(vendor, haveVendor, name, d.readVersionString())
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
	//
	// Once per daemon lifetime: the answers belong to the device, not to one
	// connection, and re-asking on every on-demand reconnect would only keep
	// the earbuds busy longer. The probe holds the connection (inflight) so the
	// idle timer cannot cut it off half way.
	if d.needsProbe() {
		d.beginRequest()
		d.mu.Lock()
		d.probing = true
		d.mu.Unlock()
		go func() {
			defer d.endRequest()
			defer func() {
				d.mu.Lock()
				d.probing = false
				d.mu.Unlock()
			}()
			d.probeCapabilities()
		}()
	}
}

// readVersionRaw reads characteristic 0007 (firmware) as raw bytes. It is a
// single GATT read — no command round-trip — so it is cheap enough to run on
// every connect. Returns nil when the characteristic is absent or unreadable.
func (d *daemon) readVersionRaw() []byte {
	s := d.curSession()
	if s == nil {
		return nil
	}
	var buf [8]byte
	n, err := s.Read(charVersion, buf[:])
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

// verifiedVendorIDs are the manufacturer-data vendorIds of the models whose
// behaviour was verified on hardware. Today that is only the QCY MeloBuds N70
// (the device used as the test model for this code; other models are
// unverified), and this set is the single place that names it.
// Exactly these two: the neighbouring ids are other products (23873/23878
// AilyBuds E50, 23874/23879 MeloBuds N60, 23875 CT03) and 23876 is not in the
// database, so a range would verify earbuds this tool was never tried on.
var verifiedVendorIDs = map[uint16]bool{23872: true, 23877: true}

// detectModel picks the model id. Precedence: the vendorId from the
// advertisement (the real identifier), then the advertised name, then the
// firmware string, then "generic". A vendorId the Quicky database knows but
// that is not a verified model becomes "qcy-<vendorId>", driven by a profile generated
// from the product entry, never by assumptions about the verified model.
func detectModel(vendorID uint16, haveVendor bool, name, version string) string {
	if haveVendor {
		if verifiedVendorIDs[vendorID] {
			return "melobuds-n70"
		}
		if _, ok := lookupProduct(vendorID); ok {
			return "qcy-" + strconv.Itoa(int(vendorID))
		}
	}
	if id := detectModelFromName(name); id != "generic" {
		return id
	}
	return detectModelFromVersion(version)
}

// detectModelFromName maps the advertised Bluetooth name onto a registered model
// id. This is the only source that actually identifies a model: the 0007
// characteristic holds a bare numeric firmware ("1.2.3") with no model string in
// it, so it cannot be used to tell a model from another.
func detectModelFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "melobuds n70"), strings.Contains(n, "n70"):
		return "melobuds-n70"
	}
	return "generic"
}

// dbg writes diagnostics to stderr when QCYCTL_DEBUG is set. Off by default:
// the per-frame chatter is far too noisy for a service log.
func dbg(format string, args ...interface{}) {
	if os.Getenv("QCYCTL_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

// parseFrames decodes a notification payload into frames (see
// protocol.ParseBlocks: a payload may carry several blocks).
func parseFrames(buf []byte) ([]frame, error) {
	blocks, err := protocol.ParseBlocks(buf)
	out := make([]frame, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, frame{cmd: b.Cmd, params: b.Params})
	}
	if len(out) == 0 {
		return nil, err
	}
	return out, err
}

// buildFrame encodes a command as FF <bodyLen> <opcode> <paramLen> <params...>
// (protocol.Block). A query is the same frame with the reserved 0xFE opcode.
func buildFrame(opcode byte, params []byte) []byte {
	return protocol.Block(opcode, params...)
}

func (d *daemon) send(opcode byte, params []byte) error {
	pkt := buildFrame(opcode, params)
	dbg("TX 0x%02x %x\n", opcode, pkt)
	sess := d.curSession()
	if sess == nil {
		return errors.New("not connected")
	}
	if err := sess.WriteCmd(pkt); err != nil {
		dbg("SET-FAIL 0x%02x: %v\n", opcode, err)
		d.markDisconnected()
		return fmt.Errorf("write 0x%02x: %v", opcode, err)
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
	if err := d.send(protocol.OpQuery, []byte{opcode}); err != nil {
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
				_ = d.send(protocol.OpQuery, []byte{opcode})
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
	// The request holds the connection for its whole duration; the idle window
	// starts when the last one ends.
	d.beginRequest()
	defer d.endRequest()

	if req.Cmd == "disconnect" {
		d.closeSession()
		return nil, nil
	}
	// Must not queue behind the fit test it is meant to cancel.
	if req.Cmd == "fitStop" {
		return nil, d.fitStopRequest()
	}
	// The capability list is static per model: answer it from what is already
	// known instead of grabbing the single BLE link for it.
	if req.Cmd == "capabilities" {
		if h := d.knownHandler(); h != nil {
			return h.Capabilities(), nil
		}
	}
	if err := d.ensureConnected(); err != nil {
		if isBusy(err) {
			if data, ok := d.staleFor(req.Cmd); ok {
				return data, nil
			}
		}
		return nil, err
	}
	// Any write may change what status/battery report: a cached reading from
	// before it must never be served as stale afterwards, whatever the outcome
	// (a failed write may still have reached the device).
	switch req.Cmd {
	case "set", "find", "eqApply", "eqMasterGain", "wear", "keysSet", "hearingMode", "hearingWrite", "windOffByte", "fnSet":
		defer d.invalidateCache()
	}
	d.mu.Lock()
	handler := d.handler
	d.mu.Unlock()
	if handler == nil {
		handler = GenericHandler{}
	}
	if err := gateCommand(handler, req.Cmd); err != nil {
		return nil, err
	}

	switch req.Cmd {
	case "status":
		gen := d.cacheGen()
		rep, err := d.gatherStatus()
		if err != nil {
			return nil, err
		}
		d.storeStatus(rep, gen)
		return rep, nil
	case "battery":
		gen := d.cacheGen()
		b, err := d.readBattery()
		if err != nil {
			return nil, err
		}
		d.storeBattery(b, gen)
		return b, nil
	case "capabilities":
		return handler.Capabilities(), nil
	case "set":
		action, _ := req.Args["action"].(string)
		params := decodeParams(req.Args["params"])
		expect := decodeParams(req.Args["expect"])
		dbg("SET action=%q params=%x expect=%x\n", action, params, expect)
		// Route through the model handler for validation/special handling
		got, err := handler.HandleSet(d, action, params, expect)
		var ind *indeterminateError
		if errors.As(err, &ind) {
			return map[string]interface{}{"indeterminate": ind.msg}, nil
		}
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
		//
		// The goroutine holds the connection for the blink: with the idle
		// window the link would otherwise drop before the stop is sent.
		d.beginRequest()
		go func() {
			defer d.endRequest()
			time.Sleep(findBlinkDuration)
			if err := d.ensureConnected(); err != nil {
				dbg("find auto-stop: %v\n", err)
				return
			}
			defer d.invalidateCache()
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
		//   gains      [10]int  band gains for positions 0-9, centi-dB (positions
		//                       10+ keep what the device reported)
		//   masterGain int      master gain, centi-dB (-1200..+1200)
		//   slot       int      target slot byte
		//   bands      [{index,freq,gain(dB),q,type}] build the payload from
		//                       these bands alone (exclusive with gains)
		//   allowProtected bool lift the protected-slot refusal (slots 6, 11)
		//   allowFull  bool     allow the unproven 143-byte write
		// Raw payload bytes are never accepted. Failures carry a code:
		// protected_slot, full_length_required, unconfirmed.
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
		fwd := map[string]interface{}{"masterGain": req.Args["masterGain"]}
		for _, k := range []string{"allowProtected", "allowFull"} {
			if v, ok := req.Args[k]; ok {
				fwd[k] = v
			}
		}
		op, err := eqOpFromArgs(fwd)
		if err != nil {
			return nil, err
		}
		return d.eqApply(op)
	case "hearingMode":
		// {on: bool}: FF 03 45 01 0x. Result: sent frame, ack, 0x22 readback.
		return d.hearingMode(req.Args)
	case "hearingWrite":
		// {ear, gains[8], confirm:true, otherGains?[8]}: writes the curve frame
		// of one ear (0x46 left, 0x47 right). Never without confirm:true.
		return d.hearingWrite(req.Args)
	case "fitTest":
		// Args: seconds (optional, 5..60, default 30). Holds the connection
		// for at most that long.
		secs := 30.0
		if v, ok := req.Args["seconds"].(float64); ok && v >= 5 && v <= 60 {
			secs = v
		}
		res, err := d.fitTest(time.Duration(secs * float64(time.Second)))
		if err != nil {
			return nil, err
		}
		return res, nil
	case "wear":
		// Read 0x2C, set byte 0, keep the remaining bytes, write, confirm.
		// state 1 = on, 2 = off. "Off" is written as 0x00 (what the vendor app
		// writes, verified on hardware 2026-10-13) on a verified model and as
		// 0x02 on any other (conservative). The diagnostic {literal:true}
		// writes exactly the byte given (0 or 2); 0 is refused on a model
		// that is not verified. Returns the confirmed payload.
		st, ok := req.Args["state"].(float64)
		literal, _ := req.Args["literal"].(bool)
		if !ok || (st != 1 && st != 2 && !(st == 0 && literal)) {
			return nil, errors.New("wear: state must be 1 (on) or 2 (off); 0 is only the diagnostic off byte (literal)")
		}
		verified := handler.Capabilities().Verified
		if st == 0 && !verified {
			return nil, errors.New("wear: off byte 0 is only written on a verified model")
		}
		if st == 2 && !literal && verified {
			st = 0
		}
		got, err := d.wearSet(byte(st))
		if err != nil {
			return nil, err
		}
		return bytesJSON(got), nil
	case "windOffByte":
		// Diagnostic: write the wind-detection "off" with an explicit byte
		// (0 as the vendor app, 2 as this tool) and report the raw readback.
		b, ok := req.Args["byte"].(float64)
		if !ok || (b != 0 && b != 2) {
			return nil, errors.New("windOffByte: byte must be 0 or 2")
		}
		got, err := d.windOff(byte(b))
		if err != nil {
			return nil, err
		}
		return bytesJSON(got), nil
	case "fnList":
		// Functions the model's panel declares that no specialised control
		// covers, with their widget type, options and state (fn.go).
		return d.fnList(), nil
	case "fnSet":
		// {cmdId, value | on | off, confirm?, confirmAgain?}: writes the bytes
		// the panel declares for that function, then reads it back.
		return d.fnSet(req.Args)
	case "noise":
		// Raw 0x17 payload [mode, scene, level].
		p, err := d.query(protocol.OpANC, 3*time.Second)
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
	default:
		return nil, fmt.Errorf("unknown command: %s", req.Cmd)
	}
}

func (d *daemon) gatherStatus() (map[string]interface{}, error) {
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}

	// The first status after a connect would otherwise race the capability
	// sweep and report every probed control as absent; wait for it (bounded).
	d.waitProbe(8 * time.Second)

	rep := map[string]interface{}{}
	if left, right, box, lc, rc, cc, raw, err := d.readBatteryRaw(); err == nil {
		rep["battery"] = batteryMap(left, right, box, lc, rc, cc, raw)
	}
	if fw := d.readVersionRaw(); len(fw) > 0 {
		rep["firmwareBytes"] = bytesJSON(fw)
	}
	if k, err := d.readKeys(); err == nil && len(k) > 0 {
		rep["keys"] = bytesJSON(k)
	}
	model := d.modelReport()
	if model != nil {
		rep["model"] = model
	}
	if snap := d.capabilitiesSnapshot(); len(snap) > 0 {
		caps := map[string]interface{}{}
		for k, v := range snap {
			caps[k] = v
		}
		if model != nil {
			// Not a feature: tells the panel the probe results above come
			// from a model this tool was never exercised against.
			caps["verified"] = model["verified"]
		}
		// Model-dependent, derived from the handler of this connection.
		caps["eq"] = d.handlerHasFeature(FeatureEQ)
		// ldac restarts the earbuds and is only writable on a verified
		// model, so the panel is told "available" only for those.
		h := d.knownHandler()
		caps["ldac"] = snap["ldac"] && h != nil && h.Capabilities().Verified
		rep["capabilities"] = caps
	}
	if p, err := d.query(protocol.OpSpatial, 800*time.Millisecond); err == nil && len(p) >= 1 {
		rep["spatial"] = bytesJSON(p)
	}
	// Wear-sensor sensitivity: only asked of a verified model that answered
	// the capability probe, so a silent unit does not slow every status.
	if snap := d.capabilitiesSnapshot(); snap["wearSensitivity"] {
		if h := d.knownHandler(); h != nil && h.Capabilities().Verified {
			if p, err := d.query(protocol.OpWearSensitivity, 800*time.Millisecond); err == nil && len(p) >= 1 && p[0] >= 1 && p[0] <= 3 {
				rep["wearSensitivity"] = int(p[0])
			}
		}
	}

	// ANC
	if p, err := d.query(protocol.OpANC, 1500*time.Millisecond); err == nil && len(p) >= 3 {
		rep["anc"] = map[string]interface{}{"modeRaw": int(p[0]), "sceneRaw": int(p[1]), "value": int(p[2])}
	}

	// Toggles
	toggles := map[string]byte{"game": protocol.OpGame, "sleep": protocol.OpSleep, "ldac": protocol.OpLDAC, "multipoint": protocol.OpMultipoint, "wind": protocol.OpWind}
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
		"promptVol": protocol.OpPromptVolume, "autoOff": protocol.OpAutoOff, "wear": protocol.OpWear, "balance": protocol.OpBalance, "eq": protocol.OpEQ,
	}
	for name, op := range other {
		if p, err := d.query(op, 1500*time.Millisecond); err == nil {
			rep[name] = bytesJSON(p)
		}
	}
	// Generic panel functions: nothing is asked when the model declares none.
	if fs := d.fnStatus(); len(fs) > 0 {
		rep["functions"] = fs
	}
	return rep, nil
}

func (d *daemon) readBatteryRaw() (left, right, box byte, lc, rc, cc bool, raw []byte, err error) {
	sess := d.curSession()
	if sess == nil {
		return 0, 0, 0, false, false, false, nil, errors.New("not connected")
	}
	// Battery notifies on 0008; we read the characteristic directly for a snapshot
	var buf [64]byte
	n, err := sess.Read(charBattery, buf[:])
	if err != nil {
		return 0, 0, 0, false, false, false, nil, err
	}
	if n < 3 {
		return 0, 0, 0, false, false, false, nil, errors.New("battery data too short")
	}
	left, right, box, lc, rc, cc = decodeBattery(buf[:n])
	return left, right, box, lc, rc, cc, append([]byte(nil), buf[:n]...), nil
}

// decodeBattery splits each of the three battery bytes (left, right, case,
// len >= 3) into its level (low 7 bits) and the charging flag (bit 7), as the
// vendor app does (b & 0x7F, b & 0x80).
func decodeBattery(data []byte) (left, right, box byte, lc, rc, cc bool) {
	return data[0] & 0x7f, data[1] & 0x7f, data[2] & 0x7f,
		data[0]&0x80 != 0, data[1]&0x80 != 0, data[2]&0x80 != 0
}

// batteryMap builds the wire object from decoded levels. 0x00 and 0x7f on the
// case level mean "no case reading" (sentinels, applied to the masked value).
func batteryMap(left, right, box byte, lc, rc, cc bool, raw []byte) map[string]interface{} {
	return map[string]interface{}{
		"left": left, "right": right, "case": box,
		"leftCharging": lc, "rightCharging": rc, "caseCharging": cc,
		"casePresent": box != 0x00 && box != 0x7f,
		// raw is the undecoded characteristic, as lowercase hex (diagnostics).
		"raw": hex.EncodeToString(raw),
	}
}

func (d *daemon) readBattery() (map[string]interface{}, error) {
	left, right, box, lc, rc, cc, raw, err := d.readBatteryRaw()
	if err != nil {
		return nil, err
	}
	return batteryMap(left, right, box, lc, rc, cc, raw), nil
}

func (d *daemon) setAction(action string, params, expect []byte) ([]byte, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	// Find my earbuds maps to the LED flash opcode here; qcyctl plays a local
	// tone instead (an earbud that is lost cannot be heard from the ear).
	// spatial and fit are wired so a model whose probe answers them can be
	// driven; on units that stay silent the capability is false and the UI
	// never offers the control.
	// ldac (0x23) restarts the earbuds: the vendor server declares the action
	// with "Turning on LDAC will restart the device" (docs/WEB_API.md), so the
	// restart is expected. It is writable on verified models only
	// (verifiedOnlyActions), once per request, and the restart that follows an
	// accepted write is not an error (see the ldac branch below). Multipoint
	// (0x24) restarts the earbuds too and is documented the same way.
	// Answering a query is not the same as honouring a write: readOnlyCmds
	// lists what must not be driven (promptVol only while promptVolWritable is
	// false).
	if reason, readOnly := readOnlyCmds[action]; readOnly {
		return nil, fmt.Errorf("%s is read-only on this device: %s", action, reason)
	}
	// 0x22 has exactly one write path, eqApply, which backs the slot up first.
	if action == "eq" {
		return nil, errors.New("eq cannot be written as a raw action: use eqApply, which reads, patches and backs the payload up first")
	}
	opcode, ok := protocol.ActionOpcode(action)
	if !ok {
		return nil, fmt.Errorf("unknown action: %s", action)
	}
	// Setters whose frame shape and value set come from the vendor app:
	// validate and normalise before anything is written.
	if verifiedOnlyActions[action] {
		if h := d.knownHandler(); h == nil || !h.Capabilities().Verified {
			return nil, fmt.Errorf("%s is only available on a verified model", action)
		}
	}
	params, expect, err := prepareSetter(action, params, expect)
	if err != nil {
		return nil, err
	}
	if action == "wind" && len(params) == 1 {
		// Wind detection "off": 0x00 as the vendor app writes it (verified on
		// hardware 2026-10-13) on a verified model, 0x02 on any other.
		verified := false
		if h := d.knownHandler(); h != nil {
			verified = h.Capabilities().Verified
		}
		switch {
		case params[0] == 0 && !verified:
			return nil, errors.New("wind: off byte 0 is only written on a verified model")
		case params[0] == 2 && verified:
			params = []byte{0}
		}
	}

	if !d.isConnected() {
		return nil, errors.New("not connected")
	}

	d.drain()
	if err := d.send(opcode, params); err != nil {
		return nil, err
	}

	if action == "ldac" {
		// The write was accepted; it is never repeated. The earbuds may
		// restart right after it, so a readback that fails because the link
		// dropped (or stayed silent) is "applied, the earbuds restarted", not
		// an error.
		got, last, ok := d.awaitMatchLast(opcode, func(p []byte) bool { return bytes.HasPrefix(p, expect) }, ldacConfirmTimeout)
		if ok {
			return got, nil
		}
		if len(last) > 0 && d.isConnected() {
			if len(last) >= 1 && len(expect) >= 1 && last[0] != expect[0] && (last[0] == 1 || last[0] == 2) {
				// The earbuds still echo the old value and the link is up:
				// the restart that applies the change may be pending. Not a
				// hard failure, and the write is never repeated.
				return nil, &indeterminateError{msg: fmt.Sprintf("indeterminado: aplicado possivelmente, reinício pendente (o aparelho ainda reporta %x) — releia o estado", last)}
			}
			return nil, fmt.Errorf("ldac: escrito mas o aparelho reportou %x (esperado %x); o aparelho pode ter aplicado, leia o estado de novo", last, expect)
		}
		return nil, nil
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
		if settle, ok := settleBeforeQuery[action]; ok {
			// The vendor app waits before it reads back; asking at once gets
			// the pre-change value.
			d.sleep(settle)
			if err := d.send(protocol.OpQuery, []byte{opcode}); err != nil {
				return nil, err
			}
			got, last, ok := d.awaitMatchLast(opcode, func(p []byte) bool { return bytes.HasPrefix(p, expect) }, 3*time.Second)
			if !ok {
				if len(last) == 0 {
					return nil, fmt.Errorf("%s: escrito mas o aparelho não respondeu à leitura de confirmação", action)
				}
				return nil, fmt.Errorf("%s: escrito mas o aparelho reportou %x (esperado %x)", action, last, expect)
			}
			return got, nil
		}
		got, err := d.awaitValue(opcode, expect, 3*time.Second)
		if err != nil {
			return nil, err
		}
		return got, nil
	}
	_, _ = d.wait(opcode, 250*time.Millisecond)
	return nil, nil
}

// promptVolWritable switches the 0x1D prompt-volume write on. The earlier
// "ignores every value" measurement used a one-byte frame; the app's frame
// (FF 04 1D 02 <v> 00, v in 4/6/8/10) is verified on the test device (N70, 2026-10-13).
// Set false to restore the read-only behaviour.
const promptVolWritable = true

// verifiedPromptVolumeSteps is the value list the vendor server gives the verified model
// (Quieter / Medium / Louder / Maximum). Per-model spec, constant for now.
var verifiedPromptVolumeSteps = []byte{4, 6, 8, 10}

// verifiedOnlyActions are setters whose frames were read from the app for the verified model
// and are refused on any model that is not verified.
var verifiedOnlyActions = map[string]bool{"promptVol": true, "wearSensitivity": true, "ldac": true}

// indeterminateError reports a write whose outcome cannot be known yet; the
// "set" reply carries it as {indeterminate: msg}, not as a failure.
type indeterminateError struct{ msg string }

func (e *indeterminateError) Error() string { return e.msg }

// ldacConfirmTimeout bounds the readback after an ldac write: the earbuds may
// restart instead of answering.
var ldacConfirmTimeout = 4 * time.Second

// settleBeforeQuery is how long a confirmed setter waits before reading back.
var settleBeforeQuery = map[string]time.Duration{
	"promptVol":       200 * time.Millisecond,
	"wearSensitivity": 200 * time.Millisecond,
}

// prepareSetter validates the value of the setters with a fixed value set and
// returns the exact frame parameters and the readback expectation. Other
// actions pass through unchanged.
func prepareSetter(action string, params, expect []byte) ([]byte, []byte, error) {
	switch action {
	case "promptVol":
		if len(params) < 1 || len(params) > 2 || (len(params) == 2 && params[1] != 0) {
			return nil, nil, errors.New("promptVol: expected one value (4, 6, 8 or 10)")
		}
		v := params[0]
		if !bytes.Contains(verifiedPromptVolumeSteps, []byte{v}) {
			return nil, nil, fmt.Errorf("promptVol: %d is not one of %v", v, verifiedPromptVolumeSteps)
		}
		// Two parameter bytes, the second 0, as the vendor app writes it.
		return []byte{v, 0}, []byte{v}, nil
	case "autoOff":
		// [lo, hi, 0, 0] minutes; ffff = never. 0 is "power off now" in the
		// vendor app, never a timer value: nothing may send it.
		if len(params) < 2 || (params[0] == 0 && params[1] == 0) {
			return nil, nil, errors.New("autoOff: 0 is refused (it powers the earbuds off now); use 65535 for never")
		}
		return params, expect, nil
	case "ldac":
		if len(params) != 1 || (params[0] != 1 && params[0] != 2) {
			return nil, nil, errors.New("ldac: expected 1 (on) or 2 (off)")
		}
		return params, []byte{params[0]}, nil
	case "wearSensitivity":
		if len(params) != 1 || params[0] < 1 || params[0] > 3 {
			return nil, nil, errors.New("wearSensitivity: expected 1 (medium), 2 (normal) or 3 (high)")
		}
		return params, []byte{params[0]}, nil
	}
	return params, expect, nil
}

// fitStart / fitStop are the frames the vendor app sends for the ear-tip fit
// test (FF 05 11 03 01 01 01 / FF 05 11 03 00 00 00), verified on the test device
// (N70) 2026-10-13.
var (
	fitStart = []byte{1, 1, 1}
	fitStop  = []byte{0, 0, 0}
)

// fitSeal is the per-ear result of the fit test.
type fitResult struct {
	Status int `json:"status"`
	Left   int `json:"left"`
	Right  int `json:"right"`
}

// fitTest starts the fit test and waits for the final notification
// (<0x11> <status 2> <left> <right>). The stop frame is always sent at the end,
// whatever the outcome. A concurrent fitStop request cancels the wait.
func (d *daemon) fitTest(timeout time.Duration) (fitResult, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	cancel := make(chan struct{})
	d.mu.Lock()
	d.fitCancel = cancel
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.fitCancel = nil
		d.mu.Unlock()
		_ = d.send(protocol.OpFit, fitStop)
	}()

	d.drain()
	if err := d.send(protocol.OpFit, fitStart); err != nil {
		return fitResult{}, err
	}
	deadline := time.After(timeout)
	for {
		select {
		case f := <-d.frames:
			if f.cmd == protocol.OpFit && len(f.params) >= 3 && f.params[0] == 2 {
				return fitResult{Status: 2, Left: int(f.params[1]), Right: int(f.params[2])}, nil
			}
		case <-cancel:
			return fitResult{}, errors.New("fit: cancelled")
		case <-deadline:
			return fitResult{}, fmt.Errorf("fit: no final result within %s", timeout)
		}
	}
}

// fitStopRequest cancels a running fit test, or just sends the stop frame.
func (d *daemon) fitStopRequest() error {
	d.mu.Lock()
	c := d.fitCancel
	d.fitCancel = nil
	d.mu.Unlock()
	if c != nil {
		close(c)
		return nil
	}
	if !d.isConnected() {
		return nil
	}
	d.txMu.Lock()
	defer d.txMu.Unlock()
	return d.send(protocol.OpFit, fitStop)
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
	got, last, ok := d.awaitMatchLast(opcode, match, timeout)
	if !ok {
		return nil, fmt.Errorf("readback mismatch for 0x%02x: want %s, device reported %x (the device may nevertheless have applied the value; re-read to check)", opcode, wantDesc, last)
	}
	return got, nil
}

// awaitMatchLast waits for a frame of opcode that match accepts. On timeout it
// returns ok=false and the last frame of that opcode seen (nil if none), which
// callers classify (see qcycore.EqWriteFailure).
func (d *daemon) awaitMatchLast(opcode byte, match func([]byte) bool, timeout time.Duration) (got, last []byte, ok bool) {
	deadline := time.After(timeout)
	requery := time.After(400 * time.Millisecond)
	queries := 0
	for {
		select {
		case f := <-d.frames:
			if f.cmd == opcode && match(f.params) {
				return f.params, f.params, true
			} else if f.cmd == opcode {
				last = f.params
			}
		case <-requery:
			if queries < 5 {
				_ = d.send(protocol.OpQuery, []byte{opcode})
				queries++
				requery = time.After(400 * time.Millisecond)
			}
		case <-deadline:
			return nil, last, false
		}
	}
}

// eqArgKeys are the only arguments eqApply accepts. A raw payload (hex or
// bytes) has no key here on purpose: 0x22 is only ever written from structured
// values that qcycore validates, never from bytes a client built.
var eqArgKeys = map[string]bool{
	"gains": true, "masterGain": true, "slot": true, "bands": true,
	"allowProtected": true, "allowFull": true,
}

var eqBandKeys = map[string]bool{"index": true, "freq": true, "gain": true, "q": true, "type": true}

// eqOpFromArgs decodes the eqApply arguments. Values are validated again by
// qcycore.ApplyEq; this only rejects shapes JSON cannot express safely.
func eqOpFromArgs(args map[string]interface{}) (qcycore.EqOp, error) {
	var op qcycore.EqOp
	for k := range args {
		if !eqArgKeys[k] {
			switch k {
			case "payload", "raw", "hex", "params", "expect", "action":
				return op, fmt.Errorf("eq: %q is not accepted: raw 0x22 payloads cannot be written over the socket; send structured gains/bands", k)
			}
			return op, fmt.Errorf("eq: unknown argument %q", k)
		}
	}
	whole := func(v interface{}, what string) (int, error) {
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 1e6 {
			return 0, fmt.Errorf("eq: %s must be a whole number", what)
		}
		return int(f), nil
	}
	flag := func(key string) (bool, error) {
		v, ok := args[key]
		if !ok {
			return false, nil
		}
		b, isBool := v.(bool)
		if !isBool {
			return false, fmt.Errorf("eq: %s must be true or false", key)
		}
		return b, nil
	}
	var err error
	if op.AllowProtected, err = flag("allowProtected"); err != nil {
		return op, err
	}
	if op.AllowFullLength, err = flag("allowFull"); err != nil {
		return op, err
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
	if v, ok := args["bands"]; ok {
		arr, ok := v.([]interface{})
		if !ok || len(arr) == 0 || len(arr) > qcycore.EqMaxBands {
			return op, fmt.Errorf("eq: bands must be an array of 1-%d objects", qcycore.EqMaxBands)
		}
		bands := make([]qcycore.BandSpec, 0, len(arr))
		for i, e := range arr {
			m, ok := e.(map[string]interface{})
			if !ok {
				return op, fmt.Errorf("eq: bands[%d] must be an object", i)
			}
			for k := range m {
				if !eqBandKeys[k] {
					return op, fmt.Errorf("eq: bands[%d]: unknown field %q", i, k)
				}
			}
			var b qcycore.BandSpec
			fields := []struct {
				key string
				dst *int
			}{{"index", &b.Index}, {"freq", &b.Freq}, {"q", &b.Q}, {"type", &b.Type}}
			for _, f := range fields {
				v, present := m[f.key]
				if !present {
					return op, fmt.Errorf("eq: bands[%d]: %s is required", i, f.key)
				}
				n, err := whole(v, fmt.Sprintf("bands[%d].%s", i, f.key))
				if err != nil {
					return op, err
				}
				*f.dst = n
			}
			g, ok := m["gain"].(float64)
			if !ok || math.IsNaN(g) || math.IsInf(g, 0) {
				return op, fmt.Errorf("eq: bands[%d].gain must be a finite number (dB)", i)
			}
			b.Gain = g
			bands = append(bands, b)
		}
		op.Bands = bands
	}
	return op, nil
}

// responseCode maps an error to the machine-readable code of the response:
// the lifecycle classes (busy, not_found) and the EQ write classes
// (protected_slot, full_length_required, unconfirmed).
func responseCode(err error) string {
	if c := errorCode(err); c != "" {
		return c
	}
	var ee *qcycore.EqError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ""
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

// eqConfirmTimeout is how long eqApply waits for the device to report the
// payload it was sent. A variable only so tests need not wait it out.
var eqConfirmTimeout = 4 * time.Second

// eqApply is the only path that writes 0x22. Under ONE txMu it reads the live
// payload, builds the payload from op (qcycore.ApplyEq: patched over what the
// device reported, or constructed from op.Bands; it also refuses protected
// slots and the unproven 143-byte format unless op allows them; an explicit
// slot 255 implies the 143-byte format), saves the
// pre-write payload as a backup (the write is aborted if that fails), writes,
// and confirms by readback. A device that keeps reporting another slot after
// the write yields a qcycore.EqError with CodeUnconfirmed.
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
	got, last, ok := d.awaitMatchLast(qcycore.EqOpcode, qcycore.EqMatch(next, op), eqConfirmTimeout)
	if !ok {
		return nil, qcycore.EqWriteFailure(next, last, bk.History)
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

// windOff writes 0x2A with off as its only parameter and confirms by reading
// it back, accepting 0x00 or 0x02 as "off". Returns the raw payload read.
func (d *daemon) windOff(off byte) ([]byte, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}
	d.drain()
	if err := d.send(protocol.OpWind, []byte{off}); err != nil {
		return nil, err
	}
	d.sleep(200 * time.Millisecond)
	if err := d.send(protocol.OpQuery, []byte{protocol.OpWind}); err != nil {
		return nil, err
	}
	return d.awaitMatch(protocol.OpWind, func(p []byte) bool {
		return len(p) >= 1 && (p[0] == 0 || p[0] == 2)
	}, "00 or 02 (off)", 3*time.Second)
}

// wearByteMatches reports whether a read-back byte 0 of 0x2C confirms a write
// of state. 0x00 and 0x02 both mean "off": the vendor app writes 0x00, this
// tool writes 0x02, and which one the firmware keeps is what `wear off
// --off-byte` is meant to find out.
func wearByteMatches(state, got byte) bool {
	if state == 0 || state == 2 {
		return got == 0 || got == 2
	}
	return got == state
}

// wearSet reads 0x2C, replaces byte 0 with state and writes the rest back
// unchanged, under one txMu.
func (d *daemon) wearSet(state byte) ([]byte, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}
	cur, err := d.queryLocked(protocol.OpWear, 2*time.Second)
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
	if err := d.send(protocol.OpWear, set); err != nil {
		return nil, err
	}
	return d.awaitMatch(protocol.OpWear, func(p []byte) bool {
		return len(p) >= len(set) && wearByteMatches(state, p[0]) && bytes.Equal(p[1:len(set)], set[1:])
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
	{"spatial", protocol.OpSpatial, nil},
	{"wear", protocol.OpWear, nil},
	{"fit", protocol.OpFit, nil},
	{"ancLevel", protocol.OpANCStrength, nil},
	{"promptVol", protocol.OpPromptVolume, sanePromptVolume},
	{"wearSensitivity", protocol.OpWearSensitivity, func(p []byte) bool { return len(p) >= 1 && p[0] >= 1 && p[0] <= 3 }},
	// FE 01 23 -> [1 on | 2 off]. The capability is further gated on a
	// verified model in gatherStatus.
	{"ldac", protocol.OpLDAC, func(p []byte) bool { return len(p) >= 1 && (p[0] == 1 || p[0] == 2) }},
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
var readOnlyCmds = buildReadOnlyCmds()

func buildReadOnlyCmds() map[string]string {
	// ldac (0x23) is no longer here: it is a verified-only setter whose
	// restart the vendor server declares (verifiedOnlyActions).
	m := map[string]string{}
	if !promptVolWritable {
		// One-byte writes to 0x1D were ignored (read back 06 0f). The
		// two-byte frame FF 04 1D 02 v 00 is verified on the test device (N70, 2026-10-13).
		m["promptVol"] = "one-byte writes were ignored; the two-byte frame is not in use"
	}
	return m
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

// handlerHasFeature reports whether the current handler declares the feature.
func (d *daemon) handlerHasFeature(f FeatureFlag) bool {
	h := d.knownHandler()
	if h == nil {
		return false
	}
	for _, x := range h.Capabilities().Features {
		if x == f {
			return true
		}
	}
	return false
}

// verifiedOnlyCmds are the daemon commands whose wire behaviour was only ever
// established on the verified model: they are refused on any model that is not verified.
var verifiedOnlyCmds = map[string]bool{"eqApply": true, "eqMasterGain": true, "keysSet": true, "fitTest": true,
	"hearingMode": true, "hearingWrite": true, "windOffByte": true}

// gateCommand is the single gate between a request and the model-specific
// commands. A model that is not Verified gets none of the verified-only commands,
// and "wear" only when its profile declares the wearDetect action.
func gateCommand(h ModelHandler, cmd string) error {
	c := h.Capabilities()
	if c.Verified {
		return nil
	}
	if verifiedOnlyCmds[cmd] {
		return fmt.Errorf("%s is disabled: %s is not a verified model", cmd, c.DisplayName)
	}
	if cmd == "wear" && !declaresAction(c, "wearDetect") {
		return fmt.Errorf("wear is disabled: %s does not declare it", c.DisplayName)
	}
	return nil
}

func declaresAction(c Capabilities, id string) bool {
	for _, a := range c.Actions {
		if daemonActionID(a.ID) == daemonActionID(id) {
			return true
		}
	}
	return false
}

// modelReport describes the detected model for the status report.
func (d *daemon) modelReport() map[string]interface{} {
	d.mu.Lock()
	h, id, vendor, have := d.handler, d.modelID, d.vendorID, d.haveVendor
	d.mu.Unlock()
	if h == nil {
		return nil
	}
	c := h.Capabilities()
	actions := make([]string, 0, len(c.Actions))
	for _, a := range c.Actions {
		actions = append(actions, a.ID)
	}
	features := make([]string, 0, len(c.Features))
	for _, f := range c.Features {
		features = append(features, string(f))
	}
	m := map[string]interface{}{
		"modelId": id, "displayName": c.DisplayName, "verified": c.Verified,
		// What the connected model declares; the panel crosses its probe
		// results with these so an undeclared control is never offered.
		"actions": actions, "features": features,
	}
	if have {
		m["vendorId"] = int(vendor)
	}
	if c.ProductTitle != "" {
		m["productTitle"] = c.ProductTitle
	}
	if have {
		if ka := keyActionsFor(vendor); len(ka) > 0 {
			m["keyActions"] = ka
		}
	}
	return m
}

// keyActionsFor lists, per music-mode key id (two hex digits, as in the
// `keys` pairs), the actions the model's panel offers for it. Call-mode and
// other groups are left out on purpose: only the music keys are editable.
func keyActionsFor(vendorID uint16) map[string][]map[string]interface{} {
	out := map[string][]map[string]interface{}{}
	for _, f := range fnFunctionsFor(vendorID) {
		if f.Keys == nil {
			continue
		}
		for _, e := range f.Keys.Events {
			if e.Group != "music" || e.EventID == nil || *e.EventID < 1 || *e.EventID > 0x0a {
				continue
			}
			var acts []map[string]interface{}
			for _, a := range e.Actions {
				if a.Cmd == nil {
					continue
				}
				acts = append(acts, map[string]interface{}{"id": *a.Cmd, "name": a.Name})
			}
			if len(acts) > 0 {
				out[fmt.Sprintf("%02x", *e.EventID)] = acts
			}
		}
	}
	// The quadruple-tap keys (0x07 left, 0x08 right) are not listed by the
	// panel but the firmware accepts them (a write of 0x07 was applied and
	// reverted on hardware); they take the list of the group's other keys.
	if tmpl, ok := out["01"]; ok {
		for _, k := range []string{"07", "08"} {
			if _, have := out[k]; !have {
				out[k] = tmpl
			}
		}
	}
	return out
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
	// "eq" is not a probe result: gatherStatus derives it from the current
	// handler, so it cannot go stale across a change of earbuds.
	d.caps["wind"] = true
	d.caps["keys"] = d.sess != nil && d.sess.Has(charKeys)

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
	// Only a sweep that ran against a live link counts; one cut short by a
	// drop is retried on the next connection.
	d.mu.Lock()
	d.probed = d.connected
	d.mu.Unlock()
}

func (d *daemon) readKeys() ([]byte, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	return d.readKeysLocked()
}

func (d *daemon) readKeysLocked() ([]byte, error) {
	sess := d.curSession()
	if sess == nil || !sess.Has(charKeys) {
		return nil, errors.New("keys characteristic 000D not available on this device")
	}
	var buf [64]byte
	n, err := sess.Read(charKeys, buf[:])
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
	sess := d.curSession()
	if sess == nil || !sess.Has(charKeys) {
		return errors.New("keys characteristic 000D not available on this device")
	}
	if len(raw) == 0 || len(raw)%2 != 0 {
		return fmt.Errorf("keys set: payload must be non-empty keyId:funId pairs, got %d bytes", len(raw))
	}
	if err := sess.WriteKeys(raw); err != nil {
		return fmt.Errorf("keys set write 000D: %w", err)
	}
	time.Sleep(150 * time.Millisecond)
	got, err := d.readKeysLocked()
	if err != nil {
		return err
	}
	if !protocol.KeysConfirmed(raw, got) {
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
			_ = enc.Encode(errorResponse(err))
		} else {
			_ = enc.Encode(response{OK: true, Data: data})
		}
	}
}

func main() {
	idleFlag := flag.Float64("idle", -1, "seconds the BLE link is kept after the last request (0 = never drop it); default $QCYCTL_IDLE_SECONDS or 6")
	flag.Parse()

	scanSeconds := 15
	if args := flag.Args(); len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil {
			scanSeconds = v
		}
	}
	idle, err := resolveIdle(*idleFlag, os.Getenv("QCYCTL_IDLE_SECONDS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "idle: %v\n", err)
		os.Exit(2)
	}

	l, sock, err := listenSocket()
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(sock)
	defer l.Close()

	cfg := defaultLifecycle()
	cfg.idle = idle
	d := newDaemon(&bleDialer{scanSeconds: scanSeconds}, cfg)

	// No connection at startup and no background reconnect or probe: the link
	// is opened by the first request and dropped after the idle window, so the
	// earbuds stay free for the phone app in between.

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			d.shutdown()
			return
		}
		go handleConn(conn, d)
	}
}
