package credit

import (
	"sync"
	"testing"
)

func TestEarnAndBalance(t *testing.T) {
	l := NewMemoryLedger()
	if _, err := l.Earn("alice", "job1", "node1", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Earn("alice", "job2", "node1", 50); err != nil {
		t.Fatal(err)
	}
	if b := l.Balance("alice"); b != 150 {
		t.Fatalf("balance: got %d want 150", b)
	}
}

func TestSpend(t *testing.T) {
	l := NewMemoryLedger()
	_, _ = l.Earn("alice", "job1", "node1", 100)
	if _, err := l.Spend("alice", "job9", 40); err != nil {
		t.Fatal(err)
	}
	if b := l.Balance("alice"); b != 60 {
		t.Fatalf("balance: got %d want 60", b)
	}
}

func TestSpendInsufficient(t *testing.T) {
	l := NewMemoryLedger()
	_, _ = l.Earn("alice", "job1", "node1", 10)
	if _, err := l.Spend("alice", "job9", 11); err != ErrInsufficient {
		t.Fatalf("expected ErrInsufficient, got %v", err)
	}
	if b := l.Balance("alice"); b != 10 {
		t.Fatalf("failed spend must not change balance: %d", b)
	}
}

func TestHistoryAppendOnly(t *testing.T) {
	l := NewMemoryLedger()
	_, _ = l.Earn("alice", "job1", "node1", 100)
	_, _ = l.Spend("alice", "job9", 40)
	h := l.History("alice")
	if len(h) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(h))
	}
	if h[0].Kind != Earn || h[1].Kind != Spend {
		t.Fatal("history order wrong")
	}
	if h[0].ID == h[1].ID {
		t.Fatal("entry IDs must be unique")
	}
	// Recomputing balance from history must match Balance().
	var b int64
	for _, e := range h {
		if e.Kind == Earn {
			b += e.Amount
		} else {
			b -= e.Amount
		}
	}
	if b != l.Balance("alice") {
		t.Fatal("balance/history divergence")
	}
}

func TestInvalidAmounts(t *testing.T) {
	l := NewMemoryLedger()
	if _, err := l.Earn("a", "j", "n", 0); err == nil {
		t.Fatal("zero earn should fail")
	}
	if _, err := l.Spend("a", "j", -5); err == nil {
		t.Fatal("negative spend should fail")
	}
}

func TestConcurrentSafe(t *testing.T) {
	l := NewMemoryLedger()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.Earn("alice", "j", "n", 1)
		}()
	}
	wg.Wait()
	if b := l.Balance("alice"); b != 50 {
		t.Fatalf("concurrent earn lost entries: %d", b)
	}
}

func TestSpendUpTo(t *testing.T) {
	l := NewMemoryLedger()
	if _, err := l.Earn("a", "j1", "n1", 100); err != nil {
		t.Fatal(err)
	}
	spent, err := l.SpendUpTo("a", "j2", 30)
	if err != nil || spent != 30 {
		t.Fatalf("expected spend 30, got %d, %v", spent, err)
	}
	// Overspend takes what's left.
	spent, err = l.SpendUpTo("a", "j3", 1000)
	if err != nil || spent != 70 {
		t.Fatalf("expected spend 70 (remainder), got %d, %v", spent, err)
	}
	if b := l.Balance("a"); b != 0 {
		t.Fatalf("expected 0 balance, got %d", b)
	}
	// Empty account fails.
	if _, err := l.SpendUpTo("a", "j4", 1); err == nil {
		t.Fatal("expected error on empty account")
	}
}

func TestAPIKeys(t *testing.T) {
	k := NewAPIKeys()
	id, key, err := k.CreateAccount()
	if err != nil || id == "" || key == "" {
		t.Fatalf("create failed: %v", err)
	}
	if got, ok := k.Authenticate(key); !ok || got != id {
		t.Fatalf("auth failed for valid key")
	}
	if _, ok := k.Authenticate("bogus"); ok {
		t.Fatal("bogus key authenticated")
	}
}

func TestReservePreventsDoubleSpend(t *testing.T) {
	l := NewMemoryLedger()
	if _, err := l.Earn("a", "j1", "n1", 100); err != nil {
		t.Fatal(err)
	}
	r1, ok := l.Reserve("a", 80)
	if !ok {
		t.Fatal("first reserve failed")
	}
	// Second concurrent reserve for 30 must fail: only 20 available.
	if _, ok := l.Reserve("a", 30); ok {
		t.Fatal("double-spend: second reserve succeeded")
	}
	if avail := l.Available("a"); avail != 20 {
		t.Fatalf("expected available 20, got %d", avail)
	}
	// Commit spends actual and releases.
	if err := l.Commit(r1, 60); err != nil {
		t.Fatal(err)
	}
	if b := l.Balance("a"); b != 40 {
		t.Fatalf("expected balance 40, got %d", b)
	}
	// Unknown reservation fails.
	if err := l.Commit("res-999", 10); err == nil {
		t.Fatal("expected error for unknown reservation")
	}
}

func TestChallenges(t *testing.T) {
	c := NewChallenges()
	ch, err := c.Issue("peer1")
	if err != nil || ch == "" {
		t.Fatalf("issue failed: %v", err)
	}
	if !c.Consume("peer1", ch) {
		t.Fatal("valid consume failed")
	}
	// Single-use: replay fails.
	if c.Consume("peer1", ch) {
		t.Fatal("replay succeeded")
	}
}
