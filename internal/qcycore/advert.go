package qcycore

// Manufacturer-data company IDs the earbuds advertise under (measured: the
// first; the second is an alternative some units are reported to use).
const (
	AdvCompanyID    uint16 = 0x521C
	AdvCompanyIDAlt uint16 = 0x522C
	advMinPayload          = 20
)

// ParseAdvert reads the vendorId and the colorIndex out of one manufacturer
// data record (payload without the company ID). The vendorId is bytes 0-1,
// big-endian; the colorIndex is bits 1-3 of byte 3 (0..7), the value the
// product list's colour entries are keyed by. ok is false for another company
// or a payload shorter than 20 bytes.
func ParseAdvert(company uint16, data []byte) (vendor uint16, color uint8, ok bool) {
	if company != AdvCompanyID && company != AdvCompanyIDAlt {
		return 0, 0, false
	}
	if len(data) < advMinPayload {
		return 0, 0, false
	}
	return uint16(data[0])<<8 | uint16(data[1]), (data[3] & 0x0E) >> 1, true
}
