# Distributed AI — GitHub 저장소 및 Oracle 배포 제안

## 1. GitHub 저장소

### 현황
- `jnsportium-alt/distributed-ai-node`: 2026-10-17 생성, README.md 21바이트만 존재 (실질적으로 빈 저장소).
- 현재 코드: `~/workspace/distributed-ai/` — 단일 Go 모듈 (`module distributed-ai`).
  - `service/` (중앙 서비스), `node/` (노드 런타임), `engine/`, `protocol/`, `limits/`.
  - 단일 모듈이므로 단일 저장소가 자연스러움.

### 제안: 기존 저장소 활용 + 이름 변경
1. `distributed-ai-node` → `distributed-ai`로 rename (GitHub Settings에서 가능, 리다이렉트 자동).
2. 현재 코드를 push (main 브랜치).
3. 이유:
   - 저장소를 새로 만들 필요가 없음 (이미 있음).
   - `distributed-ai-node`는 노드만 지칭하지만, 프로젝트는 서비스+노드+엔진 전체.
   - 단일 Go 모듈 = 단일 저장소가 관리 용이.

### 대안 (비추천)
- 새 `distributed-ai` 저장소 생성 후 기존 것은 archive: 불필요한 저장소 증가.
- service/node 분리 저장소: 단일 모듈 구조와 충돌, import 경로 복잡.

**결정 필요**: rename 후 push 진행해도 될까요?

## 2. Oracle VM 배포 (안전한 방법)

### 현황
- VM: Ubuntu x86_64, 1 OCPU, ~1GB RAM, GPU 없음, ~4GB swap.
- 권장: `ENGINE_CTX_SIZE=512` (엔진 RSS 569MB vs 기본 845MB).

### 제안: 사용자가 직접 실행하는 스크립트 방식
**개인 SSH 키를 저에게 주지 마세요.** 대신:

1. 제가 `scripts/oracle-setup.sh`를 작성합니다:
   - Go 설치 (~/workspace/go).
   - 저장소 clone.
   - llama.cpp CPU 빌드 (GGML_NATIVE=OFF).
   - systemd 서비스 등록 (노드 또는 서비스).
   - 방화벽 포트 오픈 안내.

2. 사용자가 Oracle Cloud Console → Cloud Shell (브라우저)에서 스크립트를 실행합니다.
   - SSH 키 공유 불필요.
   - 사용자가 직접 명령어를 보고 실행하므로 투명함.

3. 검증:
   - 스크립트 실행 후 `curl http://localhost:8080/v1/models`로 확인.
   - 로그는 `journalctl -u distributed-ai -f`로 확인.

### 대안
- Tailscale: VM을 Tailscale 네트워크에 join → 제가 직접 접속. (사용자가 Tailscale 설치 필요)
- GitHub Actions: push 시 자동 배포. (Oracle API 키 필요, 초기 설정 복잡)

**결정 필요**: 스크립트 방식을 작성해도 될까요? 아니면 Tailscale을 선호하시나요?

## 3. CI/CD (저장소 결정 후)
- `.github/workflows/`: Go build/vet/test + llama.cpp 빌드 매트릭스 (이미 `engines.yml` 초안 있음).
- 저장소가 정해지면 활성화.

## 4. 인스톨러/자동업데이트 (CI 이후)
- GitHub Releases에 바이너리 업로드.
- 노드: 시작 시 버전 체크 → 새 버전 있으면 다운로드 후 재시작.
- 상세 설계는 CI 활성화 후.
