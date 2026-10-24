#!/bin/bash
# Build 可达书柜 (mangagate) into a single multi-architecture fnOS package.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
GO="$ROOT/tools/go/bin/go"
PKG="$ROOT/pkg/mangagate"
GO_VERSION="1.23.4"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-off}"
export GOFLAGS="${GOFLAGS:--mod=mod}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少依赖：$1"; exit 1; }; }
need curl
need tar
need fnpack

if [ ! -x "$GO" ]; then
    echo "==> 下载 Go ${GO_VERSION}（仅项目内，不安装到系统）"
    case "$(uname -m)" in
        x86_64|amd64) GOARCH_DL=amd64 ;;
        aarch64|arm64) GOARCH_DL=arm64 ;;
        *) echo "不支持的开发机架构：$(uname -m)"; exit 1 ;;
    esac
    mkdir -p "$ROOT/tools"
    curl -fL --retry 3 -o "$ROOT/tools/go.tar.gz" \
        "https://dl.google.com/go/go${GO_VERSION}.linux-${GOARCH_DL}.tar.gz"
    tar -xzf "$ROOT/tools/go.tar.gz" -C "$ROOT/tools"
    rm -f "$ROOT/tools/go.tar.gz"
fi

echo "==> 生成图标"
# gen_icon lives outside the src module but needs x/image, so run it from src/.
( cd "$ROOT/src" && "$GO" run ../tools/gen_icon.go --out "$PKG" --src ../tools/icon.png )

echo "==> 编译 arm64 / amd64（静态链接）"
mkdir -p "$PKG/app/server"
(
    cd "$ROOT/src"
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO" build -trimpath -ldflags '-s -w' \
        -o "$PKG/app/server/mangagate-arm64" .
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO" build -trimpath -ldflags '-s -w' \
        -o "$PKG/app/server/mangagate-amd64" .
)

chmod 755 "$PKG/app/server/"* "$PKG/cmd/"*
chmod 644 "$PKG/manifest" "$PKG/config/"* "$PKG/wizard/"* "$PKG/app/ui/config" "$PKG"/ICON*.PNG

echo "==> 打包 fpk"
(
    cd "$PKG"
    fnpack build
)

echo
echo "完成：$PKG/mangagate.fpk"
ls -lh "$PKG/mangagate.fpk"
