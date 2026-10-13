package protocol

// KeysConfirmed reports whether every keyId:funId pair of want appears in the
// table the earbuds returned (got). The table is a list of pairs in the
// earbuds' own order, so a write of one pair for a key that is not first is
// confirmed by finding that key's pair, not by a prefix match.
func KeysConfirmed(want, got []byte) bool {
	if len(want) == 0 || len(want)%2 != 0 || len(got)%2 != 0 {
		return false
	}
	for i := 0; i+1 < len(want); i += 2 {
		found := false
		for j := 0; j+1 < len(got); j += 2 {
			if got[j] == want[i] {
				found = got[j+1] == want[i+1]
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
