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
//	[slot:1][masterGain:int16 LE, centi-dB][10 x (freq:u16 LE, gain:int16 LE, q:u16 LE, type:u8)]
//
// 3 + 10*7 = 73 bytes (EqLen), the part whose layout is known and the only
// part ever written. The device REPORTS 143 bytes (EqReadLen): the 73 known
// bytes followed by 70 bytes that have always read zero. Frequencies, Q and
// type are NOT fixed: the vendor's hearing-test slot uses its own values, so a
// writer must preserve them.
const (
	EqOpcode = 0x22
	// EqLen is the known/written length; EqReadLen the length the device reports.
	EqLen      = 73
	EqReadLen  = 143
	EqBands    = 10
	eqBandSize = 7
	eqHeader   = 3

	// MaxCenti is the accepted range for both master gain and band gains, in
	// centi-dB (+/-8 dB).
	MaxCenti = 800
	MaxDB    = 8.0
)

// EqBand is one parametric band as stored in the payload.
type EqBand struct {
	Freq int     `json:"freq"`
	Gain float64 `json:"gain"` // dB
	Q    int     `json:"q"`
	Type int     `json:"type"`
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
	Previous      string `json:"previous"`      // payload before the write, hex
	BackupFirst   string `json:"backupFirst"`   // immutable first-seen backup of that slot
	BackupHistory string `json:"backupHistory"` // append-only history entry for this write
}

// EqOp describes a patch. Nil fields mean "keep what the device has".
type EqOp struct {
	Gains       *[EqBands]int // centi-dB per band
	MasterCenti *int          // centi-dB
	Slot        *int          // target slot byte (0-255)
}

// ValidateEq checks a payload read from the device is usable for a
// read-modify-write: at least EqLen bytes (EqReadLen expected), not the
// all-0xFF "no data" filler, and with an all-zero tail beyond EqLen. A
// non-zero tail has unknown semantics, so it is refused rather than
// silently truncated by a 73-byte write. Reporting uses the more tolerant
// DecodeEq.
func ValidateEq(p []byte) error {
	if len(p) < EqLen {
		return fmt.Errorf("eq: unexpected payload length %d (want at least %d, device reports %d)", len(p), EqLen, EqReadLen)
	}
	if IsAllFF(p) {
		return fmt.Errorf("eq: device returned an all-0xFF payload (no real EQ data); refusing to back up or write over it")
	}
	for i := EqLen; i < len(p); i++ {
		if p[i] != 0 {
			return fmt.Errorf("eq: payload tail not zero (byte %d = 0x%02x), refusing to write: unknown semantics", i, p[i])
		}
	}
	return nil
}

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
	if n > EqBands {
		n = EqBands
	}
	for i := 0; i < n; i++ {
		b := eqHeader + i*eqBandSize
		s.Bands = append(s.Bands, EqBand{
			Freq: int(le16(p, b)),
			Gain: float64(int16(le16(p, b+2))) / 100,
			Q:    int(le16(p, b+4)),
			Type: int(p[b+6]),
		})
	}
	return s, nil
}

func le16(p []byte, off int) uint16 { return uint16(p[off]) | uint16(p[off+1])<<8 }

func masterCenti(p []byte) int { return int(int16(le16(p, 1))) }

// CentiFromDB converts dB to centi-dB, rejecting NaN, Inf and anything outside
// +/-8 dB BEFORE any integer conversion (a float outside int range converts to
// an arbitrary value in Go).
func CentiFromDB(db float64) (int, error) {
	if math.IsNaN(db) || math.IsInf(db, 0) {
		return 0, fmt.Errorf("gain %v is not a finite number", db)
	}
	if db < -MaxDB || db > MaxDB {
		return 0, fmt.Errorf("gain %.2f dB is outside the supported range of -%g..+%g dB", db, MaxDB, MaxDB)
	}
	return int(math.Round(db * 100)), nil
}

// ApplyEq returns the EqLen-byte payload to write: the first EqLen bytes of cur
// with op applied. Only the fields named in op change; frequency, Q and type of
// every band are carried over byte for byte. The (all-zero, validated) tail the
// device reports beyond EqLen is not sent: the 73-byte form is the proven
// write format.
func ApplyEq(cur []byte, op EqOp) ([]byte, error) {
	if err := ValidateEq(cur); err != nil {
		return nil, err
	}
	out := append([]byte(nil), cur[:EqLen]...)
	if op.Slot != nil {
		if *op.Slot < 0 || *op.Slot > 255 {
			return nil, fmt.Errorf("eq: slot must be 0-255, got %d", *op.Slot)
		}
		out[0] = byte(*op.Slot)
	}
	if op.MasterCenti != nil {
		c := *op.MasterCenti
		if c < -MaxCenti || c > MaxCenti {
			return nil, fmt.Errorf("eq: master gain %d centi-dB outside +/-%d", c, MaxCenti)
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
// write of sent (EqLen bytes). The echo may be EqLen or EqReadLen long; only
// its first EqLen bytes are compared: slot, master gain and the gain of all 10
// bands. op is kept for call-site compatibility.
func EqMatch(sent []byte, op EqOp) func([]byte) bool {
	return func(got []byte) bool {
		if len(got) < EqLen || len(sent) != EqLen {
			return false
		}
		if !bytes.HasPrefix(got, sent[:eqHeader]) {
			return false
		}
		for i := 0; i < EqBands; i++ {
			off := eqHeader + i*eqBandSize + 2
			if got[off] != sent[off] || got[off+1] != sent[off+1] {
				return false
			}
		}
		return true
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
