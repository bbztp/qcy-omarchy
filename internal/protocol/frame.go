package protocol

import (
	"errors"
	"fmt"
)

// Frame is one decoded block: <cmd> <paramLen> <params...>.
type Frame struct {
	Cmd    byte
	Params []byte
}

// Block encodes one command as FF <bodyLen> <cmd> <paramLen> <params...>.
// bodyLen counts every byte after itself, so bodyLen = paramLen + 2. Lengths
// are single bytes: params longer than 255 bytes wrap, as the device cannot
// carry them either.
func Block(cmd byte, params ...byte) []byte {
	p := make([]byte, 0, 4+len(params))
	p = append(p, SOF, byte(len(params)+2), cmd, byte(len(params)))
	return append(p, params...)
}

// Query encodes a read request for opcode: FF 03 FE 01 <opcode>.
func Query(opcode byte) []byte { return Block(OpQuery, opcode) }

// ParseBlocks decodes a notification payload into frames. A payload may carry
// several FF frames, and each frame several blocks.
func ParseBlocks(buf []byte) ([]Frame, error) {
	var out []Frame
	off := 0
	for off < len(buf) {
		if buf[off] != SOF {
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
			out = append(out, Frame{Cmd: op, Params: append([]byte(nil), body[bo:bo+n]...)})
			bo += n
		}
		off = end
	}
	if len(out) == 0 {
		return nil, errors.New("no frames")
	}
	return out, nil
}
