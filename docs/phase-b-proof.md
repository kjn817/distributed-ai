# Phase B: 공동 추론 증명 프로토콜

## 목표

Oracle(coordinator) + Mac(RPC worker)이 **하나의 추론 요청을 실제로 공동 계산**했음을
로그와 수치로 증명한다.

**인정되지 않는 것** (사용자 기준):
- 요청 분산(SingleNodeFirst)으로 다른 요청을 나눈 것은 공동 추론이 아님
- 모델 복제·등록만으로 "공동 추론 성공" 보고 금지
- 텍스트가 반환됐다는 것만으로는 증명 불가 — 양쪽 로그 + 사용량(트래픽·속도 특성) 필요

**성공 판정**: 아래 증거 1+2가 모두 PASS일 때만 "공동 연산 성공"으로 보고한다.
증거 3·4는 보조 증거다.

## 사전 조건

- Phase A 완료 (Mac 노드 등록·P2P 연결 확인 — `docs/phase-a-mac-test.md`)
- Mac: `scripts/build-engine-macos.sh`로 빌드한 rpc-server 바이너리
  (`build/bin/rpc-server` 또는 `build/bin/ggml-rpc-server` — 빌드 스크립트가 출력한 이름 사용)
- Mac: `dain-node-darwin-arm64` 바이너리
- Mac에 모델 파일 존재: `~/.distributed-ai-node/models/qwen2.5-0.5b-q4*`
  (Phase A 중 P2P replica로 수신됐거나 수동 복사 — 없으면 `--rpc-models` 검증에서 실패함)
- Oracle: v0.10-alpha 실행 중 (dain-svc + dain-node), 모델 `qwen2.5-0.5b-q4` 로드됨
- Oracle API 키 (요청 인증용 — TEST_FUND 발급 시 사용한 키)

## 절차

### 1단계: SSH 터널 3개 (Mac에서)

```bash
ssh -L 8080:127.0.0.1:8080 ubuntu@<oracle-ip> -N -f   # API (HTTP 등록·요청용)
ssh -L 4001:127.0.0.1:4001 ubuntu@<oracle-ip> -N -f   # P2P (Phase A와 동일)
ssh -R 50052:127.0.0.1:50052 ubuntu@<oracle-ip> -N -f # RPC 역터널 (핵심)

# 역터널 확인 (Oracle에서): 127.0.0.1:50052가 LISTEN이어야 함
ssh ubuntu@<oracle-ip> "ss -ltn | grep 50052"
```

**왜 역터널인가**: llama.cpp RPC에는 인증이 없어 인터넷 직접 노출 금지.
역터널이면 Oracle의 `127.0.0.1:50052`로 들어오는 것만 Mac에 전달되고,
외부에서는 접근 불가. sshd 기본 설정(GatewayPorts=no)이면 원격 바인딩이
localhost로만 되므로 Oracle 측 설정 변경 불필요.

### 2단계: Mac 노드 실행 (RPC standalone 모드, 로그 저장)

```bash
# Mac에서
export RPC_BIN=$HOME/<llama.cpp-build>/bin/rpc-server   # 실제 바이너리 이름으로 교체
./dain-node-darwin-arm64 \
  --api http://127.0.0.1:8080 \
  --cpu 0 --ram 0 --storage 10 \
  --p2p-port 4002 \
  --connect /ip4/127.0.0.1/tcp/4001/p2p/<oracle-peer-id> \
  --rpc-standalone --rpc-port 50052 --rpc-bind 127.0.0.1 \
  --rpc-models qwen2.5-0.5b-q4 \
  > mac-phase-b.log 2>&1 &
echo $! > mac-node.pid
```

플래그 설명:
- `--cpu 0 --ram 0`: 안전 모드 유지 — 스케줄러의 연산 노드에서 제외.
  RPC worker 선정은 `RpcAddr` 보유 여부로 판단하므로 `--cpu 0`이어도 worker로 선정됨
  (`service/internal/placement/distributed.go` — workers는 `OnlineNodes()`에서 선택).
- `--rpc-standalone`: 로컬 엔진 없이 ggml-rpc-server만 실행 (노드가 `RPC_BIN`으로 직접 기동).
- `--rpc-bind 127.0.0.1` (기본값): 0.0.0.0 노출 방지.
- `--rpc-announce` 생략 → `127.0.0.1:50052`로 광고. coordinator(Oracle)가
  `127.0.0.1:50052`로 다이얼하면 역터널을 통해 Mac의 rpc-server에 도달.
- `--rpc-models qwen2.5-0.5b-q4`: 모델 파일 존재를 증명으로 검증 (로드하지는 않음 —
  coordinator가 텐서를 전송).

### 3단계: 등록 확인 (Oracle에서 또는 Mac 터널 경유)

```bash
curl -s http://127.0.0.1:8080/network/status | python3 -c "
import json,sys
d = json.load(sys.stdin)
for n in d['nodes']:
    print(n['id'][:12], n.get('os'), 'rpc:', n.get('rpc_addr'), 'models:', n.get('models'))
"
```

확인: Mac 노드에 `rpc_addr: "127.0.0.1:50052"`, `models`에 `qwen2.5-0.5b-q4` 포함.

### 4단계: 기준 측정 (단일 노드, 분산 OFF)

```bash
# 시각 기록 (로그 대조용)
date -u +"%Y-%m-%d %H:%M"

curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5-0.5b-q4","messages":[{"role":"user","content":"Explain quantum computing in one paragraph."}],"max_tokens":64}' \
  | tee baseline.json
```

`baseline.json`의 `usage` (prompt/completion tokens)와 전체 소요시간을 기록.

### 5단계: 공동 추론 요청 (분산 ON)

```bash
date -u +"%Y-%m-%d %H:%M"   # 시작 시각 기록

curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5-0.5b-q4","distributed":true,"messages":[{"role":"user","content":"Explain quantum computing in one paragraph."}],"max_tokens":64}' \
  | tee distributed.json

date -u +"%Y-%m-%d %H:%M"   # 종료 시각 기록
```

동작 원리 (`service/internal/api/api.go` → `placement.DistributedGroup` →
`dispatch.Dispatch` → coordinator 노드의 `executor`):
1. API가 coordinator(Oracle 노드) + cohort(Mac worker)를 선정
2. coordinator 노드에 `Strategy: "DISTRIBUTED"`, `RpcBackends: ["127.0.0.1:50052"]` 작업 전달
3. coordinator의 executor가 llama-server를 `--rpc 127.0.0.1:50052`로 재시작
4. llama-server가 레이어 텐서 연산을 Mac의 rpc-server로 오프로드

## 증거 수집

### 증거 1 — Coordinator (Oracle) llama-server 로그 [필수]

엔진 재시작 로그는 노드의 systemd 저널에 남는다 (엔진 stderr가 노드 프로세스로 상속됨).

```bash
# Oracle에서 — 5단계 시작 시각 이후 로그만
journalctl --user -u distributed-ai-alpha-node.service --since "2026-10-2X HH:MM" | grep -iE "rpc|offload" | head -40
```

**PASS 조건 (둘 다 필요)**:
- (a) RPC 백엔드 등록을示す 로그 존재 (`--rpc`로 재시작됐다는 증거)
- (b) 레이어 오프로드 요약에서 **offload된 레이어 수 > 0**
  (llama.cpp는 시작 시 "offloaded N/M layers" 형태의 요약을 출력 — 실제 줄을 그대로 보관)

**FAIL 판정**: (b)에서 offload 레이어가 0이면 Mac이 연산에 참여하지 않은 것.
텍스트가 반환됐더라도 "공동 연산 성공"으로 보고하면 안 됨.
→ 원인 후보 1순위: `--n-gpu-layers` 미전달 (아래 "알려진 주의점 1" 참조).

### 증거 2 — Worker (Mac) rpc-server 로그 [필수]

```bash
# Mac에서 — 5단계 시간대의 로그만 추출
grep -iE "rpc" mac-phase-b.log | head -40
```

**PASS 조건**: 5단계 실행 시간대에 rpc-server가 요청을 수신·처리한 기록이 있을 것.
유휴 시간대에는 조용하고, 추론 시간대에만 활동이 있어야 정상.

**FAIL 판정**: 시간대에 활동이 전혀 없으면 텐서가 Mac에 도달하지 않은 것
(역터널 문제 또는 coordinator가 RPC 백엔드에 연결하지 못한 것).

### 증거 3 — 역터널 트래픽 [보조]

RPC 텐서 트래픽은 Mac↔Oracle 간 SSH 연결 안에 다닌다.
Oracle에서 해당 SSH 연결의 송수신 바이트 델타를 측정:

```bash
# Oracle에서 — 5단계 직전
ss -ti 'sport = :22' | grep -B2 -A8 "<mac-공인-ip>" | grep -E "bytes_acked|bytes_received" | tee traffic-before.txt

# 5단계 직후 동일 명령 → traffic-after.txt
# 델타가 추론 중에만 수 MB 이상 급증하면 텐서가 네트워크를 탄 증거
```

대체안 (거친 측정): `cat /proc/net/dev`의 인터페이스 카운터 전/후 비교.
유휴 상태 델타와 비교해 추론 시간대에만 급증하는지 확인할 것.

### 증거 4 — 성능 특성 변화 [보조]

- `baseline.json` vs `distributed.json`: 동일 프롬프트·동일 max_tokens.
- **정상적인 결과**: distributed가 baseline보다 **느림**
  (SSH 터널 왕복 오버헤드 — 텐서가 네트워크를 탔다는 방증).
- **의심스러운 결과**: 속도 차이가 전혀 없으면 offload가 실제로 일어나지 않았을 가능성
  → 증거 1의 offload 레이어 수로 확정 판정.
- 출력 텍스트는 유사해야 함 (완전히 다르면 다른 문제 — 별도 조사).

### 증거 5 — 크레딧 정산 [보조]

```bash
# spender(요청자) 차감 내역
curl -s http://127.0.0.1:8080/v1/credits/<spender>/history -H "Authorization: Bearer <API_KEY>"

# coordinator(Oracle 노드) 수익 — job ID로 대조
curl -s http://127.0.0.1:8080/v1/credits/<oracle-node-id> -H "Authorization: Bearer <API_KEY>"
```

`distributed.json`의 `id` (job-...)와 대조해 해당 작업의 정산이 맞는지 확인.

## 판정표

| # | 증거 | PASS 기준 | FAIL 시 의미 |
|---|------|-----------|--------------|
| 1 | coordinator offload 로그 | offload 레이어 수 > 0 | Mac 미참여 — `-ngl` 누락 의심 |
| 2 | Mac rpc-server 로그 | 추론 시간대에 요청 수신 기록 | 역터널/연결 문제 |
| 3 | 터널 트래픽 델타 | 추론 시간대에만 급증 | 텐서가 네트워크를 안 탐 |
| 4 | 속도 특성 | distributed ≠ baseline (느린 게 정상) | 차이 없으면 offload 의심 |
| 5 | 크레딧 정산 | job ID 대조 일치 | 정산 버그 (별도 이슈) |

**보고 규칙**: 1+2 PASS → "공동 연산 성공" 보고 가능 (3·4 첨부).
1 또는 2 FAIL → "공동 연산 실패/미검증"으로 보고하고 원인부터 해결.
어떤 경우에도 "텍스트가 나왔으니 성공"으로 보고하지 않는다.

## 알려진 주의점 (Phase B 실행 전 확인)

### 1. `--n-gpu-layers` 미전달 가능성 [코드 확인 필요]

현재 `node/internal/executor/executor.go`의 `ensureModel()`은 분산 요청 시
llama-server에 `--rpc <backends>`만 전달하고 `--n-gpu-layers`를 전달하지 않는다
(2곳: preplaced 재시작 경로, lazy 첫 기동 경로).

llama.cpp 공식 RPC 레시피는 `--rpc`와 `-ngl 99`를 함께 사용한다
(예: `llama-server -m model.gguf -ngl 99 --rpc 10.0.9.2:50052`).
`-ngl` 없이 RPC 백엔드만 등록하면 레이어가 오프로드되지 않아
**모든 연산이 coordinator CPU에서 실행되고도 텍스트는 정상 반환**된다 —
증거 1의 offload 로그로만 구분 가능하다.

**수정 완료** (2026-10-19, 커밋 예정):
```go
// executor.go 2곳 모두 — RpcNgl은 JobRequest.rpc_ngl에서 전달 (0이면 99)
cfg.ExtraArgs = []string{"--rpc", strings.Join(rpcBackends, ","), "--n-gpu-layers", fmt.Sprint(ngl)}
```
`protocol.JobRequest`에 `RpcNgl int` 필드 추가. API가 요청별로 레이어 수를 지정 가능.

### 레이어 배치 실험 계획

0.5B 모델은 24개 레이어. `-ngl` 값에 따른 배치:

| 실험 | `-ngl` | Oracle CPU | Mac GPU | 목적 |
|------|--------|-----------|---------|------|
| 1 (검증) | 99 | 0 레이어 | 24 레이어 | Mac GPU 단독 오프로드 확인 |
| 2 (혼합) | 12 | 12 레이어 | 12 레이어 | CPU+GPU 실제 분담 증명 |
| 3 (혼합) | 8 | 16 레이어 | 8 레이어 | 소수 레이어 오프로드 특성 |

**정확한 레이어 배치는 llama-server 실행 로그로 확인.** 로그에 표시되는
offload 레이어 목록을 그대로 보관 (증거 1).

**증거 요구사항 (양쪽 모두):**
- **Oracle CPU 참여 증거**: llama-server 로그에 로컬 실행 레이어 표시,
  또는 `-ngl 12` 실험에서 CPU 사용률 상승 (htop/perf)
- **Mac GPU 참여 증거**: rpc-server 로그에 텐서 수신·연산 기록,
  Metal GPU 활성 (Activity Monitor 또는 `powermetrics`)
- 단순 RPC 연결 성공, GPU 메모리 할당만으로는 **공동 계산 성공 판정 불가**.
  실제 행렬 연산이 양쪽에서 일어났다는 로그가 있어야 함.

### 2. Oracle 700MiB RAM 제한

분산 요청 시 coordinator의 llama-server가 재시작되면서 모델을 다시 로드한다.
0.5B 모델이라 여유 있으나, 재시작 직후 메모리 스파이크를 볼 것:
```bash
journalctl --user -u distributed-ai-alpha-node.service --since "..." | grep -iE "memory|oom|killed"
systemctl --user show distributed-ai-alpha-node.service -p MemoryMax
```
OOM kill 발생 시 Phase B 중단 후 원인 분석.

### 3. 포트 충돌 사전 확인 (Oracle)

```bash
ss -ltn | grep -E "50052|18082|8080|4001"
```
- `127.0.0.1:50052`는 역터널 전용 — 다른 프로세스가 잡고 있으면 터널 실패.
- 엔진 포트(18082)는 재시작 시 잠깐 비었다 다시 잡힘 — 정상.

### 4. 크레딧 장부는 인메모리

`dain-svc`를 재시작하면 잔액·내역이 소실된다.
**Phase B 중에는 dain-svc를 건드리지 말 것.**
테스트 전후로 `GET /v1/credits/test-user` 잔액을 기록해 둘 것.

### 5. ctx-size

Oracle 노드의 `ENGINE_CTX_SIZE` 승인(512)이 대기 중이다.
현재 설정값을 먼저 확인하고 프롬프트 길이를 그 안에 맞출 것:
```bash
systemctl --user show distributed-ai-alpha-node.service -p Environment | tr ' ' '\n' | grep -i ctx
```

## 로그 보관

증거로 제출할 때는 아래를 묶어서 보관:
- `baseline.json`, `distributed.json` (API 응답 원본)
- coordinator 저널 발췌 (`journalctl ... > oracle-engine.log`)
- `mac-phase-b.log` 발췌 (추론 시간대)
- `traffic-before.txt` / `traffic-after.txt`
- 판정표 체크 결과

파일명 규칙: `phase-b-evidence-YYYYMMDD/`
