#!/usr/bin/env bash
set -euo pipefail

# 在项目根目录生成两种 Linux 架构的独立发行包。
BUILD_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BUILD_VERSION="${VERSION:-v1.2.0}"
if [[ ! "$BUILD_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]]; then
  echo "VERSION 格式应为 v1.2.0" >&2
  exit 1
fi
BUILD_TMP="$(mktemp -d "${TMPDIR:-/tmp}/netshare-build.XXXXXX")"
trap 'rm -rf -- "$BUILD_TMP"' EXIT
mkdir -p -- "$BUILD_ROOT/dist"
cd -- "$BUILD_ROOT"
# 前端只在构建时使用 Node，运行时资源随 Go 二进制嵌入。
npm --prefix frontend ci --no-audit --no-fund
npm --prefix frontend run build
for BUILD_ARCH in amd64 arm64; do
  BUILD_NAME="netshare_${BUILD_VERSION}_linux_${BUILD_ARCH}"
  mkdir -p -- "$BUILD_TMP/$BUILD_NAME"
  CGO_ENABLED=0 GOOS=linux GOARCH="$BUILD_ARCH" go build -trimpath -buildvcs=false -ldflags="-s -w -X netshare/internal/model.Version=${BUILD_VERSION#v}" -o "$BUILD_TMP/$BUILD_NAME/netshare" .
  cp -- README.md LICENSE "$BUILD_TMP/$BUILD_NAME/"
  cp -- manage.sh "$BUILD_TMP/$BUILD_NAME/"
  cp -R -- frontend/licenses "$BUILD_TMP/$BUILD_NAME/"
  cp -R -- deploy "$BUILD_TMP/$BUILD_NAME/"
  tar -C "$BUILD_TMP" -czf "$BUILD_ROOT/dist/$BUILD_NAME.tar.gz" "$BUILD_NAME"
done
cd -- "$BUILD_ROOT/dist"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "netshare_${BUILD_VERSION}_linux_amd64.tar.gz" "netshare_${BUILD_VERSION}_linux_arm64.tar.gz" > SHA256SUMS
else
  shasum -a 256 "netshare_${BUILD_VERSION}_linux_amd64.tar.gz" "netshare_${BUILD_VERSION}_linux_arm64.tar.gz" > SHA256SUMS
fi
echo "已生成 dist/ 下的 Linux amd64、arm64 包与 SHA256SUMS。"
