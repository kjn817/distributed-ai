#!/bin/bash
# Build llama.cpp engine for macOS (Apple Silicon + Metal).
# Run this ON YOUR MAC, not on the Oracle VM.
#
# Produces:
#   build/bin/llama-server      - inference server (for coordinator mode)
#   build/bin/rpc-server        - RPC worker (for joint compute; binary name
#                                 varies by llama.cpp version, see below)
#
# Prerequisites: Xcode Command Line Tools, cmake
#   xcode-select --install
#   brew install cmake
#
# Usage:
#   bash build-engine-macos.sh
#
# The script pins the same llama.cpp commit as CI for consistency.

set -euo pipefail

LLAMA_COMMIT="10b8b7553494f3638da7668cf17cac089229122a"
WORK_DIR="$HOME/distributed-ai-engine"
BUILD_DIR="$WORK_DIR/build"

echo "=== Distributed AI: macOS engine build ==="
echo "Target: Apple Silicon (arm64) + Metal"
echo "llama.cpp commit: $LLAMA_COMMIT"
echo ""

# Check prerequisites
if ! command -v cmake &> /dev/null; then
    echo "ERROR: cmake not found. Install with: brew install cmake"
    exit 1
fi

if ! command -v git &> /dev/null; then
    echo "ERROR: git not found."
    exit 1
fi

# Check Apple Silicon
ARCH=$(uname -m)
if [ "$ARCH" != "arm64" ]; then
    echo "WARNING: This script targets Apple Silicon (arm64). Detected: $ARCH"
    echo "Intel Macs can still build but without Metal GPU acceleration."
    read -p "Continue? (y/N) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

# Clone or update llama.cpp
if [ -d "$WORK_DIR/.git" ]; then
    echo "Updating existing llama.cpp..."
    cd "$WORK_DIR"
    git fetch origin
else
    echo "Cloning llama.cpp..."
    git clone https://github.com/ggerganov/llama.cpp.git "$WORK_DIR"
    cd "$WORK_DIR"
fi

git checkout "$LLAMA_COMMIT"
echo "Checked out: $(git rev-parse --short HEAD)"
echo ""

# Configure with RPC support and Metal
echo "Configuring (Metal + RPC)..."
cmake -B "$BUILD_DIR" \
    -DGGML_METAL=ON \
    -DGGML_RPC=ON \
    -DLLAMA_BUILD_TESTS=OFF \
    -DCMAKE_BUILD_TYPE=Release

# Build
echo ""
echo "Building (this takes 5-15 minutes)..."
NPROC=$(sysctl -n hw.ncpu)
cmake --build "$BUILD_DIR" --config Release -j "$NPROC"

echo ""
echo "=== Build complete ==="
echo ""
echo "Binaries:"
ls -lh "$BUILD_DIR/bin/llama-server" 2>/dev/null || echo "  llama-server: NOT FOUND"
# RPC server binary name varies: ggml-rpc-server (older) or rpc-server (newer)
for name in ggml-rpc-server rpc-server; do
    if [ -f "$BUILD_DIR/bin/$name" ]; then
        echo "  RPC server: $BUILD_DIR/bin/$name"
        ls -lh "$BUILD_DIR/bin/$name"
    fi
done
echo ""
echo "Next steps:"
echo "  1. Note the RPC server binary name above"
echo "  2. Set RPC_BIN env when running dain-node:"
echo "     export RPC_BIN=$BUILD_DIR/bin/<rpc-server-name>"
echo "  3. For Metal GPU: no extra flags needed (auto-detected)"
echo ""
echo "Verify Metal is available:"
echo "  $BUILD_DIR/bin/llama-server --help | grep -i metal"
