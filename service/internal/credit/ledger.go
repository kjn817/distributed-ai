// Package credit is the append-only credit ledger.
//
// v1: in-memory. The interface is storage-agnostic so PostgreSQL can back it
// later without changing callers. Credits are integer base units; balances
// can never go negative — Spend fails instead of overdrawing.
//
// This is real accounting, not a mock: every Earn/Spend appends an entry and
// Balance always reflects the entry history.
package credit

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrInsufficient means the account cannot cover the spend.
var ErrInsufficient = errors.New("credit: insufficient balance")

// Kind is earn or spend.
type Kind string

const (
	Earn  Kind = "earn"
	Spend Kind = "spend"
)

// Entry is one ledger line. Append-only: entries are never modified.
type Entry struct {
	ID        int64     `json:"id"`
	AccountID string    `json:"account_id"`
	Kind      Kind      `json:"kind"`
	Amount    int64     `json:"amount"`
	JobID     string    `json:"job_id"`
	NodeID    string    `json:"node_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Ledger is the credit ledger interface.
type Ledger interface {
	Earn(accountID, jobID, nodeID string, amount int64) (Entry, error)
	Spend(accountID, jobID string, amount int64) (Entry, error)
	// SpendUpTo debits up to amount (what's available); returns the spent sum.
	SpendUpTo(accountID, jobID string, amount int64) (int64, error)
	// Reserve holds maxAmount for a future Commit; fails if the available
	// (unheld) balance is insufficient. Prevents double-spend across
	// concurrent requests (no check-then-act race).
	Reserve(accountID string, maxAmount int64) (reservationID string, ok bool)
	// Commit spends actualAmount against a reservation and releases the hold.
	Commit(reservationID string, actualAmount int64) error
	// Release drops a reservation without spending.
	Release(reservationID string)
	// Available is Balance minus outstanding reservations.
	Available(accountID string) int64
	Balance(accountID string) int64
	History(accountID string) []Entry
}

// reservation is an outstanding spending hold.
type reservation struct {
	accountID string
	maxAmount int64
}

// MemoryLedger is the in-memory Ledger.
type MemoryLedger struct {
	mu      sync.Mutex
	entries []Entry
	byAcct  map[string][]int64 // account -> entry indexes
	holds   map[string]reservation
	nextID  int64
	nextRes int64
}

// NewMemoryLedger creates an empty ledger.
func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{byAcct: make(map[string][]int64), holds: make(map[string]reservation)}
}

func (l *MemoryLedger) append(e Entry) Entry {
	l.nextID++
	e.ID = l.nextID
	e.CreatedAt = time.Now().UTC()
	l.entries = append(l.entries, e)
	l.byAcct[e.AccountID] = append(l.byAcct[e.AccountID], int64(len(l.entries)-1))
	return e
}

// Earn credits an account for contributed work.
func (l *MemoryLedger) Earn(accountID, jobID, nodeID string, amount int64) (Entry, error) {
	if amount <= 0 {
		return Entry{}, errors.New("credit: earn amount must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.append(Entry{AccountID: accountID, Kind: Earn, Amount: amount, JobID: jobID, NodeID: nodeID}), nil
}

// Spend debits an account; fails when the balance is insufficient.
func (l *MemoryLedger) Spend(accountID, jobID string, amount int64) (Entry, error) {
	if amount <= 0 {
		return Entry{}, errors.New("credit: spend amount must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.balanceLocked(accountID) < amount {
		return Entry{}, ErrInsufficient
	}
	return l.append(Entry{AccountID: accountID, Kind: Spend, Amount: amount, JobID: jobID}), nil
}

// SpendUpTo debits up to amount — whatever the balance covers. Used for
// post-inference deduction: the work already happened, so we take what's
// there instead of failing. Returns the amount actually spent.
func (l *MemoryLedger) SpendUpTo(accountID, jobID string, amount int64) (int64, error) {
	if amount <= 0 {
		return 0, errors.New("credit: spend amount must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	bal := l.balanceLocked(accountID)
	if bal <= 0 {
		return 0, ErrInsufficient
	}
	spend := amount
	if spend > bal {
		spend = bal
	}
	l.append(Entry{AccountID: accountID, Kind: Spend, Amount: spend, JobID: jobID})
	return spend, nil
}

// Balance sums the account's history.
func (l *MemoryLedger) Balance(accountID string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balanceLocked(accountID)
}

// Available is the spendable balance: total minus outstanding reservations.
func (l *MemoryLedger) Available(accountID string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	avail := l.balanceLocked(accountID)
	for _, h := range l.holds {
		if h.accountID == accountID {
			avail -= h.maxAmount
		}
	}
	if avail < 0 {
		avail = 0
	}
	return avail
}

// Reserve holds maxAmount. The hold is atomic with the balance check, so
// two concurrent requests cannot both spend the same credits.
func (l *MemoryLedger) Reserve(accountID string, maxAmount int64) (string, bool) {
	if maxAmount <= 0 {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	avail := l.balanceLocked(accountID)
	for _, h := range l.holds {
		if h.accountID == accountID {
			avail -= h.maxAmount
		}
	}
	if avail < maxAmount {
		return "", false
	}
	l.nextRes++
	id := fmt.Sprintf("res-%d", l.nextRes)
	l.holds[id] = reservation{accountID: accountID, maxAmount: maxAmount}
	return id, true
}

// Commit spends actualAmount and releases the reservation.
// The hold only guaranteed availability; the charge is the real usage
// (capped at the balance, never at the reservation).
func (l *MemoryLedger) Commit(reservationID string, actualAmount int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.holds[reservationID]
	if !ok {
		return errors.New("credit: unknown reservation")
	}
	delete(l.holds, reservationID)
	if actualAmount <= 0 {
		return nil
	}
	spend := actualAmount
	if bal := l.balanceLocked(h.accountID); spend > bal {
		spend = bal
	}
	if spend > 0 {
		l.append(Entry{AccountID: h.accountID, Kind: Spend, Amount: spend, JobID: reservationID})
	}
	return nil
}

// Release drops a reservation without spending.
func (l *MemoryLedger) Release(reservationID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.holds, reservationID)
}

func (l *MemoryLedger) balanceLocked(accountID string) int64 {
	var b int64
	for _, idx := range l.byAcct[accountID] {
		e := l.entries[idx]
		if e.Kind == Earn {
			b += e.Amount
		} else {
			b -= e.Amount
		}
	}
	return b
}

// History returns the account's entries in append order.
func (l *MemoryLedger) History(accountID string) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	idxs := l.byAcct[accountID]
	out := make([]Entry, len(idxs))
	for i, idx := range idxs {
		out[i] = l.entries[idx]
	}
	return out
}
