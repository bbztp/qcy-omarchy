package protocol

import "testing"

func TestKeysConfirmedMatchesPairsAnywhereInTheTable(t *testing.T) {
	table := []byte{0x01, 0x06, 0x02, 0x05, 0x07, 0x01, 0x08, 0x00}
	cases := []struct {
		want []byte
		ok   bool
	}{
		{[]byte{0x01, 0x06}, true},
		{[]byte{0x07, 0x01}, true},             // not the first key
		{[]byte{0x02, 0x05, 0x08, 0x00}, true}, // several, out of order of the table is fine
		{[]byte{0x07, 0x02}, false},            // wrong function
		{[]byte{0x09, 0x00}, false},            // key absent
		{[]byte{0x01}, false},                  // odd length
		{nil, false},
	}
	for _, c := range cases {
		if got := KeysConfirmed(c.want, table); got != c.ok {
			t.Errorf("KeysConfirmed(%x) = %v, want %v", c.want, got, c.ok)
		}
	}
}
