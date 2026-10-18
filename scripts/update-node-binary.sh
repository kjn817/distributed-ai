#!/bin/bash
# Safe dain-node binary update for Oracle.
#
# IMPORTANT:
# - This is NOT zero-downtime. Stopping dain-node pauses inference
#   (the node owns llama-server). Expect a brief inference outage.
# - The API server (dain-svc) is NEVER touched. The credit ledger is
#   in-memory — restarting dain-svc would LOSE all balances and history.
# - Only dain-node is stopped/updated/restarted.
# - Model files, config, and registration are preserved.
#
# Safety features:
#   1. Backup current binary before replacement
#   2. SHA256 verification of downloaded binary
#   3. Rollback to backup on failure
#
# Usage (on Oracle):
#   bash update-node-binary.sh <version-tag>
# Example:
#   bash update-node-binary.sh v0.11-alpha
#
# Or to update from a local file:
#   bash update-node-binary.sh --file /path/to/dain-node

set -euo pipefail

INSTALL_DIR="$HOME/distributed-ai-alpha"
BIN_DIR="$INSTALL_DIR/bin"
NODE_BIN="$BIN_DIR/dain-node"
BACKUP_DIR="$INSTALL_DIR/backups"
SERVICE_NAME="distributed-ai-alpha-node.service"
GITHUB_REPO="kjn817/distributed-ai"

if [ $# -lt 1 ]; then
    echo "Usage: $0 <version-tag> | --file <path>"
    echo "Example: $0 v0.11-alpha"
    exit 1
fi

mkdir -p "$BACKUP_DIR"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
BACKUP_BIN="$BACKUP_DIR/dain-node.backup-$TIMESTAMP"

echo "=== Distributed AI: Safe Node Binary Update ==="
echo "WARNING: This will briefly pause inference (node owns llama-server)."
echo "         The API server and credit ledger are NOT touched."
echo ""

# Step 1: Backup current binary
if [ ! -f "$NODE_BIN" ]; then
    echo "ERROR: Current binary not found: $NODE_BIN"
    exit 1
fi
echo "[1/6] Backing up current binary..."
cp "$NODE_BIN" "$BACKUP_BIN"
BACKUP_SHA=$(sha256sum "$BACKUP_BIN" | awk '{print $1}')
echo "      Backup: $BACKUP_BIN"
echo "      SHA: $BACKUP_SHA"

# Step 2: Obtain new binary
NEW_BIN="/tmp/dain-node.new"
if [ "$1" == "--file" ]; then
    if [ $# -lt 2 ] || [ ! -f "$2" ]; then
        echo "ERROR: File not found: ${2:-}"
        exit 1
    fi
    echo "[2/6] Using local file: $2"
    cp "$2" "$NEW_BIN"
    chmod +x "$NEW_BIN"
else
    VERSION="$1"
    echo "[2/6] Downloading dain-node $VERSION..."
    URL="https://github.com/$GITHUB_REPO/releases/download/$VERSION/dain-node-linux-amd64"
    if ! curl -sSL -o "$NEW_BIN" "$URL"; then
        echo "ERROR: Download failed: $URL"
        exit 1
    fi
    chmod +x "$NEW_BIN"
    # Verify SHA256 against release SHA256SUMS.txt
    echo "      Verifying SHA256..."
    SUMS_URL="https://github.com/$GITHUB_REPO/releases/download/$VERSION/SHA256SUMS.txt"
    EXPECTED=$(curl -sSL "$SUMS_URL" | awk '$2 == "dain-node-linux-amd64" {print $1}')
    if [ -z "$EXPECTED" ]; then
        echo "ERROR: Could not find SHA for dain-node-linux-amd64 in release"
        rm -f "$NEW_BIN"
        exit 1
    fi
    ACTUAL=$(sha256sum "$NEW_BIN" | awk '{print $1}')
    if [ "$EXPECTED" != "$ACTUAL" ]; then
        echo "ERROR: SHA256 mismatch!"
        echo "  Expected: $EXPECTED"
        echo "  Actual:   $ACTUAL"
        rm -f "$NEW_BIN"
        exit 1
    fi
    echo "      SHA256 verified: $ACTUAL"
fi

# Step 3: Pre-flight check (binary runs)
echo "[3/6] Pre-flight check..."
if ! "$NEW_BIN" --help > /dev/null 2>&1; then
    echo "ERROR: New binary failed --help check"
    rm -f "$NEW_BIN"
    exit 1
fi
echo "      New binary responds to --help"

# Step 4: Stop node service (NOT the API server)
echo "[4/6] Stopping node service (inference will pause)..."
echo "      NOTE: dain-svc (API + credit ledger) keeps running."
systemctl --user stop "$SERVICE_NAME"
sleep 3

# Verify API still running (credit ledger safety)
if ! systemctl --user is-active --quiet distributed-ai-alpha.service; then
    echo "WARNING: API server is not running! Aborting to protect credit ledger."
    echo "         Restart the node with the BACKUP binary manually:"
    echo "         cp $BACKUP_BIN $NODE_BIN && systemctl --user start $SERVICE_NAME"
    exit 1
fi
echo "      API server still running (credit ledger safe)"

# Step 5: Replace binary
echo "[5/6] Replacing binary..."
cp "$NEW_BIN" "$NODE_BIN"
chmod +x "$NODE_BIN"
NEW_SHA=$(sha256sum "$NODE_BIN" | awk '{print $1}')
echo "      Installed SHA: $NEW_SHA"
rm -f "$NEW_BIN"

# Step 6: Restart and verify
echo "[6/6] Restarting node service..."
systemctl --user start "$SERVICE_NAME"
sleep 5

if systemctl --user is-active --quiet "$SERVICE_NAME"; then
    echo ""
    echo "=== Update successful ==="
    echo "Backup kept at: $BACKUP_BIN"
    echo ""
    echo "Verify:"
    echo "  journalctl --user -u $SERVICE_NAME -n 20"
    echo "  curl http://127.0.0.1:8080/network/status | python3 -m json.tool"
else
    echo ""
    echo "=== Update FAILED — rolling back ==="
    systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
    cp "$BACKUP_BIN" "$NODE_BIN"
    chmod +x "$NODE_BIN"
    systemctl --user start "$SERVICE_NAME"
    sleep 3
    if systemctl --user is-active --quiet "$SERVICE_NAME"; then
        echo "Rollback successful. Old binary restored."
    else
        echo "ERROR: Rollback also failed! Manual intervention required."
        echo "Backup binary: $BACKUP_BIN"
    fi
    exit 1
fi
