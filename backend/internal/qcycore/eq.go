// Package qcycore holds the pure, hardware-free parts shared by qcyctl and
// qcyctld: the 0x22 (EQ) payload layout and patching, the EQ backup store and
// the daemon socket location. Nothing here touches Bluetooth.
package qcycore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// EQ payload (opcode 0x22), as observed on the MeloBuds N70:
//
//	[slot:1][masterGain:int16 LE, centi-dB][20 x (freq:u16 LE, gain:int16 LE, q:u16 LE, type:u8)]
//
// 3 + 20*7 = 143 bytes (EqReadLen), what the device reports. A band position
// whose freq is 0 is empty. Factory presets use a few positions, the user's
// custom curve may use up to 20 (positions 10-19 form the "tail" starting at
// byte 73). Frequencies, Q and type are NOT fixed, so a writer must preserve
// them.
//
// Write format: 73 bytes (EqLen, positions 0-9 only) is PROVEN on the device
// and is used whenever bytes 73.. carry no band (all zero, or only the 0xFF
// "empty" type marker; see TailBlank). When the tail holds a band
// (any other non-zero byte) the full read length would have to be sent (format
// NOT proven on the device for the factory slots): ApplyEq refuses that unless
// EqOp.AllowFullLength is set, and results then carry fullLength=true. The one
// exception is an explicit write to slot SlotCustomApp (255): the phone app
// itself writes its "Personalizado" curves there with up to 143 bytes, so
// ApplyEq allows the 143-byte format for that target without the flag.
const (
	EqOpcode = 0x22
	// EqLen is the proven write length; EqReadLen the length the device reports.
	EqLen      = 73
	EqReadLen  = 143
	EqBands    = 10 // positions addressable by `eq set` / EqOp.Gains
	EqMaxBands = 20 // positions in the full payload
	eqBandSize = 7
	eqHeader   = 3

	// MaxCenti is the accepted range for band gains, in centi-dB (+/-12 dB: factory curves
	// exceed 8 dB, down to -12).
	MaxCenti = 1200
	MaxDB    = 12.0
	// MasterMaxCenti / MasterMaxDB: the master gain range (+/-12 dB).
	MasterMaxCenti = 1200
	MasterMaxDB    = 12.0

	// Limits for a caller-supplied BandSpec (see EqOp.Bands): the audible range
	// and the u16 the payload stores for Q.
	MinBandFreq = 20
	MaxBandFreq = 20000
	MinBandQ    = 1
	MaxBandQ    = 65535
)

// BandTypes are the band type bytes accepted in a write. 0 and 2 are the
// regular band types; 3 was read back from factory presets 1, 10 and 14; 255
// (0xFF) is the type the phone app gives to the user's own bars in slot 255.
// Anything else has never been observed and is refused (1, 7, ...).
var BandTypes = []int{0, 2, 3, 255}

// BandTypeSupported reports whether t is in BandTypes.
func BandTypeSupported(t int) bool {
	for _, v := range BandTypes {
		if v == t {
			return true
		}
	}
	return false
}

// emptyTypeMarker is the type byte the device reports in an empty position.
const emptyTypeMarker = 0xff

// ProtectedSlots are the slots ApplyEq refuses to write into, or to switch away
// from, unless EqOp.AllowProtected is set: 6 is the factory "Classico" curve
// and 11 the manufacturer's hearing-test (custom) profile. An inactive slot
// cannot be read back, so whatever a write replaces there is gone.
var ProtectedSlots = []int{6, 11}

// IsProtectedSlot reports whether slot is in ProtectedSlots.
func IsProtectedSlot(slot int) bool {
	for _, p := range ProtectedSlots {
		if p == slot {
			return true
		}
	}
	return false
}

// Machine-readable classes of EqError, also the daemon's response codes.
const (
	CodeProtectedSlot      = "protected_slot"
	CodeFullLengthRequired = "full_length_required"
	CodeUnconfirmed        = "unconfirmed"
)

// EqError is a refusal or a failed confirmation with a stable Code. Match a
// class with errors.Is(err, ErrProtectedSlot) (and the other Err* sentinels) or
// read the fields through errors.As.
type EqError struct {
	Code   string
	Msg    string
	Sent   []byte // unconfirmed: the payload that was written
	Got    []byte // unconfirmed: the last payload the device reported
	Backup string // unconfirmed: where the pre-write payload was saved
}

func (e *EqError) Error() string { return e.Msg }

// Is makes the Err* sentinels match every EqError of the same Code.
func (e *EqError) Is(target error) bool {
	t, ok := target.(*EqError)
	return ok && t.Msg == "" && t.Code == e.Code
}

// Sentinels for errors.Is.
var (
	ErrProtectedSlot      = &EqError{Code: CodeProtectedSlot}
	ErrFullLengthRequired = &EqError{Code: CodeFullLengthRequired}
	ErrUnconfirmed        = &EqError{Code: CodeUnconfirmed}
)

// EqBand is one parametric band as stored in the payload.
type EqBand struct {
	Index int     `json:"index"` // position in the payload (0-19)
	Freq  int     `json:"freq"`
	Gain  float64 `json:"gain"` // dB
	Q     int     `json:"q"`
	Type  int     `json:"type"`
}

// EqSnapshot is a decoded 0x22 payload. MasterGain and band gains are in dB.
type EqSnapshot struct {
	Slot       int      `json:"slot"`
	Effect     string   `json:"effect,omitempty"` // EffectName(Slot), omitted when the slot has no measured name
	MasterGain float64  `json:"masterGain"`
	Bands      []EqBand `json:"bands"`
	Payload    string   `json:"payload"` // raw payload, lowercase hex
}

// SlotCustomApp is the slot the phone app writes any curve of its own to; the
// name the user gave it is kept only on the phone.
const SlotCustomApp = 255

// effectNames is the slot -> effect map measured on a MeloBuds N70 by reading
// 0x22. Slots 7, 8, 9, 12 and 13 have not been observed. Slot 0 is the old
// test curve of this tool and has no effect name.
var effectNames = map[int]string{
	1:   "Predefinido",
	2:   "Popular",
	3:   "Bass pesados",
	4:   "Rock",
	5:   "Suave",
	6:   "Clássico",
	10:  "Som espacial", // payload identical to slot 1
	11:  "Experiência auditiva",
	14:  "Agudos suaves",
	255: "Personalizado",
}

// EffectName returns the factory/app effect name measured for slot, or false
// when the slot is unknown or was not observed.
func EffectName(slot int) (string, bool) {
	n, ok := effectNames[slot]
	return n, ok
}

// EqResult is what a write returns: the state read back after the write plus
// where the pre-write payload was saved.
type EqResult struct {
	EqSnapshot
	Previous      string `json:"previous"`             // payload before the write, hex
	FullLength    bool   `json:"fullLength,omitempty"` // true: the full-length (unproven) write format was used
	BackupFirst   string `json:"backupFirst"`          // immutable first-seen backup of that slot
	BackupHistory string `json:"backupHistory"`        // append-only history entry for this write
}

// BandSpec is one caller-supplied band for EqOp.Bands. Gain is in dB (+/-12).
// A position left out of EqOp.Bands is written empty (freq 0).
type BandSpec struct {
	Index int     `json:"index"` // position in the payload (0-19)
	Freq  int     `json:"freq"`  // Hz, MinBandFreq..MaxBandFreq
	Gain  float64 `json:"gain"`  // dB
	Q     int     `json:"q"`     // raw payload units, MinBandQ..MaxBandQ
	Type  int     `json:"type"`  // one of BandTypes: 0, 2, 3 or 255 (the values seen on the device)
}

// EqOp describes a write.
//
// Patch mode (Bands nil): nil fields mean "keep what the device has"; Gains and
// MasterCenti are patched onto the payload the device reported.
//
// Build mode (Bands set): the payload is CONSTRUCTED from Bands and never
// copied from what the device reported, so nothing of the active slot's curve
// can reach another slot; the device's payload is only the proof of life and
// the backup. MasterCenti nil keeps the device's master gain when the target is
// the active slot, and is an error for another slot (the reported master gain
// belongs to the active slot). Gains must be nil.
type EqOp struct {
	Gains       *[EqBands]int // centi-dB per band (patch mode)
	MasterCenti *int          // centi-dB
	Slot        *int          // target slot byte (0-255); nil = the active slot
	Bands       []BandSpec    // build mode; see above

	// AllowProtected lets the write touch a slot in ProtectedSlots (see
	// ApplyEq for exactly what is refused without it).
	AllowProtected bool
	// AllowFullLength lets the write use the 143-byte format, which is NOT
	// proven on the device. Without it ApplyEq refuses any payload longer than
	// EqLen, except when Slot is explicitly SlotCustomApp (255): the phone app
	// writes up to 143 bytes there, so that target implies the flag. The
	// implicit allowance looks at Slot only, never at the active slot.
	AllowFullLength bool
}

// ValidateEq checks a payload read from the device is usable for a
// read-modify-write: at least EqLen bytes (EqReadLen expected) and not the
// all-0xFF "no data" filler. A non-zero tail is allowed; ApplyEq then writes
// the full length. Reporting uses the more tolerant DecodeEq.
func ValidateEq(p []byte) error {
	if len(p) < EqLen {
		return fmt.Errorf("eq: unexpected payload length %d (want at least %d, device reports %d)", len(p), EqLen, EqReadLen)
	}
	if IsAllFF(p) {
		return fmt.Errorf("eq: device returned an all-0xFF payload (no real EQ data); refusing to back up or write over it")
	}
	return nil
}

// TailBlank reports whether bytes EqLen.. of p hold no band: every byte is zero,
// except that a band's type byte may be 0xFF, which is how the device marks an
// empty position (factory presets report freq 0, gain 0, q 0, type 0xFF in
// every unused position, including positions 10-19). Such a tail carries no
// information, so the proven 73-byte write format applies.
func TailBlank(p []byte) bool {
	for i := EqLen; i < len(p); i++ {
		if p[i] == 0 {
			continue
		}
		if p[i] == 0xff && (i-eqHeader)%eqBandSize == eqBandSize-1 {
			continue
		}
		return false
	}
	return true
}

// FullLength reports whether the payload to send uses the unproven full-length
// format (longer than EqLen).
func FullLength(sent []byte) bool { return len(sent) > EqLen }

// IsAllFF reports whether b is non-empty and every byte is 0xFF, the device's
// "no data yet" filler. A read-modify-write must never treat it as live state.
func IsAllFF(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c != 0xff {
			return false
		}
	}
	return true
}

// DecodeEq decodes as much of a payload as it has. It needs the 3-byte header;
// bands beyond what is present are omitted.
func DecodeEq(p []byte) (EqSnapshot, error) {
	if len(p) < eqHeader {
		return EqSnapshot{}, fmt.Errorf("eq: too short (%d bytes)", len(p))
	}
	s := EqSnapshot{
		Slot:       int(p[0]),
		MasterGain: float64(masterCenti(p)) / 100,
		Payload:    hex.EncodeToString(p),
		Bands:      []EqBand{},
	}
	if name, ok := EffectName(s.Slot); ok {
		s.Effect = name
	}
	n := (len(p) - eqHeader) / eqBandSize
	if n > EqMaxBands {
		n = EqMaxBands
	}
	for i := 0; i < n; i++ {
		b := eqHeader + i*eqBandSize
		freq := le16(p, b)
		if freq == 0 {
			continue
		}
		s.Bands = append(s.Bands, EqBand{
			Index: i,
			Freq:  int(freq),
			Gain:  float64(int16(le16(p, b+2))) / 100,
			Q:     int(le16(p, b+4)),
			Type:  int(p[b+6]),
		})
	}
	return s, nil
}

func le16(p []byte, off int) uint16 { return uint16(p[off]) | uint16(p[off+1])<<8 }

func masterCenti(p []byte) int { return int(int16(le16(p, 1))) }

// CentiFromDB converts a band gain in dB to centi-dB, rejecting NaN, Inf and
// anything outside +/-12 dB BEFORE any integer conversion (a float outside int
// range converts to an arbitrary value in Go).
func CentiFromDB(db float64) (int, error) {
	return centiInRange(db, MaxDB)
}

// MasterCentiFromDB is CentiFromDB for the master gain (+/-12 dB).
func MasterCentiFromDB(db float64) (int, error) {
	return centiInRange(db, MasterMaxDB)
}

func centiInRange(db, max float64) (int, error) {
	if math.IsNaN(db) || math.IsInf(db, 0) {
		return 0, fmt.Errorf("gain %v is not a finite number", db)
	}
	if db < -max || db > max {
		return 0, fmt.Errorf("gain %.2f dB is outside the supported range of -%g..+%g dB", db, max, max)
	}
	return int(math.Round(db * 100)), nil
}

// ApplyEq returns the payload to write and enforces the write-safety rules, so
// every caller (CLI direct, daemon socket, preset library) gets them. cur, the
// payload the device reports for the ACTIVE slot, is the proof of life and the
// base of a patch; with op.Bands it is not copied at all (see EqOp).
//
// Refusals, each lifted by its own flag:
//
//   - without AllowProtected: an explicit op.Slot in ProtectedSlots (even the
//     active one: omit Slot to adjust the active slot); a Slot other than the
//     active one while the active slot is protected (a cross-slot write, which
//     also switches the device away from it); op.Bands on a protected active
//     slot (it replaces the whole curve). A master or band-gain patch of the
//     active protected slot (Slot nil) stays allowed.
//   - without AllowFullLength: any payload longer than EqLen, unless op.Slot is
//     explicitly SlotCustomApp (255; see EqOp.AllowFullLength). A patch gets that
//     length only when bytes EqLen.. of cur hold a band (not TailBlank); Bands
//     get it only when a position is >= EqBands.
//
// Patch mode otherwise carries everything the op does not name over byte for
// byte. Its length is EqLen (the proven format) when bytes EqLen.. of cur are
// blank (TailBlank), otherwise the full length read.
func ApplyEq(cur []byte, op EqOp) ([]byte, error) {
	if err := ValidateEq(cur); err != nil {
		return nil, err
	}
	active := int(cur[0])
	target := active
	if op.Slot != nil {
		if *op.Slot < 0 || *op.Slot > 255 {
			return nil, fmt.Errorf("eq: slot must be 0-255, got %d", *op.Slot)
		}
		target = *op.Slot
	}
	if op.Bands != nil && op.Gains != nil {
		return nil, errors.New("eq: gains and bands are mutually exclusive")
	}
	if err := checkProtected(active, target, op); err != nil {
		return nil, err
	}
	var out []byte
	var err error
	if op.Bands != nil {
		out, err = buildEq(cur, target, op)
	} else {
		out, err = patchEq(cur, op)
	}
	if err != nil {
		return nil, err
	}
	implicitFull := op.Slot != nil && *op.Slot == SlotCustomApp
	if len(out) > EqLen && !op.AllowFullLength && !implicitFull {
		why := "bands at positions 10-19"
		if op.Bands == nil {
			why = "the payload holds a band beyond position 9, bytes 73.. are not blank"
		}
		return nil, &EqError{Code: CodeFullLengthRequired, Msg: fmt.Sprintf(
			"eq: writing slot %d needs the %d-byte format (%s); writing %d bytes has not been proven on the hardware; use --full-length (AllowFullLength) to allow it",
			target, len(out), why, len(out))}
	}
	return out, nil
}

func checkProtected(active, target int, op EqOp) error {
	if op.AllowProtected {
		return nil
	}
	refuse := func(slot int, why string) error {
		return &EqError{Code: CodeProtectedSlot, Msg: fmt.Sprintf(
			"eq: slot %d is protected (%s); refusing the write; use --allow-protected (AllowProtected) to override", slot, why)}
	}
	// Slot 255 is the phone app's "Personalizado" buffer: measured on the
	// hardware, writing it with 6 or 11 active leaves 6 and 11 byte-identical,
	// and the app itself does this. It is the one safe target (the active
	// slot is still backed up by the caller before the write).
	if op.Slot != nil && *op.Slot == SlotCustomApp {
		return nil
	}
	switch {
	case op.Slot != nil && IsProtectedSlot(*op.Slot):
		return refuse(*op.Slot, "factory or hearing-test curve whose current content cannot be read back once replaced; omit the slot to adjust the active one")
	case IsProtectedSlot(active) && target != active:
		return refuse(active, fmt.Sprintf("it is the active slot and writing slot %d would switch the device away from it", target))
	case IsProtectedSlot(active) && op.Bands != nil:
		return refuse(active, "a full band list would replace its whole curve")
	}
	return nil
}

// patchEq patches op.Gains / op.MasterCenti / op.Slot onto a copy of cur.
func patchEq(cur []byte, op EqOp) ([]byte, error) {
	n := len(cur)
	if TailBlank(cur) {
		n = EqLen
	}
	out := append([]byte(nil), cur[:n]...)
	if op.Slot != nil {
		out[0] = byte(*op.Slot)
	}
	if op.MasterCenti != nil {
		c := *op.MasterCenti
		if c < -MasterMaxCenti || c > MasterMaxCenti {
			return nil, fmt.Errorf("eq: master gain %d centi-dB outside +/-%d", c, MasterMaxCenti)
		}
		out[1], out[2] = byte(int16(c)), byte(int16(c)>>8)
	}
	if op.Gains != nil {
		for i, c := range op.Gains {
			if c < -MaxCenti || c > MaxCenti {
				return nil, fmt.Errorf("eq: band %d gain %d centi-dB outside +/-%d", i+1, c, MaxCenti)
			}
			off := eqHeader + i*eqBandSize + 2
			out[off], out[off+1] = byte(int16(c)), byte(int16(c)>>8)
		}
	}
	return out, nil
}

// buildEq constructs the payload for op.Bands from nothing: zeroed, then the
// target slot, the master gain and each band at its position. Positions 0-9
// only give EqLen bytes with a zero tail; any position >= EqBands gives
// EqReadLen. cur is read for the active slot's master gain only.
//
// A 143-byte payload for slot SlotCustomApp carries the format the device
// reports for the app's own writes: every position that holds no band is all
// zero except its type byte, which is 0xFF (the empty marker). That keeps the
// payload sent identical to what the device echoes, so EqMatch compares like
// with like. Other targets keep the all-zero tail.
func buildEq(cur []byte, target int, op EqOp) ([]byte, error) {
	if len(op.Bands) == 0 {
		return nil, errors.New("eq: bands is empty; a write must name at least one band")
	}
	if len(op.Bands) > EqMaxBands {
		return nil, fmt.Errorf("eq: %d bands given, at most %d positions exist", len(op.Bands), EqMaxBands)
	}
	n := EqLen
	var seen [EqMaxBands]bool
	for _, b := range op.Bands {
		switch {
		case b.Index < 0 || b.Index >= EqMaxBands:
			return nil, fmt.Errorf("eq: band index %d outside 0-%d", b.Index, EqMaxBands-1)
		case seen[b.Index]:
			return nil, fmt.Errorf("eq: band index %d given twice", b.Index)
		case b.Freq < MinBandFreq || b.Freq > MaxBandFreq:
			return nil, fmt.Errorf("eq: band %d: freq %d Hz outside %d-%d", b.Index, b.Freq, MinBandFreq, MaxBandFreq)
		case b.Q < MinBandQ || b.Q > MaxBandQ:
			return nil, fmt.Errorf("eq: band %d: q %d outside %d-%d", b.Index, b.Q, MinBandQ, MaxBandQ)
		case !BandTypeSupported(b.Type):
			return nil, fmt.Errorf("eq: band %d: type %d not allowed (the device has shown 0, 2, 3 and 255)", b.Index, b.Type)
		}
		seen[b.Index] = true
		if b.Index >= EqBands {
			n = EqReadLen
		}
	}
	var master int
	switch {
	case op.MasterCenti != nil:
		master = *op.MasterCenti
	case target == int(cur[0]):
		master = masterCenti(cur)
	default:
		return nil, fmt.Errorf("eq: master gain must be given explicitly when writing bands to slot %d: the reported master gain belongs to the active slot %d", target, cur[0])
	}
	if master < -MasterMaxCenti || master > MasterMaxCenti {
		return nil, fmt.Errorf("eq: master gain %d centi-dB outside +/-%d", master, MasterMaxCenti)
	}
	out := make([]byte, n)
	out[0] = byte(target)
	out[1], out[2] = byte(int16(master)), byte(int16(master)>>8)
	for _, b := range op.Bands {
		c, err := CentiFromDB(b.Gain)
		if err != nil {
			return nil, fmt.Errorf("eq: band %d: %w", b.Index, err)
		}
		o := eqHeader + b.Index*eqBandSize
		out[o], out[o+1] = byte(b.Freq), byte(b.Freq>>8)
		out[o+2], out[o+3] = byte(int16(c)), byte(int16(c)>>8)
		out[o+4], out[o+5] = byte(b.Q), byte(b.Q>>8)
		out[o+6] = byte(b.Type)
	}
	if n == EqReadLen && target == SlotCustomApp {
		for i, used := range seen {
			if !used {
				out[eqHeader+i*eqBandSize+eqBandSize-1] = emptyTypeMarker
			}
		}
	}
	return out, nil
}

// EqMatch returns the predicate that decides whether a device echo confirms a
// write of sent. Zero tolerance: the echo must be at least as long as sent and
// its first len(sent) bytes (slot, master gain, every band written) must equal
// sent, with one exception: a position that holds no band (freq, gain and q all
// zero) matches an empty position whose type byte is 0 or the 0xFF marker, in
// either direction, because the device reports its empty positions with the
// marker whichever of the two was written. The echo may be longer (73 sent, 143
// echoed), but then the part beyond what was sent must hold no band
// (TailBlank): a 73-byte write that the device answers with a populated tail
// did not produce the curve that was meant. op is kept for call-site
// compatibility.
func EqMatch(sent []byte, op EqOp) func([]byte) bool {
	return func(got []byte) bool {
		if len(sent) < EqLen || len(got) < len(sent) {
			return false
		}
		if !sameBands(sent, got[:len(sent)]) {
			return false
		}
		// TailBlank takes the whole payload (it inspects bytes EqLen.. in place).
		if len(sent) == EqLen && len(got) > EqLen && !TailBlank(got) {
			return false
		}
		return true
	}
}

// sameBands compares two payloads of equal length position by position; see
// EqMatch for the empty-position allowance.
func sameBands(a, b []byte) bool {
	if !bytes.Equal(a[:eqHeader], b[:eqHeader]) {
		return false
	}
	off := eqHeader
	for ; off+eqBandSize <= len(a); off += eqBandSize {
		x, y := a[off:off+eqBandSize], b[off:off+eqBandSize]
		if bytes.Equal(x, y) || (emptyPosition(x) && emptyPosition(y)) {
			continue
		}
		return false
	}
	return bytes.Equal(a[off:], b[off:])
}

// emptyPosition reports whether one 7-byte band position holds no band: freq,
// gain and q are zero and the type byte is 0 or the 0xFF marker.
func emptyPosition(p []byte) bool {
	for _, c := range p[:eqBandSize-1] {
		if c != 0 {
			return false
		}
	}
	return p[eqBandSize-1] == 0 || p[eqBandSize-1] == emptyTypeMarker
}

// EqWriteFailure is the error for a write whose echo never matched. got is the
// last 0x22 payload the device reported (nil when it said nothing), backup the
// path the pre-write payload was saved at. When the device keeps reporting a
// different slot than the one written the result is a *EqError with
// CodeUnconfirmed: the write was sent but the device does not report that slot
// as active, so what it stored cannot be told from here. Anything else is a
// plain readback mismatch.
func EqWriteFailure(sent, got []byte, backup string) error {
	if len(sent) > 0 && len(got) > 0 && got[0] != sent[0] {
		return &EqError{
			Code: CodeUnconfirmed,
			Msg: fmt.Sprintf("eq: written but unconfirmed: slot %d was sent but the device does not report it as active (it reports slot %d); the earbuds may or may not have stored it. sent %x, device reported %x; previous payload saved at %s",
				sent[0], got[0], sent, got, backup),
			Sent: append([]byte(nil), sent...), Got: append([]byte(nil), got...), Backup: backup,
		}
	}
	return fmt.Errorf("eq: readback mismatch: want %x, device reported %x; the device may nevertheless have applied the value (previous payload saved at %s)", sent, got, backup)
}

// ParseGains parses "g1,...,g10" (dB) into centi-dB, rejecting NaN/Inf and
// out-of-range values rather than clamping them.
func ParseGains(s string) ([EqBands]int, error) {
	var out [EqBands]int
	parts := strings.Split(s, ",")
	if len(parts) != EqBands {
		return out, fmt.Errorf("need %d gains, got %d", EqBands, len(parts))
	}
	for i, part := range parts {
		db, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return out, fmt.Errorf("invalid gain %q", part)
		}
		c, err := CentiFromDB(db)
		if err != nil {
			return out, fmt.Errorf("band %d: %w", i+1, err)
		}
		out[i] = c
	}
	return out, nil
}
