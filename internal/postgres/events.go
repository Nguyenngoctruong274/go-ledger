package postgres

import (
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

// TopicTransferPosted is the outbox topic for successfully posted transfers.
const TopicTransferPosted = "transfer.posted"

// TransferPostedEvent is the JSON payload published for every posted
// transfer. Field names are part of the public contract with consumers:
// add fields freely, but never rename or remove one.
type TransferPostedEvent struct {
	TransferID string               `json:"transfer_id"`
	Currency   string               `json:"currency"`
	Entries    []TransferEventEntry `json:"entries"`
	PostedAt   time.Time            `json:"posted_at"`
}

// TransferEventEntry is one leg of a posted transfer in an event.
type TransferEventEntry struct {
	AccountID    string `json:"account_id"`
	Amount       int64  `json:"amount"`
	BalanceAfter int64  `json:"balance_after"`
}

func newTransferPostedEvent(r ledger.Receipt) TransferPostedEvent {
	ev := TransferPostedEvent{
		TransferID: string(r.TransferID),
		Currency:   string(r.Currency),
		Entries:    make([]TransferEventEntry, len(r.Entries)),
		PostedAt:   r.CreatedAt.UTC(),
	}
	for i, e := range r.Entries {
		ev.Entries[i] = TransferEventEntry{
			AccountID: string(e.AccountID), Amount: int64(e.Amount), BalanceAfter: int64(e.BalanceAfter),
		}
	}
	return ev
}
