// Package qcycore holds the pure, hardware-free parts shared by qcyctl and
// qcyctld: the 0x22 (EQ) payload layout and patching, the EQ backup store and
// the daemon socket location. Nothing here touches Bluetooth.
package qcycore

import (
	"bytes"
	"encoding/hex"
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
// (any other non-zero byte) the full read length is sent instead (format NOT proven on the device);
// results then carry fullLength=true.
const (
	EqOpcode = 0x22
	// EqLen is the proven write length; EqReadLen the length the device reports.
	EqLen      = 73
	EqReadLen  = 143
	EqBands    = 10 // positions addressable by `eq set` / EqOp.Gains
	EqMaxBands = 20 // positions in the full payload
	eqBandSize = 7
	eqHeader   = 3

	// MaxCenti is the accepted range for band gains, in centi-dB (+/-8 dB).
	MaxCenti = 800
	MaxDB    = 8.0
	// MasterMaxCenti / MasterMaxDB: the master gain range (+/-12 dB).
	MasterMaxCenti = 1200
	MasterMaxDB    = 12.0
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
	MasterGain float64  `json:"masterGain"`
	Bands      []EqBand `json:"bands"`
	Payload    string   `json:"payload"` // raw payload, lowercase hex
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

// EqOp describes a patch. Nil fields mean "keep what the device has".
type EqOp struct {
	Gains       *[EqBands]int // centi-dB per band
	MasterCenti *int          // centi-dB
	Slot        *int          // target slot byte (0-255)
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
// anything outside +/-8 dB BEFORE any integer conversion (a float outside int
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

// ApplyEq returns the payload to write: a copy of cur with op applied. Only the
// fields named in op change; everything else (every band's freq/Q/type, bands
// 10-19, unknown bytes) is carried over byte for byte. The length is EqLen (the
// proven format) when bytes EqLen.. of cur are blank (TailBlank), otherwise the full
// length read (unproven; see FullLength).
func ApplyEq(cur []byte, op EqOp) ([]byte, error) {
	if err := ValidateEq(cur); err != nil {
		return nil, err
	}
	n := len(cur)
	if TailBlank(cur) {
		n = EqLen
	}
	out := append([]byte(nil), cur[:n]...)
	if op.Slot != nil {
		if *op.Slot < 0 || *op.Slot > 255 {
			return nil, fmt.Errorf("eq: slot must be 0-255, got %d", *op.Slot)
		}
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

// EqMatch returns the predicate that decides whether a device echo confirms a
// write of sent. Zero tolerance: the echo must be at least as long as sent and
// its first len(sent) bytes (slot, master gain, every band written) must equal
// sent exactly. The echo may be longer (73 sent, 143 echoed). op is kept for
// call-site compatibility.
func EqMatch(sent []byte, op EqOp) func([]byte) bool {
	return func(got []byte) bool {
		if len(sent) < EqLen || len(got) < len(sent) {
			return false
		}
		return bytes.Equal(got[:len(sent)], sent)
	}
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
