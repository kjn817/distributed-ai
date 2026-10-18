#!/bin/bash
# Distributed AI Oracle VM Setup Script (Alpha)
# 
# 실행 방법 (사용자가 Mac에서 직접 SSH로 접속 후):
#   1. 스크립트 다운로드:
#      ssh ubuntu@<oracle-vm-ip>
#      curl -sSL -o /tmp/oracle-setup.sh \
#        https://raw.githubusercontent.com/kjn817/distributed-ai/main/scripts/oracle-setup.sh
#
#   2. 내용 검토 (필수):
#      less /tmp/oracle-setup.sh
#      # 또는 에디터로 열어서 확인
#
#   3. 실행:
#      bash /tmp/oracle-setup.sh
#
# 주의: curl | bash로 즉시 실행하지 마세요. 반드시 먼저 내용을 검토하세요.
#
# 주의:
# - 기존 ~/distributed-ai 폴더는 절대 건드리지 않음. 새 Alpha는 ~/distributed-ai-alpha에 설치.
# - 기존 Oracle 환경과 프로그램을 임의로 삭제하거나 덮어쓰지 않음.
# - 모든 설치는 ~/distributed-ai-alpha/ 하위에 격리됨.
# - 시스템 패키지는 apt로만 설치 (기존 설정 변경 없음).
# - API는 기본적으로 localhost(127.0.0.1)에만 바인딩.
# - 8080 포트를 인터넷에 직접 열지 않음 (인증 없는 외부 공개 금지).
# - SSH 종료 후에도 서비스가 유지되도록 linger 활성화.

set -e

INSTALL_DIR="$HOME/distributed-ai-alpha"
REPO_URL="https://github.com/kjn817/distributed-ai.git"
GO_VERSION="1.26.0"
RELEASE_TAG="v0.9-alpha"

echo "========================================"
echo " Distributed AI Oracle Setup (Alpha)"
echo "========================================"
echo "Install dir: $INSTALL_DIR"
echo " 기존 ~/distributed-ai 폴더는 건드리지 않습니다."
echo ""

# 1. 기존 Alpha 설치 확인 (덮어쓰기 방지)
if [ -d "$INSTALL_DIR" ]; then
    echo "WARNING: $INSTALL_DIR already exists."
    echo "기존 Alpha 설치를 덮어쓰지 않습니다."
    echo "계속하려면 디렉토리를 백업하거나 삭제하세요."
    exit 1
fi

# 기존 ~/distributed-ai 존재 확인 (정보용, 건드리지 않음)
if [ -d "$HOME/distributed-ai" ]; then
    echo "INFO: 기존 ~/distributed-ai 폴더가 존재합니다. 건드리지 않습니다."
fi

# 2. 시스템 패키지 (기존 설정 변경 없음, 설치만)
echo "[1/8] Installing system packages..."
sudo apt-get update -qq
sudo apt-get install -y -qq git curl > /dev/null

# 3. Go는 필요 없음 — 사전 빌드된 정적 바이너리 사용 (CGO_ENABLED=0)
# Oracle에 Go SDK를 설치하거나 기존 버전을 변경하지 않음.
echo "[2/8] Go SDK not required (using prebuilt static binaries)."

# 4. 저장소 clone
echo "[3/8] Cloning repository..."
git clone "$REPO_URL" "$INSTALL_DIR"
cd "$INSTALL_DIR"

# 5. 사전 빌드 바이너리 다운로드 (Go 서버 + Node) + SHA-256 검증
# 
# 1GB RAM 환경에서 빌드하지 않음. 다운로드 실패 시 설치 중단.
echo "[4/8] Downloading prebuilt binaries..."
mkdir -p "$INSTALL_DIR/bin"
BIN_BASE="https://github.com/kjn817/distributed-ai/releases/download/${RELEASE_TAG}"

# SHA256SUMS 다운로드 (릴리스에서)
echo "  Downloading SHA256SUMS..."
if ! curl -sL --fail -o "/tmp/SHA256SUMS.txt" "${BIN_BASE}/SHA256SUMS.txt"; then
    echo "  ERROR: SHA256SUMS.txt not found in release ${RELEASE_TAG}."
    echo "  설치를 중단합니다."
    exit 1
fi

# SHA-256 검증 함수
# $1: 로컬 파일 경로, $2: SHA256SUMS.txt에 기록된 에셋 이름 (정확히 일치해야 함)
# 주의: grep 부분 매칭 금지 — "dain-svc"로 검색하면 darwin/windows 변종까지 4개가 매칭됨
verify_sha256() {
    local file="$1"
    local asset_name="$2"
    local expected=$(awk -v name="$asset_name" '$2 == name {print $1}' /tmp/SHA256SUMS.txt)
    if [ -z "$expected" ]; then
        echo "  ERROR: No SHA-256 entry for $asset_name in SHA256SUMS.txt"
        return 1
    fi
    local actual=$(sha256sum "$file" | awk '{print $1}')
    if [ "$actual" != "$expected" ]; then
        echo "  ERROR: SHA-256 mismatch for $asset_name!"
        echo "    Expected: $expected"
        echo "    Actual:   $actual"
        return 1
    fi
    echo "  SHA-256 verified: $asset_name"
    return 0
}

download_binary() {
    local name="$1"
    local asset="${name}-linux-amd64"
    local dest="$INSTALL_DIR/bin/$name"
    local remote="${BIN_BASE}/${asset}"
    echo "  Downloading $name..."
    if ! curl -sL --fail -o "$dest" "$remote"; then
        echo "  ERROR: Failed to download $name from release ${RELEASE_TAG}."
        echo "  Oracle에서 직접 빌드하지 않습니다. 설치를 중단합니다."
        exit 1
    fi
    chmod +x "$dest"
    if ! verify_sha256 "$dest" "$asset"; then
        echo "  설치를 중단합니다."
        exit 1
    fi
}

download_binary "dain-svc"
download_binary "dain-node"

# 6. llama.cpp 엔진 패키지 (공유 라이브러리 포함)
#
# llama-server는 실행에 libggml-*.so가 필요함.
# 사전 빌드 패키지는 바이너리 + .so 파일을 포함한 tar.gz.
echo "[5/8] Setting up llama.cpp engine..."
ENGINE_DIR="$INSTALL_DIR/engines/llama.cpp"
mkdir -p "$ENGINE_DIR"

ENGINE_PKG="llama-engine-linux-amd64.tar.gz"
echo "  Downloading $ENGINE_PKG..."
if ! curl -sL --fail -o "/tmp/$ENGINE_PKG" "${BIN_BASE}/${ENGINE_PKG}"; then
    echo "  ERROR: Prebuilt engine package not found in release ${RELEASE_TAG}."
    echo "  1GB RAM에서 로컬 빌드는 OOM 위험이 있어 지원하지 않습니다."
    echo "  설치를 중단합니다."
    exit 1
fi
if ! verify_sha256 "/tmp/$ENGINE_PKG" "$ENGINE_PKG"; then
    echo "  설치를 중단합니다."
    exit 1
fi
echo "  Extracting engine package..."
tar -xzf "/tmp/$ENGINE_PKG" -C "$ENGINE_DIR"
rm "/tmp/$ENGINE_PKG"
# 압축 해제 후 경로 확인 (build/bin/ 구조)
if [ ! -f "$ENGINE_DIR/build/bin/llama-server" ]; then
    echo "  ERROR: Engine package structure invalid (missing build/bin/llama-server)."
    echo "  설치를 중단합니다."
    exit 1
fi
echo "  Engine package extracted and verified."

# 엔진 바이너리 검증
if ! "$ENGINE_DIR/build/bin/llama-server" --version > /dev/null 2>&1; then
    echo "ERROR: llama-server binary verification failed."
    echo "공유 라이브러리 누락 가능: ldd로 확인하세요."
    ldd "$ENGINE_DIR/build/bin/llama-server" 2>&1 | head -10
    exit 1
fi
echo "  llama-server verified."

# 7. 모델 다운로드 (SHA-256 검증)
echo "[6/8] Downloading model..."
MODELS_DIR="$INSTALL_DIR/models"
mkdir -p "$MODELS_DIR"
MODEL_FILE="$MODELS_DIR/qwen2.5-0.5b-instruct-q4_k_m.gguf"
EXPECTED_SHA="74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db"

if [ ! -f "$MODEL_FILE" ]; then
    curl -sL -o "$MODEL_FILE" \
        "https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf"
fi

ACTUAL_SHA=$(sha256sum "$MODEL_FILE" | awk '{print $1}')
if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
    echo "ERROR: Model SHA-256 mismatch!"
    exit 1
fi
echo "  Model verified (SHA-256 OK)."

# 8. systemd 서비스 등록 (API 서버 + Node)
#
# 보안: API는 127.0.0.1에만 바인딩 (외부 직접 공개 금지).
# SSH 종료 후에도 유지되도록 linger 활성화 필요:
#   sudo loginctl enable-linger $USER
echo "[7/8] Setting up systemd services..."

# 디렉토리를 먼저 생성 (수정사항 반영)
mkdir -p "$HOME/.config/systemd/user"

# API 서버 서비스
#
# 엔진 공유 모드: dain-svc는 로컬 llama-server를 띄우지 않고,
# Node가 소유한 엔진(http://127.0.0.1:18082)을 ENGINE_URL로 공유 사용.
# 동일 0.5B 모델을 두 번 로딩하면 1GB RAM에서 OOM → 단일 엔진으로 ~568MB 절약.
# Node 시작 전까지는 /v1/chat/completions이 503 (수 초 내 복구).
cat > "$HOME/.config/systemd/user/distributed-ai-alpha.service" << EOF
[Unit]
Description=Distributed AI Alpha - API Service (control plane)
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
Environment=MODELS_DIR=$MODELS_DIR
Environment=LISTEN_ADDR=127.0.0.1:8080
Environment=ENGINE_URL=http://127.0.0.1:18082
ExecStart=$INSTALL_DIR/bin/dain-svc
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF

# Node 서비스 (Oracle 자체가 첫 번째 자원 제공 Node)
# API 서버와 같은 머신이므로 --api는 localhost 사용
#
# 자원 계산 (Oracle: 1 OCPU, 1024MB RAM):
#   예약 (공유하지 않음):
#     - OS (Ubuntu minimal): ~150MB RAM, ~10% CPU
#     - API 서버 (Go): ~30MB RAM, ~5% CPU (실측 23.6MB)
#     - 버퍼: ~100MB RAM, ~15% CPU
#   공유 가능:
#     - RAM: 1024 - 150 - 30 - 100 = ~744MB → --ram 70 (717MB)
#       (llama-server 0.5B ctx512 실측 568MB, 여유 있음)
#     - CPU: 100 - 10 - 5 - 15 = ~70% → --cpu 70
#     - Storage: 여유 공간의 20% (모델 0.5GB + 여유)
#
# 주의: cgroup 한도는 최대치이며, 실제 사용량은 그 이하.
# 메모리 초과 방지를 위해 보수적으로 설정.
# Node가 단일 llama-server의 소유자 (포트 18082, cgroup 70% 강제).
# dain-svc(API)는 ENGINE_URL로 이 엔진을 공유 사용.
cat > "$HOME/.config/systemd/user/distributed-ai-alpha-node.service" << EOF
[Unit]
Description=Distributed AI Alpha - Node (resource provider)
After=network.target distributed-ai-alpha.service

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
Environment=ENGINE_BIN=$ENGINE_DIR/build/bin/llama-server
Environment=MODEL_ID=qwen2.5-0.5b-q4
Environment=MODEL_PATH=$MODEL_FILE
Environment=ENGINE_CTX_SIZE=512
Environment=LD_LIBRARY_PATH=$ENGINE_DIR/build/bin
ExecStart=$INSTALL_DIR/bin/dain-node --api http://127.0.0.1:8080 --cpu 70 --ram 70 --storage 20
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable distributed-ai-alpha.service
systemctl --user enable distributed-ai-alpha-node.service

# linger 활성화 확인/안내
echo ""
echo "[8/8] Checking linger status..."
if loginctl show-user "$USER" 2>/dev/null | grep -q "Linger=yes"; then
    echo "  Linger already enabled. Services will persist after SSH disconnect."
else
    echo "  WARNING: Linger is NOT enabled."
    echo "  SSH 종료 후 서비스가 중단될 수 있습니다. 아래 명령으로 활성화하세요:"
    echo "    sudo loginctl enable-linger $USER"
    echo ""
    read -p "지금 활성화하시겠습니까? (y/N) " -n 1 -r
    echo ""
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        sudo loginctl enable-linger "$USER"
        echo "  Linger enabled."
    fi
fi

echo ""
echo "========================================"
echo " Setup complete!"
echo "========================================"
echo ""
echo "설치 경로: $INSTALL_DIR"
echo "기존 ~/distributed-ai 폴더는 건드리지 않았습니다."
echo ""
echo "보안 주의사항:"
echo "  - API는 127.0.0.1:8080에만 바인딩됨 (외부 직접 공개 안 됨)."
echo "  - 외부 공개가 필요하면 Nginx/Caddy 리버스 프록시 + HTTPS 사용."
echo "  - 8080 포트를 인증 없이 인터넷에 직접 열지 마세요."
echo ""
echo "서비스 시작:"
echo "  systemctl --user start distributed-ai-alpha.service"
echo "  systemctl --user start distributed-ai-alpha-node.service"
echo ""
echo "상태 확인:"
echo "  systemctl --user status distributed-ai-alpha.service"
echo "  curl http://127.0.0.1:8080/v1/models"
echo "  curl http://127.0.0.1:8080/network/status | python3 -m json.tool"
echo ""
echo "대시보드에서 Oracle Node 확인:"
echo "  - 온라인 Node 수에 Oracle Node 포함 여부"
echo "  - CPU/RAM/SSD 자원 집계에 반영 여부"
echo ""
echo "로그:"
echo "  journalctl --user -u distributed-ai-alpha.service -f"
echo "  journalctl --user -u distributed-ai-alpha-node.service -f"
echo ""
