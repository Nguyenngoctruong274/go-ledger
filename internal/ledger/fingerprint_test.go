package ledger

import "testing"

func mustTransfer(t *testing.T, id TransferID, cur Currency, entries ...Entry) Transfer {
	t.Helper()
	tr, err := NewTransfer(id, cur, entries...)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestFingerprint(t *testing.T) {
	base := mustTransfer(t, "t1", "VND", Entry{"a", -100}, Entry{"b", 60}, Entry{"c", 40})

	same := map[string]Transfer{
		"different id":             mustTransfer(t, "other-id", "VND", Entry{"a", -100}, Entry{"b", 60}, Entry{"c", 40}),
		"entries in another order": mustTransfer(t, "t1", "VND", Entry{"c", 40}, Entry{"a", -100}, Entry{"b", 60}),
	}
	for name, tr := range same {
		if tr.Fingerprint() != base.Fingerprint() {
			t.Errorf("%s: fingerprint changed, want equal", name)
		}
	}

	different := map[string]Transfer{
		"currency":    mustTransfer(t, "t1", "USD", Entry{"a", -100}, Entry{"b", 60}, Entry{"c", 40}),
		"amount":      mustTransfer(t, "t1", "VND", Entry{"a", -100}, Entry{"b", 61}, Entry{"c", 39}),
		"account":     mustTransfer(t, "t1", "VND", Entry{"a", -100}, Entry{"b", 60}, Entry{"d", 40}),
		"fewer legs":  mustTransfer(t, "t1", "VND", Entry{"a", -100}, Entry{"b", 100}),
		"swapped amt": mustTransfer(t, "t1", "VND", Entry{"a", -100}, Entry{"b", 40}, Entry{"c", 60}),
	}
	for name, tr := range different {
		if tr.Fingerprint() == base.Fingerprint() {
			t.Errorf("%s: fingerprint unchanged, want different", name)
		}
	}
}

// Account ids are length-prefixed, so shifting characters between adjacent
// ids cannot produce the same byte stream.
func TestFingerprintBoundaries(t *testing.T) {
	x := mustTransfer(t, "t", "VND", Entry{"ab", -1}, Entry{"c", 1})
	y := mustTransfer(t, "t", "VND", Entry{"a", -1}, Entry{"bc", 1})
	if x.Fingerprint() == y.Fingerprint() {
		t.Fatal("fingerprints collide across id boundaries")
	}
}
