package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// Fingerprint is a SHA-256 digest of what a transfer does: its currency and
// the set of (account, amount) entries. It ignores the transfer id and the
// order in which entries were listed, so two requests that move the same
// money the same way have the same fingerprint.
//
// The store keeps it next to the transfer id so a retried request can be
// told apart from a different request that reuses the same id.
func (t Transfer) Fingerprint() [sha256.Size]byte {
	entries := t.Entries()
	sort.Slice(entries, func(i, j int) bool { return entries[i].AccountID < entries[j].AccountID })

	h := sha256.New()
	var buf [binary.MaxVarintLen64]byte
	writeString := func(s string) {
		// Length-prefixing keeps ("ab","c") and ("a","bc") distinct.
		n := binary.PutUvarint(buf[:], uint64(len(s)))
		h.Write(buf[:n])
		h.Write([]byte(s))
	}

	writeString(string(t.currency))
	for _, e := range entries {
		writeString(string(e.AccountID))
		var amt [8]byte
		// Reinterpreting the two's-complement bits is intended: it is a lossless encoding.
		binary.BigEndian.PutUint64(amt[:], uint64(e.Amount)) //nolint:gosec // G115: bit reinterpretation, not arithmetic
		h.Write(amt[:])
	}

	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}
