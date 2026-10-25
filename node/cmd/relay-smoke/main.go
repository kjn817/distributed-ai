// Command relay-smoke is the Phase A local experiment harness.
//
// It runs ONE libp2p peer as a separate OS process so the relay
// topology can be exercised in its real deployment shape (relay
// process + peer processes), not only in-process. The parent (a human
// or node/internal/p2p's smoke test) starts a dain-relay plus one or
// more relay-smoke peers and reads their SMOKE: lines.
//
// Modes:
//
//	svc   — receive ResourceInfo (V2), print GOT lines; with -reply,
//	        answer the sender with our own info over the same relay.
//	node  — with -sendto, send our ResourceInfo there (retrying until
//	        the relay path is up), print SENT; then keep listening.
//	rogue — try to reserve a relay slot; print RESERVE DENIED/ALLOWED.
//
// Identity is a base64 libp2p private key passed with -key, so the
// parent controls Peer IDs (and therefore the relay allowlist).
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"

	"distributed-ai/node/internal/p2p"
	"distributed-ai/node/internal/protocol"
)

func fatal(args ...any) {
	fmt.Println(append([]any{"SMOKE: FATAL"}, args...)...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatal("mode required: svc|node|rogue")
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	keyB64 := fs.String("key", "", "base64 libp2p private key")
	relayAddr := fs.String("relay", "", "relay multiaddr (…/p2p/<relayID>)")
	sendTo := fs.String("sendto", "", "circuit multiaddr to send ResourceInfo to")
	reply := fs.Bool("reply", false, "svc: reply to the sender with our info via the relay")
	name := fs.String("name", mode, "node name to advertise")
	stay := fs.Duration("stay", 25*time.Second, "how long to stay alive (svc/node)")
	_ = fs.Parse(os.Args[2:])

	keyBytes, err := base64.StdEncoding.DecodeString(*keyB64)
	if err != nil {
		fatal("key decode:", err)
	}
	priv, err := crypto.UnmarshalPrivateKey(keyBytes)
	if err != nil {
		fatal("key unmarshal:", err)
	}
	myID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		fatal(err)
	}
	info := protocol.ResourceInfo{
		NodeName: *name, PeerID: myID.String(), OS: "smoke", Architecture: "test",
		CPUModel: "smoke", PhysicalCores: 1, LogicalCores: 1, RAMTotalGB: 1,
		CPUShare: 10, GPUShare: 0, RAMShare: 10, StorageShare: 10,
	}
	ctx := context.Background()

	if mode == "rogue" {
		h, err := p2p.NewWithAddrs(priv, "/ip4/127.0.0.1/tcp/0")
		if err != nil {
			fatal(err)
		}
		defer h.Close()
		relayInfo, err := peer.AddrInfoFromP2pAddr(mustMA(*relayAddr))
		if err != nil {
			fatal(err)
		}
		if err := h.Connect(ctx, *relayInfo); err != nil {
			fatal("connect relay:", err)
		}
		if _, err := client.Reserve(ctx, h, *relayInfo); err != nil {
			fmt.Println("SMOKE: RESERVE DENIED:", err)
			return
		}
		fmt.Println("SMOKE: RESERVE ALLOWED (unexpected for a rogue peer)")
		return
	}
	if mode != "svc" && mode != "node" {
		fatal("unknown mode", mode)
	}

	var h host.Host
	h, err = p2p.NewWithConfig(priv, p2p.Config{
		RelayAddr:           *relayAddr,
		ForcePrivate:        true,
		DisableHolePunching: true,
	}, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		fatal(err)
	}
	defer h.Close()

	// Receive path with parent-readable GOT lines (+ optional reply).
	h.SetStreamHandler(protocol.ResourceProtocolV2,
		p2p.NewResourceStreamV2Handler(func(got protocol.ResourceInfo, from peer.ID) {
			fmt.Printf("SMOKE: GOT from=%s name=%s\n", from, got.NodeName)
			if *reply {
				senderCircuit := *relayAddr + "/p2p-circuit/p2p/" + from.String()
				go func() {
					res, err := p2p.ConnectWithRetry(ctx, h, senderCircuit, info, 5*time.Second, nil)
					if err != nil {
						fmt.Println("SMOKE: REPLY FAILED:", err)
						return
					}
					fmt.Printf("SMOKE: REPLIED acked=%v kind=%s\n", res.ACKed, res.Kind)
				}()
			}
		}))

	fmt.Println("SMOKE: READY", myID.String())
	if *sendTo != "" {
		res, err := p2p.ConnectWithRetry(ctx, h, *sendTo, info, 5*time.Second,
			func(f string, a ...any) { fmt.Printf("SMOKE: "+f+"\n", a...) })
		if err != nil {
			fatal("send:", err)
		}
		fmt.Printf("SMOKE: SENT acked=%v fallback=%v kind=%s\n", res.ACKed, res.FellBack, res.Kind)
	}
	time.Sleep(*stay)
	fmt.Println("SMOKE: DONE")
}

func mustMA(s string) ma.Multiaddr {
	a, err := ma.NewMultiaddr(s)
	if err != nil {
		fatal(err)
	}
	return a
}
