// Package identity manages the node's persistent Ed25519 identity.
//
// Behavior preserved from the original runtime: the private key lives at
// ~/.distributed-ai-node/identity.key (0700 dir, 0600 file) so a node keeps
// the same Peer ID across restarts.
package identity

import (
	"crypto/rand"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
)

// LoadOrCreate reads the persisted key or generates and stores a new one.
func LoadOrCreate() (crypto.PrivKey, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(home, ".distributed-ai-node")
	keyFile := filepath.Join(dir, "identity.key")

	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}

	if data, err := os.ReadFile(keyFile); err == nil {
		return crypto.UnmarshalPrivateKey(data)
	}

	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}

	data, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(keyFile, data, 0600); err != nil {
		return nil, err
	}

	return priv, nil
}
