package credit

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// Challenges issues one-time signing challenges so a node operator can
// prove ownership of a peer ID (via its libp2p private key) and claim an
// API key for that account. This binds spending rights to the key holder
// actually controlling the peer ID — no one can claim another node's
// earnings with an arbitrary string.
type Challenges struct {
	mu sync.Mutex
	// peerID -> challenge
	pending map[string]challenge
}

type challenge struct {
	value     string
	expiresAt time.Time
}

// NewChallenges creates a challenge store.
func NewChallenges() *Challenges {
	return &Challenges{pending: make(map[string]challenge)}
}

// Issue creates a fresh challenge for a peer ID (5 minute expiry).
func (c *Challenges) Issue(peerID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("credit: challenge: %w", err)
	}
	val := base64.RawURLEncoding.EncodeToString(raw)
	c.mu.Lock()
	c.pending[peerID] = challenge{value: val, expiresAt: time.Now().Add(5 * time.Minute)}
	c.mu.Unlock()
	return val, nil
}

// Consume validates and removes a challenge. Returns false when unknown or
// expired (single-use: replaying a signature fails).
func (c *Challenges) Consume(peerID, value string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.pending[peerID]
	if !ok || ch.value != value || time.Now().After(ch.expiresAt) {
		return false
	}
	delete(c.pending, peerID)
	return true
}
