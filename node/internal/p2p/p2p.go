// Package p2p owns the libp2p host: creation, the resource stream handler,
// and dialing a peer to send our ResourceInfo.
//
// Behavior preserved from the original runtime: listen addresses,
// protocol usage, and the human-readable receive printout.
package p2p

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	"distributed-ai/node/internal/protocol"
)

// New creates the libp2p host with the node's identity.
func New(priv crypto.PrivKey) (host.Host, error) {
	return NewWithAddrs(priv,
		"/ip4/0.0.0.0/tcp/4001",
		"/ip4/0.0.0.0/udp/4001/quic-v1",
	)
}

// NewWithPort is New on a custom p2p port (lets two nodes share one box;
// production default stays 4001).
func NewWithPort(priv crypto.PrivKey, port int) (host.Host, error) {
	return NewWithAddrs(priv,
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
	)
}

// NewWithAddrs is New with explicit listen addresses (used by tests to grab
// ephemeral ports; production always uses New's fixed addresses).
func NewWithAddrs(priv crypto.PrivKey, addrs ...string) (host.Host, error) {
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(addrs...),
	)
	if err != nil {
		return nil, err
	}
	h.SetStreamHandler(protocol.ResourceProtocol, HandleResourceStream)
	return h, nil
}

// HandleResourceStream receives a peer's ResourceInfo and prints it.
// Same output format as the original runtime.
func HandleResourceStream(s network.Stream) {
	defer s.Close()

	var info protocol.ResourceInfo
	if err := json.NewDecoder(s).Decode(&info); err != nil {
		fmt.Println("Failed to read resource info:", err)
		return
	}

	fmt.Println()
	fmt.Println("================================")
	fmt.Println(" Resource info received")
	fmt.Println("================================")
	fmt.Println("Node:", info.NodeName)
	fmt.Println("Peer ID:", info.PeerID)
	fmt.Println("OS:", info.OS)
	fmt.Println("Architecture:", info.Architecture)
	fmt.Println("CPU:", info.CPUModel)
	fmt.Println("Physical cores:", info.PhysicalCores)
	fmt.Println("Logical cores:", info.LogicalCores)
	fmt.Printf("RAM: %.1f GB\n", info.RAMTotalGB)
	fmt.Println("GPU:", info.GPUModel)
	if info.GPUVendor != "" {
		fmt.Println("GPU vendor:", info.GPUVendor)
	}
	if info.GPUCores > 0 {
		fmt.Println("GPU cores:", info.GPUCores)
	}
	if info.GPUDriver != "" {
		fmt.Println("GPU driver:", info.GPUDriver)
	}
	if info.MetalSupport != "" {
		fmt.Println("Metal:", info.MetalSupport)
	}
	fmt.Printf("Storage total: %.1f GB\n", info.StorageTotalGB)
	fmt.Printf("Storage used: %.1f GB\n", info.StorageUsedGB)
	fmt.Printf("Storage free: %.1f GB\n", info.StorageFreeGB)
	fmt.Println()
	fmt.Printf("CPU share: %d%%\n", info.CPUShare)
	fmt.Printf("GPU share: %d%%\n", info.GPUShare)
	fmt.Printf("RAM share: %d%%\n", info.RAMShare)
	fmt.Printf("Storage share: %d%%\n", info.StorageShare)
	fmt.Println("================================")
}

// ConnectAndSend dials the peer at addrStr and sends info over the resource
// protocol. Mirrors the original main() connect+send flow.
func ConnectAndSend(ctx context.Context, h host.Host, addrStr string, info protocol.ResourceInfo) error {
	addr, err := ma.NewMultiaddr(addrStr)
	if err != nil {
		return fmt.Errorf("bad multiaddr: %w", err)
	}
	pi, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return fmt.Errorf("bad p2p addr: %w", err)
	}
	if err := h.Connect(ctx, *pi); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}
	stream, err := h.NewStream(ctx, pi.ID, protocol.ResourceProtocol)
	if err != nil {
		return fmt.Errorf("open stream failed: %w", err)
	}
	defer stream.Close()
	if err := json.NewEncoder(stream).Encode(info); err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	return nil
}
