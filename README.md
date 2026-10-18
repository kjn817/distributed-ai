# distributed-ai-node

> 2026-10-18: 저장소 이름이 `distributed-ai`로 변경됨. 기존 Git 이력(Initial commit)과 릴리스 v0.7/v0.8은 그대로 보존.

---

# Distributed AI Network

실험적 분산 AI 네트워크. 남는 CPU/GPU/RAM/SSD를 공유받아 LLM 추론을 수행한다.

## 언어 결정

- 서비스 코어(API/스케줄러/레지스트리)도 **Go**로 구현한다.
- 이유: Node 런타임이 이미 Go라 언어를 하나로 유지. 정적 바이너리 하나로 Oracle에 배포.
- 기술스택 문서의 "FastAPI 또는 Go" 중 Go를 선택. (결정일: 2026-10-17)

## 구조

```text
distributed-ai/
  docs/            # 스펙 문서 (manifest-spec.md 등)
  service/         # 중앙 서비스 (Oracle) — M1
    cmd/server/    # 진입점
    internal/api/      # HTTP API (OpenAI-compatible + network)
    internal/registry/ # Node/Model 레지스트리 (현재 in-memory, 나중에 PostgreSQL로 교체)
    internal/model/    # manifest 로드 + SHA-256 검증
    internal/scheduler/ # 추론 라우팅 선택 (단일노드 기본)
    internal/credit/   # append-only 크레딧 원장
  engine/            # 공용 엔진 모듈 (module distributed-ai/engine)
                       # llama.cpp 프로세스 관리 + 자동 설치/선택 — service·node 공용
  node/                  # Node 런타임 (module distributed-ai/node) — M2 병합 완료
    cmd/node/            # 진입점 (원본 main.go 흐름 그대로)
    internal/identity/   # Ed25519 identity 영속화
    internal/hardware/   # CPU/RAM/SSD/GPU 감지
    internal/protocol/   # /distributed-ai/resource/0.4.0 + ResourceInfo (wire 호환)
    internal/p2p/        # libp2p 호스트, 스트림 핸들러, connect+send
  engines/         # llama.cpp 엔진 빌드 매트릭스 (CI)
  web/             # Next.js 프론트 (후순위)
```

## 아키텍처 방어선 (측정 스킵에 따른 결정)

1. 라우팅 정책은 **단일 노드 우선으로 고정**. 다노드 분산 메커니즘은 만들되 실전 투입은 보수적으로.
2. 노드 등록 시 **직접 연결 가능 여부**를 체크. 릴레이 노드는 연산 풀에서 제외 (저장 풀은 OK).
3. **GB 단위 트래픽(모델 파일, RPC 텐서)은 중앙 서버를 거치지 않는다.** 중앙은 컨트롤 플레인만.

## 현재 상태

- [x] Repo scaffold + manifest/shard 스펙
- [ ] M1: 서비스 코어
- [ ] 엔진 빌드 매트릭스 CI
- [ ] M2: Node 런타임 병합
