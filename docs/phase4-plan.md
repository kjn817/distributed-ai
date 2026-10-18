# Phase 4 실행 계획 — 실제 인터넷 공개 Alpha
# (2026-10-18 사용자 검토 반영 수정본)

## 사용자 확인 사항 (실행 전 승인 필요)

### 1. RPC 테스트 정정
- **정정**: Phase 2 e2e의 RPC Worker 2개는 **동일 개발 머신**(127.0.0.1)에서 실행됨.
- **기록**: "로컬 통합 테스트 PASS"로 정정. 프로토콜·placement·공동계산 경로는 검증됨.
- **남은 과제**: 실제 원격 노드 간 (서로 다른 물리 장비) 공동 추론 검증 — 별도 마일스톤.

### 2. GPU 공유 정책 변경 (구현 완료)
- `--gpu` 기본값 10 → **0 (비활성화)**으로 변경.
- `--gpu 1~99`: **실행 거부** (부분 제한 불가 — 벤더 API 없음).
- `--gpu 100`: 전체 GPU 사용 명시적 동의 (경고 출력).
- 노드 시작 시 OS별 강제 지원 현황 표시.

### 3. 플랫폼별 CPU/RAM 강제 현황 (구현 완료)
| 플랫폼 | CPU | RAM | 상태 |
|--------|-----|-----|------|
| Linux | cgroup v2 `cpu.max` | cgroup v2 `memory.max` | ✅ 구현됨, 실측 verified |
| Windows | Job Objects (미구현) | Job Objects (미구현) | ⚠️ 스레드 힌트만, 명시됨 |
| macOS | API 없음 | setrlimit (미구현) | ⚠️ 스레드 힌트만, 명시됨 |

### 4. 기존 Node Runtime 보존 (확인 완료 2026-10-18)
원본 ZIP과 새 코드 비교 결과 — 4개 핵심 기능 모두 보존됨:
- Persistent Peer ID (`~/.distributed-ai-node/identity.key`)
- P2P 연결 (프로토콜 `/distributed-ai/resource/0.4.0` 동일)
- Windows/Mac GPU 감지 (`system_profiler`, `Win32_VideoController` 포팅됨)
- 자원 정보 교환 (ResourceInfo JSON 필드 동일)

상세: `docs/github-backup-merge-plan.md`

## GitHub (사용자 확인 후 실행)

### 백업 (완료 2026-10-18, 다운로드 가능)
위치: `~/workspace/distributed-ai-backup/`

| 파일 | SHA-256 |
|------|---------|
| `distributed-ai-node-windows-v07.exe` (24MB) | `bffe97e54548a033d64a699638b63e06f468ab1855e7366d15f3a5c3dc69aa4c` |
| `distributed-ai-node-windows-v08.exe` (24MB) | `7d74a39c035b6d617cad66aaafefaabb134318f5dd190f33b3d28c9d98880bd1` |
| `distributed-ai-node-source.zip` (15KB) | `b07c1e9b7ce09d3f5241fc24599737b2c409b43426136f9d022b07263627b192` |
| `distributed-ai-node.git/` (mirror clone) | Git 히스토리 보존 |

### 병합 방식 (수정됨)
**기존 방식(폐기)**: `git init` 후 push — 이력 충돌 가능.
**수정된 방식**: 기존 저장소 clone → 새 코드 추가 → push (force 없음).

```bash
git clone https://github.com/kjn817/distributed-ai.git
cd distributed-ai
rsync -av --exclude='.git' ~/workspace/distributed-ai/ ./
git add -A
git commit -m "Add Distributed AI Network Alpha codebase"
git push origin main  # force 아님
```

상세: `docs/github-backup-merge-plan.md`

**확인 필요**:
- [ ] Rename 실행 승인 (`distributed-ai-node` → `distributed-ai`)
- [ ] 병합 방식 승인
- [ ] 새 릴리스 태그명 승인 (`v0.9-alpha`)

## Oracle 배포

### 메모리 실측 결과 (개발 VM, 2026-10-18)
| 구성요소 | 메모리 |
|----------|--------|
| API 서버 (Go) | 23.6 MB |
| llama-server (0.5B Q4, ctx 512) | 568.4 MB |
| **합계** | **~592 MB** |

**주의**: 개발환경 측정값만으로 안정적 운영을 확정할 수 없음.
Oracle VM에서 실제 검증 필요 (아래 참조).

### 추론 속도 (개발 VM 기준)
- 10 토큰 생성: 1.34초 (prompt 37.7 tok/s, gen 17.3 tok/s).
- Oracle 1 OCPU에서는 더 느릴 것으로 예상.

### 배포 방식
**사용자가 Mac에서 SSH로 직접 접속 후 스크립트 실행** (Cloud Shell로는 VM 배포 불가).

스크립트: `scripts/oracle-setup.sh` (GitHub 커밋 후 사용자 검토 → 실행)

**스크립트 원칙 (2026-10-18 수정 반영)**:
- 설치 경로: `~/distributed-ai-alpha` (기존 `~/distributed-ai` 절대 건드리지 않음).
- 기존 Alpha 설치가 있으면 중단 (덮어쓰기 방지).
- 디렉토리 생성 순서: `~/.config/systemd/user/` → 서비스 파일, `bin/` → Go 빌드.
- Go 버전은 디렉토리 존재가 아닌 실제 `go version`으로 확인.
- SSH 종료 후에도 유지되도록 `loginctl enable-linger` 확인/안내.
- 사전 빌드 바이너리 우선 사용 (CI 릴리스에서 다운로드, SHA-256 검증).
  - Go 서버/노드: `dain-svc-linux-amd64`, `dain-node-linux-amd64`.
  - llama.cpp: `llama-engine-linux-amd64.tar.gz` (바이너리 + `.so` 포함).
- 1GB RAM에서 로컬 빌드는 OOM 위험으로 지원하지 않음 (릴리스 필수).
- 모델 SHA-256 검증 필수.
- **API는 127.0.0.1:8080에만 바인딩** (외부 직접 공개 금지).
- 외부 공개 시 리버스 프록시(Nginx/Caddy) + HTTPS 사용.
- 8080 포트를 인증 없이 인터넷에 직접 열지 않음.
- **Oracle Node**: API 서버와 함께 Node도 실행 — Oracle 자체가 첫 번째 자원 제공 Node.
  - Dashboard의 온라인 Node 수와 자원 집계에 반영됨.

**확인 필요**:
- [ ] 스크립트 내용 검토 승인
- [ ] 실행 승인

### Oracle 실제 검증 (배포 후)
개발환경 측정값이 아닌 실제 Oracle VM에서 확인:
- [ ] 0.5B 모델 추론 성공 여부
- [ ] API 서버 메모리 사용량
- [ ] Swap 사용량 (4GB 중)
- [ ] 추론 응답시간 (1 OCPU 기준)
- [ ] Node Runtime 실행 여부
- [ ] 오류 로그 확인
- [ ] 장시간 안정성 (24시간)

## Phase 4 완료 기준 체크리스트

- [ ] 기존 코드와 Git 이력 보존 (백업 완료 ✅)
- [ ] GitHub CI 빌드 통과 (워크플로우 작성 후)
- [ ] Oracle VM 실제 배포 (사용자 SSH 실행)
- [ ] 실제 0.5B API 추론 성공 (Oracle에서)
- [ ] Node와 Dashboard 연결 확인
- [ ] Windows/macOS/Linux 배포 바이너리 생성
- [ ] 원클릭 설치 및 자동 업데이트 검증
- [ ] 외부 사용자 연결과 기본 보안 점검

## 실행 순서 (사용자 승인 후)

### Phase A: CI 및 릴리스 (Oracle 배포 전)
1. GitHub 백업 확인 → Rename → 병합 → push. (승인 필요)
2. CI 워크플로우 활성화 (`.github/workflows/release.yml` 작성 완료).
3. `v0.9-alpha` 태그 push → CI가 자동 빌드:
   - Go 바이너리: Linux/Windows/macOS (amd64, arm64).
   - llama.cpp 엔진 패키지: `llama-engine-linux-amd64.tar.gz` (바이너리 + `.so` 포함).
   - `SHA256SUMS.txt` 자동 생성.
4. 릴리스 페이지에서 패키지 확인.

**왜 순서가 중요한가**: Oracle(1GB RAM)에서 빌드하면 OOM 위험. 사전 빌드된 패키지를 다운로드하는 것이 안전.

### Phase B: Oracle 배포 (릴리스 후)
5. `scripts/oracle-setup.sh` GitHub 커밋. (스크립트 작성 완료, 커밋 대기)
6. 사용자 스크립트 검토 → Mac SSH로 Oracle 배포 실행. (승인 필요)
   - 설치 경로: `~/distributed-ai-alpha` (기존 `~/distributed-ai` 건드리지 않음).
   - API 서버 + Node 동시 실행 (Oracle이 첫 자원 제공 Node).
7. Oracle 실제 검증 (메모리/Swap/응답시간/안정성).

### Phase C: 공개 Alpha
8. 크로스 플랫폼 인스톨러.
9. 자동 업데이트.
10. 외부 사용자 온보딩.
