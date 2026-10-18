#!/bin/bash
# M1 end-to-end verification: service -> llama-server -> real tokens.
# Usage: ./e2e-m1.sh
# Requires: engines/llama.cpp/build/bin/llama-server and models/qwen2.5-0.5b-instruct-q4_k_m.gguf
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/engines/llama.cpp/build/bin/llama-server"
MODEL="$ROOT/models/qwen2.5-0.5b-instruct-q4_k_m.gguf"
[ -x "$BIN" ]   || { echo "MISSING engine binary: $BIN"; exit 1; }
[ -f "$MODEL" ] || { echo "MISSING model: $MODEL"; exit 1; }

export PATH="$HOME/workspace/go/bin:$PATH"
cd "$ROOT/service" || exit 1

ENGINE_BIN="$BIN" MODEL_PATH="$MODEL" ENGINE_PORT=18080 LISTEN_ADDR=":18081" \
  go run ./cmd/server > /tmp/dain-e2e.log 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null' EXIT

echo "waiting for service..."
for i in $(seq 1 60); do
  curl -sf -o /dev/null http://127.0.0.1:18081/v1/models && break
  sleep 2
done
curl -sf -o /dev/null http://127.0.0.1:18081/v1/models || { echo "service did not start"; exit 1; }

echo "sending chat completion..."
START=$(date +%s)
RESP=$(curl -s http://127.0.0.1:18081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-0.5b-q4","messages":[{"role":"user","content":"Say hello in one short sentence."}],"max_tokens":32}')
END=$(date +%s)
echo "$RESP" | head -c 600; echo

CONTENT=$(echo "$RESP" | grep -o '"content":"[^"]*"' | head -1)
if [ -z "$CONTENT" ]; then
  echo "E2E FAIL: no content in response"
  exit 1
fi
echo "E2E PASS in $((END-START))s: $CONTENT"
