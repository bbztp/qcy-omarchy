package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/hearing"
	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// Hearing test (phase 2): two raw-frame commands, only for a model whose
// handler drives them (Capabilities.HearingTest: the N70).
//
//	hearingMode  {on: bool, confirm:true}           FF 03 45 01 0x   (0x45: NOT verified on hardware)
//	hearingWrite {ear, gains[8], confirm:true,      FF <n> 46|47 <n-2> 0B <mg> {bands}
//	              otherGains[8]?}
//
// What the device answers to 0x45/0x46/0x47 was never captured. The daemon
// therefore waits a short window for a frame of the same opcode (an echo or an
// unsolicited notification) and reports whether one came, but never fails on
// its absence: the authoritative evidence is the 0x22 readback it adds to the
// result (slot, effect, master gain as the device reports them afterwards).

// hearingAckTimeout is how long a frame of the written opcode is awaited. A
// variable so tests need not wait it out.
var hearingAckTimeout = 1500 * time.Millisecond

// hearingReadTimeout bounds the 0x22 readback.
var hearingReadTimeout = 3 * time.Second

type hearingReadback struct {
	Slot       int     `json:"slot"`
	Effect     string  `json:"effect,omitempty"`
	MasterGain float64 `json:"masterGain"`
	Payload    string  `json:"payload"`
}

type hearingResult struct {
	// Sent is the frame that went on the wire, hex.
	Sent string `json:"sent"`
	// Ack is true when a frame of the same opcode arrived within the window;
	// AckPayload is its body (hex).
	Ack        bool   `json:"ack"`
	AckPayload string `json:"ackPayload,omitempty"`
	// MasterGain is the dB value written (hearingWrite only); MasterClamped
	// says the rule's value was outside +-12 dB and was limited.
	MasterGain    *float64 `json:"masterGain,omitempty"`
	MasterClamped bool     `json:"masterClamped,omitempty"`
	// Readback is the 0x22 state read after the send; ReadbackError says why
	// it is missing.
	Readback      *hearingReadback `json:"readback,omitempty"`
	ReadbackError string           `json:"readbackError,omitempty"`
}

// hearingGains decodes an array of exactly hearing.Bands integers within
// -8..+6.
func hearingGains(v interface{}, what string) ([hearing.Bands]float64, error) {
	var g [hearing.Bands]float64
	arr, ok := v.([]interface{})
	if !ok {
		return g, fmt.Errorf("%s must be an array of %d integers", what, hearing.Bands)
	}
	if len(arr) != hearing.Bands {
		return g, fmt.Errorf("%s needs %d values, got %d", what, hearing.Bands, len(arr))
	}
	for i, x := range arr {
		f, ok := x.(float64)
		if !ok || math.IsNaN(f) || f != math.Trunc(f) {
			return g, fmt.Errorf("%s[%d] must be an integer", what, i)
		}
		if f < hearing.MinStep || f > hearing.MaxGain {
			return g, fmt.Errorf("%s[%d] = %v is outside %d..+%d", what, i, f, hearing.MinStep, hearing.MaxGain)
		}
		g[i] = f
	}
	return g, nil
}

// hearingMode switches the personalised-effect flag.
func (d *daemon) hearingMode(args map[string]interface{}) (*hearingResult, error) {
	if c, _ := args["confirm"].(bool); !c {
		return nil, errors.New("hearingMode: refused, the request needs confirm:true; nothing was sent")
	}
	on, ok := args["on"].(bool)
	if !ok {
		return nil, errors.New("hearingMode: on must be true or false")
	}
	return d.hearingSend(hearing.FlagFrame(on), nil)
}

// hearingWrite validates the request and writes the curve frame of one ear.
// Without confirm:true nothing is sent. The master gain is the one the app's
// rule gives for these gains; when otherGains (the opposite ear) is present the
// lower of the two is used, so both ears' frames carry the same value.
func (d *daemon) hearingWrite(args map[string]interface{}) (*hearingResult, error) {
	if c, _ := args["confirm"].(bool); !c {
		return nil, errors.New("hearingWrite: refused, the request needs confirm:true; nothing was sent")
	}
	var ear hearing.Ear
	switch args["ear"] {
	case "left":
		ear = hearing.Left
	case "right":
		ear = hearing.Right
	default:
		return nil, errors.New(`hearingWrite: ear must be "left" or "right"`)
	}
	gains, err := hearingGains(args["gains"], "gains")
	if err != nil {
		return nil, fmt.Errorf("hearingWrite: %w", err)
	}
	mg := hearing.MasterGain(gains)
	if v, ok := args["otherGains"]; ok && v != nil {
		other, err := hearingGains(v, "otherGains")
		if err != nil {
			return nil, fmt.Errorf("hearingWrite: %w", err)
		}
		if o := hearing.MasterGain(other); o < mg {
			mg = o
		}
	}
	mg, clamped := hearing.ClampMaster(mg)
	// baseline: "factory" (default, the 8 test bands) or "zero" (EXPERIMENT:
	// the 5 factory bands at gain 0 first, 13 bands per ear).
	frame := hearing.WriteFrame(ear, gains, mg)
	b := ""
	if v, present := args["baseline"]; present {
		str, ok := v.(string)
		if !ok {
			return nil, errors.New(`hearingWrite: baseline must be the string "factory" or "zero"`)
		}
		b = str
	}
	switch b {
	case "", "factory":
	case "zero":
		frame = hearing.WriteFrameBands(ear, hearing.ZeroBaselineBands(gains), mg)
	default:
		return nil, fmt.Errorf(`hearingWrite: baseline must be "factory" or "zero", got %q`, b)
	}
	res, err := d.hearingSend(frame, &mg)
	if res != nil {
		res.MasterClamped = clamped
	}
	return res, err
}

// hearingSend sends one raw frame under txMu, waits for an echo of its opcode
// and reads 0x22 back.
func (d *daemon) hearingSend(frame []byte, mg *float64) (*hearingResult, error) {
	if len(frame) < 4 || int(frame[3]) != len(frame)-4 {
		return nil, fmt.Errorf("hearing: malformed frame %x", frame)
	}
	opcode, params := frame[2], frame[4:]
	if !bytes.Equal(buildFrame(opcode, params), frame) {
		return nil, fmt.Errorf("hearing: frame %x does not match the transport framing", frame)
	}
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if !d.isConnected() {
		return nil, errors.New("not connected")
	}
	d.drain()
	if err := d.send(opcode, params); err != nil {
		return nil, err
	}
	res := &hearingResult{Sent: hex.EncodeToString(frame), MasterGain: mg}
	deadline := time.After(hearingAckTimeout)
wait:
	for {
		select {
		case f := <-d.frames:
			if f.cmd == opcode {
				res.Ack, res.AckPayload = true, hex.EncodeToString(f.params)
				break wait
			}
		case <-deadline:
			break wait
		}
	}
	cur, err := d.queryLocked(qcycore.EqOpcode, hearingReadTimeout)
	if err != nil {
		res.ReadbackError = err.Error()
		return res, nil
	}
	snap, err := qcycore.DecodeEq(cur)
	if err != nil {
		res.ReadbackError = err.Error()
		return res, nil
	}
	res.Readback = &hearingReadback{Slot: snap.Slot, Effect: snap.Effect, MasterGain: snap.MasterGain, Payload: snap.Payload}
	return res, nil
}
