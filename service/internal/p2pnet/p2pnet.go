// Package p2pnet gives the central service its own libp2p host.
//
// The service needs a host for two M3b jobs: dial-back verification (is a
// node really directly reachable?) and remote job dispatch over
// /distributed-ai/job/1. Identity persists at
// ~/.distributed-ai-service/identity.key so the service keeps one Peer ID.
package p2pnet

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"

	jobprotocol "distributed-ai/protocol"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
)

// Default listen addresses. Port 4002: the node runtime uses 4001, so both
// can run on the same box (the Oracle deployment does exactly this).
var defaultAddrs = []string{
	"/ip4/0.0.0.0/tcp/4002",
	"/ip4/0.0.0.0/udp/4002/quic-v1",
}

// New creates the service host with a persistent identity.
func New() (host.Host, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newHost(home, defaultAddrs...)
}

// NewWithAddrs is New with explicit listen addresses (tests use ephemeral).
func NewWithAddrs(addrs ...string) (host.Host, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newHost(home, addrs...)
}

// newHost builds a host with identity stored under home. Tests pass a temp
// dir so each host gets a distinct identity.
func newHost(home string, addrs ...string) (host.Host, error) {
	priv, err := loadOrCreateIdentity(home)
	if err != nil {
		return nil, err
	}
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(addrs...),
	)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func loadOrCreateIdentity(home string) (crypto.PrivKey, error) {
	dir := filepath.Join(home, ".distributed-ai-service")
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

// ConnectNode establishes a libp2p connection to the node at peerID,
// trying candidates in order:
//
//  1. The node's reported libp2p addresses (skipping unspecified 0.0.0.0).
//  2. The HTTP source IP the node registered from, combined with the p2p
//
// ConnectNode establishes a connection to a node. Delegates to the shared
// protocol.ConnectPeer so the service and nodes use one dial algorithm.
func ConnectNode(ctx context.Context, h host.Host, peerID string, addrs []string, sourceIP string) error {
	return jobprotocol.ConnectPeer(ctx, h, peerID, addrs, sourceIP)
}

// DialBack reports whether the node is directly dialable, and how long the
// dial took. It is the honest measurement behind the registry's DirectOK
// and RTTMs — never self-reported.
func DialBack(ctx context.Context, h host.Host, peerID string, addrs []string, sourceIP string) (bool, time.Duration) {
	start := time.Now()
	err := ConnectNode(ctx, h, peerID, addrs, sourceIP)
	return err == nil, time.Since(start)
}

// PeerAddrString renders "multiaddr/p2p/peerID" for logging.
func PeerAddrString(h host.Host) string {
	if len(h.Addrs()) == 0 {
		return h.ID().String() + " (no addrs)"
	}
	return fmt.Sprintf("%s/p2p/%s", h.Addrs()[0], h.ID())
}
