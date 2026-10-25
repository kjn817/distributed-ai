// Command dain-relay is the DAIN Circuit Relay v2 + bootstrap node.
//
// It is pure connectivity infrastructure: it holds no service state,
// no credits, and no model data. Nodes reserve relay slots (Peer ID
// allowlist), advertise /p2p-circuit addresses through it, and use
// DCUtR to upgrade to direct connections whenever the NAT allows.
//
// Production shape (see docs/phase-a-relay-prototype.md): a dedicated
// public VM, a real domain, and a publicly trusted TLS certificate for
// the WSS listener, with a non-empty --allow allowlist.
//
// Default deny (2026-10-25): an empty allowlist refuses to start.
// Allow-all exists only as an explicit --allow-all opt-in for isolated
// local tests. Revocation is file-based: keep member Peer IDs in a
// file (--allow-file), remove a leaked ID from it, and restart the
// relay — see docs/security-architecture.md for the full procedure.
package main

import (
	"bufio"
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

// parseAllowlistIDs parses a comma-separated Peer ID list.
func parseAllowlistIDs(s string) ([]peer.ID, error) {
	var ids []peer.ID
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := peer.Decode(part)
		if err != nil {
			return nil, fmt.Errorf("bad peer id %q: %w", part, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// loadAllowlistFile reads one Peer ID per line. Blank lines and lines
// starting with # are ignored. This is the revocation surface: remove
// a leaked Peer ID from the file and restart the relay.
func loadAllowlistFile(path string) ([]peer.ID, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ids []peer.ID
	sc := bufio.NewScanner(f)
	// Lines are Peer IDs (< 100 bytes); the default 64 KiB token cap is ample.
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Allow a trailing inline comment.
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		id, err := peer.Decode(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: bad peer id %q: %w", path, lineNo, line, err)
		}
		ids = append(ids, id)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func main() {
	port := flag.Int("port", 4001, "TCP/QUIC listen port")
	wsPort := flag.Int("wss-port", 0, "WSS listen port (0 = disabled; requires -tls-cert and -tls-key)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (PEM) for WSS")
	tlsKey := flag.String("tls-key", "", "TLS private key file (PEM) for WSS")
	allow := flag.String("allow", "", "comma-separated member Peer IDs allowed to use this relay")
	allowFile := flag.String("allow-file", "", "file with one member Peer ID per line (combined with --allow; this is the revocation list — remove an ID and restart)")
	allowAll := flag.Bool("allow-all", false, "ALLOW EVERYONE: isolated local tests only; never use on a public relay (default deny otherwise)")
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

	seen := make(map[peer.ID]bool)
	var allowlist []peer.ID
	addIDs := func(ids []peer.ID) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				allowlist = append(allowlist, id)
			}
		}
	}
	if strings.TrimSpace(*allow) != "" {
		ids, err := parseAllowlistIDs(*allow)
		if err != nil {
			log.Fatal(err)
		}
		addIDs(ids)
	}
	if strings.TrimSpace(*allowFile) != "" {
		ids, err := loadAllowlistFile(*allowFile)
		if err != nil {
			log.Fatal(err)
		}
		addIDs(ids)
	}
	if len(allowlist) == 0 && !*allowAll {
		log.Fatal("relay allowlist is empty: refusing to start an open relay (pass --allow/--allow-file with member Peer IDs, or --allow-all for isolated local tests only)")
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
		AllowAll:    *allowAll,
		TLSConfig:   tlsCfg,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer h.Close()

	fmt.Println("dain-relay up")
	fmt.Println("Peer ID:", h.ID())
	if *allowAll {
		fmt.Println("WARNING: --allow-all set — this relay accepts EVERYONE. Isolated local tests only.")
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
