package credit

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sync"
)

// APIKeys issues and verifies bearer keys for credit accounts.
// Keys are stored as SHA-256 hashes; the raw key is shown once at creation.
type APIKeys struct {
	mu   sync.Mutex
	keys map[[32]byte]string // key hash -> account ID
}

// NewAPIKeys creates an empty key store.
func NewAPIKeys() *APIKeys {
	return &APIKeys{keys: make(map[[32]byte]string)}
}

// CreateAccount generates a new account ID and API key.
// The key is returned once; only its hash is retained.
func (a *APIKeys) CreateAccount() (accountID, apiKey string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("credit: key generation: %w", err)
	}
	apiKey = base64.RawURLEncoding.EncodeToString(raw)
	accountID = "acct-" + base64.RawURLEncoding.EncodeToString(raw[:12])
	a.IssueFor(accountID, apiKey)
	return accountID, apiKey, nil
}

// IssueFor binds a raw API key to a specific account ID (e.g. a peer ID
// whose ownership was proven via challenge-response). The key is hashed;
// the raw value must be delivered to the owner out-of-band.
func (a *APIKeys) IssueFor(accountID, apiKey string) {
	h := sha256.Sum256([]byte(apiKey))
	a.mu.Lock()
	a.keys[h] = accountID
	a.mu.Unlock()
}

// NewKey generates a random API key string (for IssueFor callers).
func NewKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("credit: key generation: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Authenticate returns the account ID for a bearer key.
func (a *APIKeys) Authenticate(apiKey string) (string, bool) {
	h := sha256.Sum256([]byte(apiKey))
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.keys[h]
	return id, ok
}

// Count reports the number of issued keys (for observability).
func (a *APIKeys) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.keys)
}
