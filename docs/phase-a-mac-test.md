# Phase A: Mac 두 번째 Node 연결 테스트

## 목표

Oracle과 Mac이 실제로 연결되어 하나의 네트워크로 동작하는지 검증.
HTTP 등록만으로는 부족 — **libp2p P2P 연결과 자원 정보 교환**까지 확인해야 성공.

## 전제 조건

- Oracle: v0.10-alpha 실행 중 (dain-svc + dain-node) — 변경 없음
- Mac: M3 Apple Silicon, **v0.10-alpha dain-node-darwin-arm64** 바이너리
  (릴리스에서 다운로드, 새 빌드 불필요 — Phase A에 필요한 플래그 전부 포함)
- Mac에서 Oracle SSH **22번 포트** 접속 가능 (터널용, 방화벽에서 허용 필요)
- Mac에 llama.cpp 엔진 빌드 **불필요** (Phase A는 등록+P2P만)

## 0단계: 바이너리 다운로드 (Mac에서)

```bash
# v0.10-alpha 릴리스에서 다운로드 (새 빌드 불필요 — Phase A에 필요한 플래그 전부 포함)
# 파일 크기: 약 34MB (2026-10-19 다운로드 확인됨)
curl -sSL -o dain-node https://github.com/kjn817/distributed-ai/releases/download/v0.10-alpha/dain-node-darwin-arm64
chmod +x dain-node
```

## 1단계: SSH 터널 설정 (Mac에서)

```bash
# API 터널 (HTTP 등록용)
ssh -L 8080:127.0.0.1:8080 ubuntu@<oracle-ip> -N -f

# P2P 터널 (libp2p 연결용) — 4001 포트를 열지 않고 터널로 검증
# 주의: Mac 로컬 4001은 터널이 사용하므로, Mac Node는 4002로 리슨
# 터널용 로컬 포트는 14001로 분리 (Mac Node 기본 포트와 충돌 방지)
ssh -L 14001:127.0.0.1:4001 ubuntu@<oracle-ip> -N -f

# 터널 확인
lsof -i :8080 -i :14001 | grep LISTEN
```

**왜 터널인가:** Oracle 방화벽에 4001을 열지 않고도 P2P를 검증할 수 있음.
인터넷에 libp2p 포트를 노출하지 않음.

## 2단계: Oracle Peer ID 확인 (Oracle에서)

```bash
# Node 로그에서 Peer ID 확인
journalctl --user -u distributed-ai-alpha-node.service | grep -i "peer" | head -5
```

또는 API에서:
```bash
curl http://127.0.0.1:8080/network/status | python3 -c \
  "import json,sys; d=json.load(sys.stdin); print(d['nodes'][0]['peer_id'])"
```

## 3단계: Mac Node 실행 (안전 모드)

```bash
# 안전 모드: --cpu 0 --ram 0 --storage 0
# → ComputeNodes에서 제외되어 연산 작업이 할당되지 않음 (v0.10-alpha 확인)
# → 등록·하트비트·P2P 통신만 수행
# → macOS는 OS 수준 자원 제한이 불가능하므로 강제되는 것처럼 표시하지 않음
# → 실제 자원 제공은 연결 성공 후 별도 승인 필요

./dain-node \
  --api http://127.0.0.1:8080 \
  --cpu 0 --ram 0 --storage 0 \
  --p2p-port 4002 \
  --connect /ip4/127.0.0.1/tcp/14001/p2p/<oracle-peer-id>
```

**플래그 설명:**
- `--cpu 0 --ram 0 --storage 0`: 자원 미제공 (안전 모드). 스케줄러가 이 노드에 추론 작업을 보내지 않음.
  v0.10-alpha의 `ComputeNodes()`가 `CPUCores <= 0`을 제외함을 확인.
- `--p2p-port 4002`: Mac Node 리슨 포트 (기본 4001과 분리).
- `--connect /ip4/127.0.0.1/tcp/14001/...`: SSH 터널(로컬 14001 → Oracle 4001)을 통한 Oracle libp2p 연결.

**포트 정리:**
| 용도 | 포트 | 설명 |
|------|------|------|
| Mac SSH 터널 (P2P) | 14001 | 로컬 → Oracle 4001 |
| Mac Node P2P 리슨 | 4002 | Mac 자체 libp2p |
| Oracle P2P 리슨 | 4001 | Oracle 자체 libp2p (터널 목적지) |
| Mac SSH 터널 (API) | 8080 | 로컬 → Oracle 8080 |

## 4단계: 성공 기준 확인

### 4-1. HTTP 등록 (기본)

```bash
# Oracle에서 또는 Mac 터널 경유
curl http://127.0.0.1:8080/network/status | python3 -m json.tool
```

**확인:** `nodes` 배열에 2개 노드, 각각 다른 `id`, `os`가 `linux`와 `darwin`으로 구분됨.

### 4-2. P2P 연결 (필수)

**Mac 로그에서 확인:**
```
Connecting to: /ip4/127.0.0.1/tcp/14001/p2p/<peer-id>
# 성공 시 에러 없이 다음 단계로 진행 (14001 = SSH 터널 로컬 포트)
```

**Oracle 로그에서 확인 (핵심 증거):**
```
================================
 Resource info received
================================
Node: <mac-hostname>
Peer ID: <mac-peer-id>
OS: darwin
Architecture: arm64
CPU: Apple M3
...
```

이 로그가 있어야 **실제 P2P 성공**으로 인정. HTTP 등록만으로는 불충분.

```bash
# Oracle에서 확인
journalctl --user -u distributed-ai-alpha-node.service | grep -A 15 "Resource info received"
```

### 4-3. 자원 정보 정직성 (macOS)

```bash
curl http://127.0.0.1:8080/network/status | python3 -c "
import json,sys
d = json.load(sys.stdin)
for n in d['nodes']:
    if n['os'] == 'darwin':
        print('Mac node:')
        print('  cpu_cores:', n['cpu_cores'], '(0이어야 함 - 연산 미제공)')
        print('  limits_enforced:', n['limits_enforced'], '(false이어야 함)')
        print('  limits_reason:', n.get('limits_reason', ''))
"
```

**확인:**
- `cpu_cores: 0` — 연산 작업 할당 안 됨
- `limits_enforced: false` — macOS는 강제 불가이므로 false가 정상
- 거짓으로 `true`가 나오면 버그

### 4-4. 모델 청크 P2P 전송 (선택, 시간이 있으면)

Mac에서 모델 복제를 트리거하면 libp2p로 청크가 전송됨:
```bash
# Mac Node가 모델을 요청하도록 (API를 통해 replicate 지시)
# 또는 수동으로 청크 요청
```

Oracle 로그에서 `ChunkProtocol` 스트림 처리 확인.

## 성공 판정

| 항목 | 기준 | 증거 |
|------|------|------|
| HTTP 등록 | Node 2개 표시 | `/network/status` |
| P2P 연결 | libp2p 핸드셰이크 성공 | Mac 로그 "Connecting to" 에러 없음 |
| 자원 교환 | ResourceInfo 수신 | Oracle 로그 "Resource info received" + Mac 정보 |
| 안전 모드 | 연산 미할당 | `cpu_cores: 0`, 스케줄러 제외 |
| 정직 보고 | limits false | `limits_enforced: false` + reason |

**5개 모두 충족해야 Phase A 성공.**

## 문제 해결

### P2P 연결 실패
```bash
# 터널 확인 (Phase A 1단계와 동일한 14001 터널)
ssh -v -L 14001:127.0.0.1:4001 ubuntu@<oracle-ip> -N
# Oracle에서 4001 리스닝 확인
ss -tlnp | grep 4001
```

### Peer ID를 모르겠으면
Oracle Node 재시작 시 로그 첫 부분에 Peer ID 출력됨.

### Mac Node가 연산 작업을 받으면
`--cpu 0` 확인. `ComputeNodes()`는 `CPUCores <= 0`인 노드를 제외함.
