package qcycore

import "testing"

func advPayload(vendor uint16, b3 byte, n int) []byte {
	b := make([]byte, n)
	b[0], b[1], b[3] = byte(vendor>>8), byte(vendor), b3
	return b
}

func TestParseAdvert(t *testing.T) {
	cases := []struct {
		name    string
		company uint16
		data    []byte
		vendor  uint16
		color   uint8
		ok      bool
	}{
		{"colour 0", AdvCompanyID, advPayload(23872, 0x00, 24), 23872, 0, true},
		{"bits 1-3 only", AdvCompanyID, advPayload(23872, 0x0E, 24), 23872, 7, true},
		{"bit 0 ignored", AdvCompanyID, advPayload(23872, 0x01, 24), 23872, 0, true},
		{"bits 3-4 are not the colour", AdvCompanyID, advPayload(23872, 0x18, 24), 23872, 4, true},
		{"high bits ignored", AdvCompanyID, advPayload(23872, 0xF2, 24), 23872, 1, true},
		{"alternative company", AdvCompanyIDAlt, advPayload(19786, 0x06, 20), 19786, 3, true},
		{"exactly 20 bytes", AdvCompanyID, advPayload(100, 0x04, 20), 100, 2, true},
		{"payload too short", AdvCompanyID, advPayload(23872, 0x0E, 19), 0, 0, false},
		{"nil payload", AdvCompanyID, nil, 0, 0, false},
		{"other company", 0x004C, advPayload(23872, 0x0E, 24), 0, 0, false},
	}
	for _, c := range cases {
		v, col, ok := ParseAdvert(c.company, c.data)
		if ok != c.ok || v != c.vendor || col != c.color {
			t.Errorf("%s: got %d,%d,%v want %d,%d,%v", c.name, v, col, ok, c.vendor, c.color, c.ok)
		}
	}
}
