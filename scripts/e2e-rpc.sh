#!/bin/bash
# Phase 2 RPC e2e: RPC protocol and placement verification.
# *** CORRECTION (2026-10-19) ***: This test was previously labeled
# "jointly compute ONE request" but the coordinator ran WITHOUT -ngl,
# so all layers computed on local CPU. This verifies RPC protocol,
# worker registration, placement, and credit flow — NOT joint compute.
# Status: 공동 계산 미검증 (프로토콜·배관만 검증).
# True joint-compute verification requires the -ngl fix (executor.go)
# and is covered by docs/phase-b-proof.md.
#
# *** LOCAL INTEGRATION TEST *** Both workers run on the SAME machine
# (127.0.0.1). NOT network traversal. Remote multi-machine verification
# is a separate milestone (requires 2+ physical hosts).
# Prerequisites: service with TEST_FUND, two worker nodes with --rpc-port.
set -e
API_KEY="testkey123456789"
SVC="http://127.0.0.1:18081"

echo "=== 1. Check workers ==="
curl -s $SVC/network/status | python3 -c "
import json,sys
d = json.load(sys.stdin)
workers = [n for n in d['nodes'] if n.get('rpc_addr')]
print(f'RPC workers: {len(workers)}')
for w in workers:
    print(' ', w['id'][:12], w['rpc_addr'])
assert len(workers) >= 2, 'need 2+ workers'
"

echo "=== 2. Distributed inference (joint compute) ==="
time curl -s -X POST $SVC/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5-0.5b-q4","messages":[{"role":"user","content":"Say hello in one word."}],"max_tokens":10,"distributed":true}' \
  --max-time 240 | python3 -c "
import json,sys
d = json.load(sys.stdin)
if 'error' in d:
    print('FAILED:', d['error']['message'][:200])
    sys.exit(1)
print('SUCCESS:', d['choices'][0]['message']['content'][:100])
"

echo "=== 3. Request routing (single node, for comparison) ==="
time curl -s -X POST $SVC/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5-0.5b-q4","messages":[{"role":"user","content":"Say hello in one word."}],"max_tokens":10}' \
  --max-time 240 | python3 -c "
import json,sys
d = json.load(sys.stdin)
if 'error' in d:
    print('FAILED:', d['error']['message'][:200])
    sys.exit(1)
print('SUCCESS:', d['choices'][0]['message']['content'][:100])
"

echo "=== Phase 2 e2e PASS (local integration, same machine) ==="
