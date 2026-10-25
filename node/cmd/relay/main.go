// Command dain-relay is the DAIN Circuit Relay v2 + bootstrap node.
//
// It is pure connectivity infrastructure: it holds no service state,
// no credits, and no model data. Nodes reserve relay slots (Peer ID
// allowlist), advertise /p2p-circuit addresses through it, and use
// DCUtR to upgrade to direct connections whenever the NAT allows.
//
// Production shape (see docs/phase-a-relay-prototype.md): a dedicated
// public VM, a real domain, and a publicly trusted TLS certificate for
// the WSS listener, with a non-empty --allow allowlist. An empty
// allowlist means "allow everyone" and is for local tests only — the
// binary says so loudly at startup.
package main

import (
	"crypto/rand"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"distributed-ai/node/internal/p2p"
)

func loadOrCreateKey(dir string) (crypto.PrivKey, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyFile := filepath.Join(dir, "identity.key")
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
	if err := os.WriteFile(keyFile, data, 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

func main() {
	port := flag.Int("port", 4001, "TCP/QUIC listen port")
	wsPort := flag.Int("wss-port", 0, "WSS listen port (0 = disabled; requires -tls-cert and -tls-key)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (PEM) for WSS")
	tlsKey := flag.String("tls-key", "", "TLS private key file (PEM) for WSS")
	allow := flag.String("allow", "", "comma-separated member Peer IDs allowed to use this relay (empty = ALLOW ALL, local tests only)")
	dataDir := flag.String("data-dir", "", "identity directory (default ~/.distributed-ai-relay)")
	flag.Parse()

	dir := *dataDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatal(err)
		}
		dir = filepath.Join(home, ".distributed-ai-relay")
	}
	priv, err := loadOrCreateKey(dir)
	if err != nil {
		log.Fatal(err)
	}

	var allowlist []peer.ID
	if strings.TrimSpace(*allow) != "" {
		for _, s := range strings.Split(*allow, ",") {
			id, err := peer.Decode(strings.TrimSpace(s))
			if err != nil {
				log.Fatalf("bad --allow peer id %q: %v", s, err)
			}
			allowlist = append(allowlist, id)
		}
	}

	addrs := []string{
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port),
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", *port),
	}
	var tlsCfg *tls.Config
	if *wsPort > 0 {
		if *tlsCert == "" || *tlsKey == "" {
			log.Fatal("--wss-port requires --tls-cert and --tls-key (WSS without TLS is refused)")
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("load TLS pair: %v", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		addrs = append(addrs, fmt.Sprintf("/ip4/0.0.0.0/tcp/%d/wss", *wsPort))
	}

	h, err := p2p.NewRelayHost(priv, p2p.RelayConfig{
		ListenAddrs: addrs,
		Allowlist:   allowlist,
		TLSConfig:   tlsCfg,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer h.Close()

	fmt.Println("dain-relay up")
	fmt.Println("Peer ID:", h.ID())
	if len(allowlist) == 0 {
		fmt.Println("WARNING: no --allow allowlist — this relay accepts EVERYONE. Local tests only.")
	} else {
		fmt.Printf("Allowlist enforced: %d member(s)\n", len(allowlist))
	}
	fmt.Println("Listening addresses (give nodes the /p2p/ form via --relay):")
	for _, a := range h.Addrs() {
		fmt.Printf("  %s/p2p/%s\n", a, h.ID())
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("dain-relay shutting down")
}
