#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# 远程 Bash 入口只负责下载并校验正式管理脚本，再交给中文菜单。
die() { printf '错误：%s\n' "$*" >&2; exit 1; }
[[ "$(uname -s)" == Linux ]] || die "NetShare 安装器需要使用 systemd 的 Linux。"
command -v curl >/dev/null 2>&1 || die "请先安装 curl。"
if command -v sha256sum >/dev/null 2>&1; then
  HASH_TOOL=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  HASH_TOOL=(shasum -a 256)
else
  die "请先安装 sha256sum（coreutils）。"
fi

INSTALL_TMP="$(mktemp -d "${TMPDIR:-/tmp}/netshare-install.XXXXXX")"
trap 'rm -rf -- "$INSTALL_TMP"' EXIT
fetch() {
  curl --fail --silent --show-error --location --proto '=https' \
    --proto-redir '=https' --connect-timeout 10 --max-time 120 "$@"
}

printf '正在查询 NetShare 最新正式发行版…\n'
RELEASE_URL="$(fetch --output /dev/null --write-out '%{url_effective}' https://github.com/byJoey/netshare/releases/latest)"
[[ "$RELEASE_URL" =~ ^https://github\.com/byJoey/netshare/releases/tag/(v[0-9]+\.[0-9]+\.[0-9]+)$ ]] || die "未找到可用的正式发行版。"
RELEASE_TAG="${BASH_REMATCH[1]}"
DOWNLOAD_BASE="https://github.com/byJoey/netshare/releases/download/$RELEASE_TAG"
fetch --output "$INSTALL_TMP/SHA256SUMS" "$DOWNLOAD_BASE/SHA256SUMS"
fetch --output "$INSTALL_TMP/manage.sh" "$DOWNLOAD_BASE/manage.sh"

EXPECTED_SHA=""
MATCHES=0
while read -r checksum filename extra; do
  if [[ "$filename" == manage.sh && -z "$extra" ]]; then
    [[ "$checksum" =~ ^[a-fA-F0-9]{64}$ ]] || die "管理脚本校验清单无效。"
    EXPECTED_SHA="$checksum"
    MATCHES=$((MATCHES + 1))
  fi
done < "$INSTALL_TMP/SHA256SUMS"
[[ "$MATCHES" == 1 ]] || die "发行清单缺少唯一的 manage.sh 校验和。"
ACTUAL_SHA="$("${HASH_TOOL[@]}" "$INSTALL_TMP/manage.sh")"
ACTUAL_SHA="${ACTUAL_SHA%% *}"
[[ "$ACTUAL_SHA" == "$EXPECTED_SHA" ]] || die "管理脚本 SHA256 校验失败，未执行。"
printf '已验证 %s，进入中文管理菜单。\n' "$RELEASE_TAG"
if [[ "${EUID:-$(id -u)}" == 0 ]]; then
  bash "$INSTALL_TMP/manage.sh" "$@"
else
  command -v sudo >/dev/null 2>&1 || die "请用 root 执行，或安装 sudo。"
  sudo bash "$INSTALL_TMP/manage.sh" "$@"
fi
