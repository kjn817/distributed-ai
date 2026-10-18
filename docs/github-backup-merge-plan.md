# GitHub 저장소 백업/병합 계획

## 현황 파악 (2026-10-18 실측)

### 기존 저장소: `jnsportium-alt/distributed-ai-node`
- 생성: 2026-10-17T08:38:31Z
- Git 내용: README.md 21바이트만 (실질적으로 빈 저장소)
- **Releases 2개 (보존 필수)**:
  - `v0.8` (2026-10-17): `distributed-ai-node-windows-v08.exe` (25,139,712 bytes)
  - `v0.7` (2026-10-17): `distributed-ai-node-windows-v07.exe` (25,015,296 bytes)
- 위 바이너리는 기존 Node Runtime (Alpha 이전 버전) — 사용자가 "기존 코드"로 지칭한 자산.

### 새 코드: `~/workspace/distributed-ai/`
- 단일 Go 모듈 (`module distributed-ai`)
- 구성: `service/` + `node/` + `engine/` + `protocol/` + `limits/`
- 기존 `distributed-ai-node`의 Go 코드와는 다른 코드베이스 (병합이 아닌 신규).

## Rename 영향 분석

### GitHub 자동 처리
- `distributed-ai-node` → `distributed-ai` rename 시 GitHub이 자동 리다이렉트 생성.
- 기존 clone의 `git remote`는 수동 업데이트 필요 (`git remote set-url origin`).

### Release 자산 다운로드 링크
- 기존 URL: `github.com/jnsportium-alt/distributed-ai-node/releases/download/v0.8/distributed-ai-node-windows-v08.exe`
- Rename 후: GitHub이 자동 리다이렉트 (웹 UI 기준).
- **단, 리다이렉트에만 의존하는 것은 권장되지 않음** — 중요 다운로드 링크는 새 URL로 업데이트 권장.
- Release 자체는 저장소와 함께 이동 (삭제되지 않음).

### CI 영향
- 현재 `.github/workflows/` 없음 (비활성 상태).
- Rename 후 새로 작성하는 CI에는 영향 없음.

## 백업 계획 (rename 전 실행)

### 백업 위치
`~/workspace/distributed-ai-backup/` — 다운로드 가능한 형태로 보존.

### 1. Release 자산 백업 (완료 2026-10-18)
| 파일 | SHA-256 |
|------|---------|
| `distributed-ai-node-windows-v07.exe` (24MB) | `bffe97e54548a033d64a699638b63e06f468ab1855e7366d15f3a5c3dc69aa4c` |
| `distributed-ai-node-windows-v08.exe` (24MB) | `7d74a39c035b6d617cad66aaafefaabb134318f5dd190f33b3d28c9d98880bd1` |
| `distributed-ai-node-source.zip` (15KB, 사용자 제공 원본 소스) | `b07c1e9b7ce09d3f5241fc24599737b2c409b43426136f9d022b07263627b192` |

SHA-256 목록: `~/workspace/distributed-ai-backup/SHA256SUMS.txt`

### 2. Git 히스토리 백업 (완료 2026-10-18)
```bash
# 이미 실행됨
git clone --mirror https://github.com/jnsportium-alt/distributed-ai-node.git \
  ~/workspace/distributed-ai-backup/distributed-ai-node.git
# 히스토리: e649ed5 "Initial commit" (README만)
```

### 3. 기존 Node Runtime 기능 보존 확인 (완료 2026-10-18)
원본 ZIP (`distributed-ai-node-source.zip`, main.go 493줄)과 새 코드 비교:

| 원본 기능 | 새 코드 위치 | 보존 여부 |
|-----------|--------------|-----------|
| Persistent Peer ID (`~/.distributed-ai-node/identity.key`, 0700/0600) | `node/internal/identity/identity.go` | ✅ 동일 경로/권한 |
| P2P 연결 (`--connect`, libp2p) | `node/internal/p2p/`, `node/cmd/node/main.go` | ✅ |
| 프로토콜 ID `/distributed-ai/resource/0.4.0` | `node/internal/protocol/resource.go` | ✅ 동일 (테스트로 고정) |
| Mac GPU 감지 (`system_profiler`) | `node/internal/hardware/gpu.go` | ✅ `detectMacGPU()` 포팅됨 |
| Windows GPU 감지 (`Win32_VideoController`) | `node/internal/hardware/gpu.go` | ✅ `detectWindowsGPU()` 포팅됨 |
| 가상 GPU 필터 (`isVirtualGPU`) | `node/internal/hardware/gpu.go` | ✅ 포팅됨 |
| 벤더 정규화 (`normalizeGPUVendor`) | `node/internal/hardware/gpu.go` | ✅ 포팅됨 |
| ResourceInfo JSON 필드 | `node/internal/protocol/resource.go` | ✅ `NodeName`, `GPUVendor`, `MetalSupport` 등 동일 |
| 자원 정보 교환 (스트림) | `node/internal/p2p/` + reporter | ✅ 확장됨 (service 등록 추가) |

**결론**: 원본의 핵심 기능 4가지(Persistent Peer ID, P2P 연결, Windows/Mac 하드웨어 감지, 자원 정보 교환)가 모두 새 코드에 반영됨. 단, 원본은 단일 main.go 493줄이었고 새 코드는 패키지 분리된 구조이므로, 직접적인 코드 병합이 아닌 기능 수준의 보존임.

## 병합 계획 (rename 후 실행, 사용자 확인 후)

### 단계 1: Rename (사용자 확인 후)
- GitHub 웹 UI → Settings → Repository name → `distributed-ai` → Rename.
- 확인: `github.com/jnsportium-alt/distributed-ai` 접속 가능.
- 확인: 기존 release 2개(v0.7, v0.8)가 그대로 있는지.

### 단계 2: 기존 저장소 Clone 후 새 코드 추가 (force push 없음)
**기존 방식(폐기)**: `git init` 후 새 저장소로 push — 이력이 충돌할 수 있음.
**수정된 방식**: 기존 저장소를 clone하고 그 안에 새 코드를 안전하게 추가.

```bash
# 1. 기존 저장소 clone (rename 후 URL)
git clone https://github.com/jnsportium-alt/distributed-ai.git
cd distributed-ai
# 현재: README.md만 있는 main 브랜치 (e649ed5)

# 2. 새 코드를 하위 디렉토리로 추가하지 않고, 저장소 루트에 병합
# 기존 README.md는 유지하고, 새 코드의 파일들을 추가
# (충돌 파일이 없으므로 안전)

# 새 코드 복사 (~/workspace/distributed-ai/의 내용)
# 단, .git 디렉토리는 제외
rsync -av --exclude='.git' ~/workspace/distributed-ai/ ./

# 3. 상태 확인
git status
# 예상: README.md (modified 아님, 그대로), 새 파일들 (untracked)

# 4. 커밋
git add -A
git commit -m "Add Distributed AI Network Alpha codebase

- Service (control plane) + Node (P2P runtime) + Engine (llama.cpp)
- Preserves original Node Runtime features: persistent peer ID,
  P2P resource protocol (/distributed-ai/resource/0.4.0),
  Windows/Mac GPU detection, resource info exchange
- Phases 1a/1b/2/3 complete: credit security, OS-enforced limits,
  RPC joint inference, replica healing
- Original releases v0.7/v0.8 preserved (see Releases page)"

# 5. Push (force 아님)
git push origin main
```

**왜 안전한가**:
- 기존 커밋(e649ed5)을 기반으로 하므로 히스토리가 이어짐.
- `git push --force`를 사용하지 않음.
- README.md는 그대로 유지됨 (새 코드에 README가 없으므로 충돌 없음).
- 만약 충돌이 발생하면 push가 거부되고, 수동 해결 후 재시도.

### 단계 3: 기존 Release 보존 확인
- `https://github.com/jnsportium-alt/distributed-ai/releases` 에서 v0.7, v0.8 확인.
- 다운로드 링크 테스트 (신규 URL).

### 단계 4: 새 Release (v0.9-alpha)
- 기존 v0.7/v0.8과 별도로 `v0.9-alpha` 태그.
- Windows/macOS/Linux 바이너리 포함.
- 기존 릴리스를 덮어쓰지 않음.

## 롤백 계획
- Rename은 되돌릴 수 있음 (Settings에서 다시 `distributed-ai-node`로).
- 로컬 백업 (`~/workspace/distributed-ai-backup/`)으로 복원 가능.
- Release는 삭제되지 않으므로 안전.

## 사용자 확인 필요 사항
- [ ] Rename 실행 (`distributed-ai-node` → `distributed-ai`)
- [ ] 백업 위치 확인 (`~/workspace/distributed-ai-backup/`)
- [ ] 병합 방식 확인 (README 유지 + 새 코드 추가)
- [ ] 새 릴리스 태그명 (`v0.9-alpha`)
