#!/bin/bash
# GitHub 업로드 전 안전 검사
#
# 사용법:
#   ./scripts/pre-push-check.sh
#
# 기존 저장소 clone 후 새 코드를 추가하기 전에 실행.
# 민감하거나 불필요한 파일이 포함되지 않았는지 검사하고,
# push될 변경 파일 목록을 보여줌.

set -e

WORK_DIR="${1:-$HOME/workspace/distributed-ai}"
cd "$WORK_DIR"

echo "========================================"
echo " Pre-push Safety Check"
echo "========================================"
echo "Directory: $WORK_DIR"
echo ""

FAILED=0

# 1. 민감한 파일 검사
echo "[1/4] Checking for sensitive files..."
SENSITIVE_PATTERNS=(
    ".env"
    ".env.*"
    "*.pem"
    "*.key"
    "id_rsa*"
    "id_ed25519*"
    "*secret*"
    "*password*"
    "*credential*"
    ".ssh/"
    "identity.key"
)

FOUND_SENSITIVE=()
for pattern in "${SENSITIVE_PATTERNS[@]}"; do
    while IFS= read -r -d '' file; do
        # .git 디렉토리 제외
        if [[ "$file" != *"/.git/"* ]]; then
            FOUND_SENSITIVE+=("$file")
        fi
    done < <(find . -name "$pattern" -print0 2>/dev/null)
done

# Node 개인키는 특별히 확인 (~/.distributed-ai-node는 홈에 있지만, 혹시 작업 폴더에 있을 경우)
if [ -f "$HOME/.distributed-ai-node/identity.key" ]; then
    echo "  INFO: Node identity key exists in HOME (not in repo, OK)."
fi

if [ ${#FOUND_SENSITIVE[@]} -gt 0 ]; then
    echo "  ERROR: Sensitive files found:"
    printf '    %s\n' "${FOUND_SENSITIVE[@]}"
    FAILED=1
else
    echo "  OK: No sensitive files."
fi
echo ""

# 2. 불필요한 대용량 파일 검사
echo "[2/4] Checking for large/unnecessary files..."
# 모델 파일 (.gguf)
LARGE_FILES=$(find . -name "*.gguf" -not -path "./.git/*" 2>/dev/null)
if [ -n "$LARGE_FILES" ]; then
    echo "  ERROR: Model files found (should not be in repo):"
    echo "$LARGE_FILES" | sed 's/^/    /'
    FAILED=1
fi

# 빌드 캐시
CACHE_DIRS=$(find . -type d \( -name "__pycache__" -o -name ".pytest_cache" -o -name "node_modules" -o -name "target" \) -not -path "./.git/*" 2>/dev/null | head -5)
if [ -n "$CACHE_DIRS" ]; then
    echo "  WARNING: Build cache directories found:"
    echo "$CACHE_DIRS" | sed 's/^/    /'
fi

# 바이너리 (Go 빌드 결과물)
BINARIES=$(find . -type f -executable -not -path "./.git/*" -not -name "*.sh" 2>/dev/null | head -10)
if [ -n "$BINARIES" ]; then
    echo "  WARNING: Executable binaries found (review before push):"
    echo "$BINARIES" | sed 's/^/    /'
fi

# 대용량 파일 (>10MB)
BIG_FILES=$(find . -type f -size +10M -not -path "./.git/*" 2>/dev/null | head -10)
if [ -n "$BIG_FILES" ]; then
    echo "  WARNING: Files larger than 10MB:"
    echo "$BIG_FILES" | sed 's/^/    /'
    echo "  Consider using Git LFS or Releases for large binaries."
fi

if [ -z "$LARGE_FILES" ]; then
    echo "  OK: No model files."
fi
echo ""

# 3. .gitignore 확인
echo "[3/4] Checking .gitignore..."
if [ ! -f ".gitignore" ]; then
    echo "  WARNING: No .gitignore file. Creating recommended one..."
    cat > .gitignore << 'EOF'
# Go
*.exe
*.test
*.out

# Models (large, download separately)
*.gguf

# Binaries (use Releases)
/bin/
/build/

# Node identity (sensitive)
**/identity.key

# Env
.env
.env.*

# OS
.DS_Store
Thumbs.db

# IDE
.vscode/
.idea/

# Build cache
__pycache__/
.pytest_cache/
EOF
    echo "  Created .gitignore"
else
    echo "  OK: .gitignore exists."
    # 필수 항목 확인
    for required in "*.gguf" "identity.key" ".env"; do
        if ! grep -q "$required" .gitignore; then
            echo "  WARNING: .gitignore missing pattern: $required"
        fi
    done
fi
echo ""

# 4. 변경 파일 목록 (전체)
echo "[4/4] Files to be pushed (FULL LIST)..."
if git rev-parse --git-dir > /dev/null 2>&1; then
    echo ""
    echo "  === Staged changes ==="
    STAGED=$(git diff --cached --name-status)
    if [ -n "$STAGED" ]; then
        echo "$STAGED" | sed 's/^/    /'
        STAGED_COUNT=$(echo "$STAGED" | wc -l)
    else
        echo "    (none)"
        STAGED_COUNT=0
    fi
    echo ""
    echo "  === Unstaged changes ==="
    UNSTAGED=$(git diff --name-status)
    if [ -n "$UNSTAGED" ]; then
        echo "$UNSTAGED" | sed 's/^/    /'
        UNSTAGED_COUNT=$(echo "$UNSTAGED" | wc -l)
    else
        echo "    (none)"
        UNSTAGED_COUNT=0
    fi
    echo ""
    echo "  === Untracked files ==="
    UNTRACKED=$(git status --porcelain | grep "^??")
    if [ -n "$UNTRACKED" ]; then
        echo "$UNTRACKED" | sed 's/^/    /'
        UNTRACKED_COUNT=$(echo "$UNTRACKED" | wc -l)
    else
        echo "    (none)"
        UNTRACKED_COUNT=0
    fi
    echo ""
    TOTAL=$((STAGED_COUNT + UNSTAGED_COUNT + UNTRACKED_COUNT))
    echo "  Total: $TOTAL files (staged: $STAGED_COUNT, unstaged: $UNSTAGED_COUNT, untracked: $UNTRACKED_COUNT)"
    
    # 전체 파일에 대한 민감 정보 재검사
    echo ""
    echo "  === Full sensitive scan on all changed files ==="
    ALL_CHANGED=$(git status --porcelain | awk '{print $2}')
    SCAN_FAILED=0
    for f in $ALL_CHANGED; do
        if [ -f "$f" ]; then
            # API 키 패턴 검사
            if grep -qiE "(api[_-]?key|secret|password|token).{0,20}[=:].{8,}" "$f" 2>/dev/null; then
                echo "    WARNING: Possible secret in $f"
                SCAN_FAILED=1
            fi
        fi
    done
    if [ $SCAN_FAILED -eq 0 ]; then
        echo "    OK: No secrets detected in changed files."
    fi
else
    echo "  Not a git repository yet. Files that would be added:"
    find . -type f -not -path "./.git/*" | sed 's/^/    /'
    TOTAL=$(find . -type f -not -path "./.git/*" | wc -l)
    echo "  Total files: $TOTAL"
fi
echo ""

echo "========================================"
if [ $FAILED -eq 1 ]; then
    echo " FAILED: Sensitive files detected. Do NOT push."
    echo "========================================"
    exit 1
else
    echo " PASSED: Safe to proceed."
    echo ""
    echo "다음 단계:"
    echo "  1. 위 변경 파일 목록을 검토하세요."
    echo "  2. 문제가 없으면: git add -A && git commit"
    echo "  3. git push origin main"
    echo "========================================"
fi
