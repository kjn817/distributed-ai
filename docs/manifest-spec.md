# Distributed AI Network — Model Manifest & Shard Spec

- 확정일: 2026-10-17 (0일차 결정)
- 개정: 2026-10-17 rev2 — Storage/Materialization/Compute 3층 분리, execution strategy 개념 도입
- 상태: v2 — 이후 변경 시 버전업

## 설계 원칙

저장, 실행 준비, 연산 배치는 **서로 다른 개념**이다. 한 층의 단위를 다른 층에 투영하지 않는다.

```text
Storage Shard      모델을 여러 Node의 SSD에 분산 저장하는 단위 (chunk 기반)
      ↓
Materialization    실행 시 필요한 shard를 준비하는 과정 (전략에 따라 다름)
      ↓
Compute Placement  실제 layer/tensor를 어떤 CPU/GPU Worker가 담당할지 결정
```

## 1. Storage Shard

- 모델 파일을 일정 크기로 자른 **저장 단위**. P2P 전송·복제·healing의 최소 단위.
- Chunk 크기: **64 MiB 고정** (마지막 chunk만 더 작을 수 있음). Alpha의 전송/복제 단위.
- 해시: SHA-256 (청크별). Chunk ID: `{model_id}/chunk-{index:05d}`.
- content-addressed: chunk의 해시가 곧 주소.
- ⚠️ **64MB chunk는 저장/전송 단위일 뿐이다. compute shard와 동일시하지 않는다.**
  llama.cpp의 layer/tensor 배치와 storage chunk는 별개의 개념이며, 둘 사이의 매핑은
  Materialization이 담당한다.

## 2. Materialization (실행 준비 과정)

실행 전략에 따라 "필요한 것을 준비한다"의 의미가 달라진다.

### FULL_MATERIALIZE (v1 실행 전략)

- 전체 chunk를 **한 곳에** 모아 원본 파일로 재조립 → `full_file_hash` 검증 → llama.cpp 로드.
- 초기 0.5B~7B는 이 전략으로 간다. 구현이 단순하고, 이 크기에서는 단일 노드 재조립이 항상 가능하다.
- ⚠️ 이것은 **초기 execution strategy**이지 시스템의 영구 불변 규칙이 아니다.

### DISTRIBUTED_MATERIALIZE (향후 실행 전략)

- 각 Worker는 자신에게 배정된 **layer/tensor에 필요한 shard만** 준비한다.
- 어떤 단일 Node도 전체 모델을 보유하거나 재조립할 필요가 없는 구조가 목표.
- 커스텀 worker protocol / scheduler와 함께 도입. manifest 형식 변경 없이 전략만 추가한다.

## 3. Compute Placement

- 실제 layer/tensor를 어떤 CPU/GPU Worker가 담당할지 결정하는 과정 (스케줄러 영역).
- 입력: Worker들의 연산 능력·메모리·RTT·대역폭, 모델의 layer/tensor 구조.
- v1에서는 "단일 노드 우선" 정책으로 고정 (방어선). 메커니즘은 전략 교체를 허용하도록 설계.

## Manifest 형식 (YAML)

```yaml
model_id: qwen2.5-0.5b-q4
version: 1
format: gguf
quant: q4_k_m
size_bytes: 491400032
chunk_size_bytes: 67108864
full_file_hash:
  algo: sha256
  value: "..."
chunks:
  - index: 0
    hash: "..."
    size: 67108864
  - index: 1
    hash: "..."
    size: 62500000
execution:
  strategies: ["FULL_MATERIALIZE"]   # v1. DISTRIBUTED_MATERIALIZE는 향후 추가
  # 전략이 추가되어도 chunk 목록 형식은 바뀌지 않는다.
requirements:
  storage_bytes: 397875000
  memory_bytes: 1073741824
  min_nodes: 1
replication:
  min: 2
  target: 3
source:
  type: seed
  url: "https://..."
license: "Apache-2.0"
```

## 복제 규칙

- 목표 복제본 수(`target`)만큼 서로 다른 노드에 chunk 세트 유지.
- `min` 아래로 떨어지면 healing 대상 (새 노드 선택 → P2P 전송).
- 인기 모델은 replica planner가 `target` 상향.

## 확장성 보장

- 단일 Node가 전체 GGUF를 저장·재조립·로드할 수 없는 크기(수십B+)가 와도
  manifest의 chunk 목록은 그대로 유효하다. 바뀌는 것은 `execution.strategies`와
  Materialization/Placement 구현뿐이다.
- 어떤 코드도 "chunk == compute shard"라고 가정해서는 안 된다.
  storage chunk → (materialization) → layer/tensor 배치의 2단계 매핑을 항상 거친다.

## "실제" 원칙

- `/network/status`는 실제 heartbeat 노드들의 자원 합산 (mock 숫자 금지).
- Model Quest는 실제 네트워크 자원 스냅샷으로 계산.
- shard replica는 실제 파일 생성·전송·hash 검증.
- chat completion은 실제 llama.cpp 추론.
- Worker는 실제 Node Runtime으로 작업 전달.
