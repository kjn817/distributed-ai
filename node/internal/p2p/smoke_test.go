package p2p

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Process-level Phase A smoke test: the REAL binaries as separate OS
// processes — dain-relay (allowlist enforced), a svc peer, a node
// peer, a rogue peer — over loopback. This is the deployment shape,
// and it exists because stream behavior is only meaningful there.
//
// Skipped in -short mode and when no Go toolchain is on PATH.
func TestRelayProcessSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("process smoke test skipped in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	relayBin := filepath.Join(binDir, "dain-relay")
	smokeBin := filepath.Join(binDir, "relay-smoke")
	for _, build := range [][2]string{
		{relayBin, "./node/cmd/relay"},
		{smokeBin, "./node/cmd/relay-smoke"},
	} {
		cmd := exec.Command(goBin, "build", "-o", build[0], build[1])
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", build[1], err, out)
		}
	}

	genKey := func() (string, peer.ID) {
		priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := crypto.MarshalPrivateKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		id, err := peer.IDFromPrivateKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(raw), id
	}
	svcKey, svcID := genKey()
	nodeKey, nodeID := genKey()
	rogueKey, rogueID := genKey()
	_ = rogueID

	// A child keeps every output line. Waits consume from a cursor:
	// a fast peer can print a later assertion's line while an earlier
	// wait is still scanning (the svc reply can reach the node before
	// the node's own SENT line is consumed), so lines must never be
	// discarded just because they arrived early.
	type child struct {
		cmd  *exec.Cmd
		mu   sync.Mutex
		seen []string
		next int
		done bool
	}
	start := func(bin string, args ...string) *child {
		cmd := exec.Command(bin, args...)
		cmd.Dir = root
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		c := &child{cmd: cmd}
		go func() {
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 1024*1024), 1024*1024)
			for sc.Scan() {
				c.mu.Lock()
				c.seen = append(c.seen, sc.Text())
				c.mu.Unlock()
			}
			c.mu.Lock()
			c.done = true
			c.mu.Unlock()
		}()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return c
	}
	waitLine := func(c *child, substr string, timeout time.Duration) string {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			c.mu.Lock()
			for c.next < len(c.seen) {
				line := c.seen[c.next]
				c.next++
				t.Logf("child: %s", line)
				if strings.Contains(line, substr) {
					c.mu.Unlock()
					return line
				}
			}
			done := c.done
			c.mu.Unlock()
			if done {
				t.Fatalf("child exited while waiting for %q", substr)
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q", substr)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	relayDir := t.TempDir()
	relayPort := freePort(t)
	startRelay := func() (*child, string) {
		c := start(relayBin, "-port", fmt.Sprint(relayPort), "-data-dir", relayDir,
			"-allow", svcID.String()+","+nodeID.String())
		line := waitLine(c, "Peer ID:", 20*time.Second)
		relayID := strings.TrimSpace(strings.TrimPrefix(line, "Peer ID:"))
		// Dial the relay by loopback explicitly. The relay process may
		// advertise the sandbox host's public-form address first; this
		// smoke test is a local topology, and the loopback form is the
		// deterministic local address for the same listener.
		return c, fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", relayPort, relayID)
	}

	// 1. Relay up with the member allowlist.
	relayChild, relayAddr := startRelay()
	t.Logf("relay: %s", relayAddr)

	// 2. svc peer up (reserves via AutoRelay, replies to senders).
	svc := start(smokeBin, "svc", "-key", svcKey, "-relay", relayAddr, "-reply",
		"-name", "svc", "-stay", "4m")
	waitLine(svc, "SMOKE: READY", 20*time.Second)

	// 3. node sends ResourceInfo to svc THROUGH the relay.
	svcCircuit := relayAddr + "/p2p-circuit/p2p/" + svcID.String()
	node := start(smokeBin, "node", "-key", nodeKey, "-relay", relayAddr,
		"-sendto", svcCircuit, "-name", "node-a", "-stay", "90s")
	waitLine(node, "SMOKE: READY", 20*time.Second)
	sent := waitLine(node, "SMOKE: SENT", 120*time.Second)
	if !strings.Contains(sent, "acked=true") || !strings.Contains(sent, "kind=relayed") {
		t.Fatalf("node send line = %q, want acked=true kind=relayed", sent)
	}
	got := waitLine(svc, "SMOKE: GOT", 30*time.Second)
	if !strings.Contains(got, "from="+nodeID.String()) {
		t.Fatalf("svc GOT line = %q, want from=%s", got, nodeID)
	}
	t.Log("node -> relay -> svc: ResourceInfo delivered, V2 ACK returned, kind=relayed")

	// 4. svc replies over the same relay (svc -> node direction).
	waitLine(svc, "SMOKE: REPLIED", 60*time.Second)
	back := waitLine(node, "SMOKE: GOT", 30*time.Second)
	if !strings.Contains(back, "from="+svcID.String()) {
		t.Fatalf("node GOT line = %q, want from=%s", back, svcID)
	}
	t.Log("svc -> relay -> node: reply delivered with ACK")

	// 5. Rogue peer is refused a reservation.
	rogueCmd := exec.Command(smokeBin, "rogue", "-key", rogueKey, "-relay", relayAddr)
	rogueCmd.Dir = root
	out, err := rogueCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rogue run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "RESERVE DENIED") {
		t.Fatalf("rogue output = %q, want RESERVE DENIED", out)
	}
	t.Log("rogue reservation denied by the allowlist relay")

	// 6. Relay restart (same identity dir + port): a fresh node
	// process must get its info through again — the peers re-reserve
	// and the --connect retry loop rides out the gap.
	_ = relayChild.cmd.Process.Kill()
	_, relayAddr2 := startRelay()
	if relayAddr2 != relayAddr {
		t.Fatalf("relay address changed across restart: %q vs %q", relayAddr2, relayAddr)
	}
	node2 := start(smokeBin, "node", "-key", nodeKey, "-relay", relayAddr,
		"-sendto", svcCircuit, "-name", "node-a2", "-stay", "30s")
	sent2 := waitLine(node2, "SMOKE: SENT", 150*time.Second)
	if !strings.Contains(sent2, "acked=true") {
		t.Fatalf("post-restart send line = %q, want acked=true", sent2)
	}
	t.Log("relay restart: peers recovered, ResourceInfo delivered with ACK again")

	_ = context.Background
	_ = os.Getenv
}
