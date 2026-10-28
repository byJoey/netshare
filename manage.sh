#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# NetShare 运维入口；只管理本项目的文件、服务和独立 nftables 表。
SCRIPT_PATH="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/$(basename -- "${BASH_SOURCE[0]}")"
ROOT_PREFIX=""
TEST_MODE=false
TX_DIR=""
TX_ROLE=""
TX_ACTIVE=false
TX_ENABLED=false
TX_STARTED=false
TMP_DIR=""
LOCK_DIR=""
ROLE=""
SOURCE=""
BUNDLE=""
PAIRING=""
HOST=""
PORT=""
LISTEN=""
AUTO_HOST=false
NODE_ID=""
NODE_NAME=""
PUBLIC_IP=""
PRIVATE_IP=""
PUBLIC_INTERFACE=""
PRIVATE_INTERFACE=""
AUTO_BOOTSTRAP=false
EXPECTED_SHA=""
PURGE=false
NO_START=false
LOG_LINES=100
PACKAGE_DIR=""
OFFICIAL_REPO="byJoey/netshare"

say() { printf '%s\n' "$*"; }
die() { printf '错误：%s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'EOF'
NetShare 安装与管理

  bash manage.sh install controller [--port 18440] [--host 访问IPv4] [--source 发行目录或.tar.gz]
  bash manage.sh install controller --bundle /私密目录/controller [--source 发行目录或.tar.gz] [--no-start]
  bash manage.sh install agent --pairing /节点配对包.tar.gz [--source 发行目录或.tar.gz]
  bash manage.sh install agent --bundle /私密目录/agents/节点ID [--source 发行目录或.tar.gz] [--no-start]
  netshare-manage upgrade controller|agent [--source 发行目录或.tar.gz]
  netshare-manage start|stop|restart|status|logs controller|agent
  netshare-manage entry
  netshare-manage node add --id ID --name 名称 --public-ip IP --private-ip IP --public-interface 网卡 --private-interface 网卡
  netshare-manage path rotate
  netshare-manage backup controller|agent
  netshare-manage uninstall controller|agent [--purge]

安装/升级不填 --source 时，自动下载本项目 GitHub 最新稳定 Release 并校验 SHA256。
--source 也支持显式 HTTPS 地址，此时必须加 --sha256 64位校验和。
本地压缩包若提供 --sha256 也会校验；离线使用本目录时填 --source .。
已有配置、证书和映射不会被安装/升级覆盖。卸载默认保留配置与状态；
--purge 才删除该角色的数据，备份始终保留。首次控制台安装默认直接通过 HTTP IP:端口访问；
已有控制台保留原监听和 HTTPS 配置。脚本不改已有 Web 站点、网卡、云私网或全局防火墙。
不带参数时打开中文菜单；输入 help 查看本说明。
EOF
}

cleanup() {
  local result=$?
  trap - ERR
  if [[ "$result" -ne 0 && "$TX_STARTED" == true ]];then rollback || say "退出后的自动回退未完整完成，请按备份恢复。" >&2;fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then rm -rf -- "$TMP_DIR"; fi
  if [[ -n "$LOCK_DIR" && -d "$LOCK_DIR" ]]; then rmdir -- "$LOCK_DIR" 2>/dev/null || true; fi
  return "$result"
}
trap cleanup EXIT

# 只允许测试工具指定带哨兵的沙箱；正式运行永远使用标准系统路径。
setup_environment() {
  if [[ "${NETSHARE_TEST_MODE:-}" == 1 ]]; then
    [[ -n "${NETSHARE_TEST_ROOT:-}" && "$NETSHARE_TEST_ROOT" == /* && -f "$NETSHARE_TEST_ROOT/.netshare-test-root" ]] || die "测试根目录缺少显式哨兵"
    ROOT_PREFIX="$(cd -- "$NETSHARE_TEST_ROOT" && pwd -P)"
    [[ "$ROOT_PREFIX" != / ]] || die "测试根目录不能是 /"
    TEST_MODE=true
  else
    [[ -z "${NETSHARE_TEST_ROOT:-}" ]] || die "测试根目录只能配合显式测试模式"
    [[ "$(uname -s)" == Linux ]] || die "此脚本面向 Linux systemd 主机"
    [[ "${EUID:-$(id -u)}" == 0 ]] || die "请以 root 运行"
    [[ -d /run/systemd/system ]] || die "当前主机未运行 systemd"
  fi
  if ! $TEST_MODE; then
    local -a missing_packages=()
    command -v python3 >/dev/null 2>&1 || missing_packages+=(python3)
    command -v curl >/dev/null 2>&1 || missing_packages+=(curl)
    [[ -s /etc/ssl/certs/ca-certificates.crt ]] || missing_packages+=(ca-certificates)
    command -v ip >/dev/null 2>&1 || missing_packages+=(iproute2)
    if [[ "$ROLE" == agent ]]; then
      command -v nft >/dev/null 2>&1 || missing_packages+=(nftables)
      if ! command -v ip >/dev/null 2>&1 || ! command -v ss >/dev/null 2>&1;then missing_packages+=(iproute2);fi
    fi
    if [[ ${#missing_packages[@]} -gt 0 ]];then
      command -v apt-get >/dev/null 2>&1 || die "缺少安装依赖且当前系统没有 apt-get，请先安装 python3/curl；节点还需 nftables/iproute2"
      say "正在安装缺少的系统依赖：${missing_packages[*]}"
      apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${missing_packages[@]}"
    fi
  fi
  for tool in python3 systemctl install tar cp mv chmod mkdir rm date mktemp; do command -v "$tool" >/dev/null 2>&1 || die "缺少依赖：$tool"; done
  if $TEST_MODE; then
    assert_no_symlink "$ROOT_PREFIX/test-bin"
    local resolved
    resolved="$(command -v systemctl)"
    [[ "$resolved" == "$ROOT_PREFIX/test-bin/"* && ! -L "$resolved" ]] || die "测试模式必须使用沙箱 test-bin 中的 systemctl 模拟命令"
    assert_no_symlink "$resolved"
    if [[ "$ROLE" == agent ]]; then
      local nft_path
      nft_path="$(command -v nft || true)"
      [[ "$nft_path" == "$ROOT_PREFIX/test-bin/"* && ! -L "$nft_path" ]] || die "测试模式必须使用沙箱 test-bin 中的 nft 模拟命令"
      assert_no_symlink "$nft_path"
    fi
  fi
  case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) die "只支持 Linux amd64、arm64" ;; esac
  TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/netshare-manage.XXXXXX")"
  TMP_DIR="$(cd -- "$TMP_DIR" && pwd -P)"
}
path_for() { printf '%s%s' "$ROOT_PREFIX" "$1"; }
config_for() { if [[ "$1" == controller ]]; then path_for /etc/netshare/controller.json; else path_for /etc/netshare-agent/agent.json; fi; }
unit_for() { if [[ "$1" == controller ]]; then printf netshare; else printf netshare-agent; fi; }
role_check() { [[ "$1" == controller || "$1" == agent ]] || die "角色必须是 controller 或 agent"; }
assert_no_symlink() {
  python3 - "$1" <<'PY'
import os, pathlib, sys
p=pathlib.Path(sys.argv[1]).absolute()
for part in [p,*p.parents]:
    if part.is_symlink(): raise SystemExit('拒绝通过符号链接写入受管路径')
PY
}
lock_mutation() {
  local parent
  parent="$(path_for /run/netshare-manage)"
  assert_no_symlink "$parent"
  mkdir -p -- "$parent"
  local candidate="$parent/lock"
  mkdir -- "$candidate" 2>/dev/null || die "另一项管理操作正在运行；若上次被强制结束，确认没有管理进程后移除 /run/netshare-manage/lock"
  LOCK_DIR="$candidate"
}
atomic_copy() {
  local source="$1" destination="$2" mode="$3"
  assert_no_symlink "$destination"
  python3 - "$source" "$destination" "$mode" <<'PY'
import os, pathlib, shutil, sys, tempfile
src,dst,mode=sys.argv[1:]; dst=pathlib.Path(dst)
if not pathlib.Path(src).is_file() or pathlib.Path(src).is_symlink(): raise SystemExit('源文件必须是普通文件')
dst.parent.mkdir(parents=True,exist_ok=True)
fd,tmp=tempfile.mkstemp(prefix='.'+dst.name+'.',dir=dst.parent)
try:
    with os.fdopen(fd,'wb') as out, open(src,'rb') as inp:
        shutil.copyfileobj(inp,out);out.flush();os.fsync(out.fileno())
    os.chmod(tmp,int(mode,8));os.replace(tmp,dst)
finally:
    if os.path.exists(tmp):os.unlink(tmp)
PY
}
ensure_private_dir() { assert_no_symlink "$1"; mkdir -p -- "$1"; python3 -c 'import os,sys;os.chmod(sys.argv[1],0o700)' "$1"; }

download_https() {
  command -v curl >/dev/null 2>&1 || die "下载需要 curl"
  local url="$1" destination="$2" limit="$3"
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --connect-timeout 10 --max-time 300 --max-filesize "$limit" \
    --output "$destination" "$url"
}
latest_official_release() {
  say "正在查询 NetShare 官方最新稳定发行版…" >&2
  download_https "https://api.github.com/repos/$OFFICIAL_REPO/releases/latest" "$TMP_DIR/release.json" 1048576 || die "查询官方 Release 失败，请稍后重试，或用 --source 指定已验证发行包"
  local tag filename
  tag="$(python3 - "$TMP_DIR/release.json" <<'PYCODE'
import json,re,sys
with open(sys.argv[1]) as f:value=json.load(f)
tag=value.get('tag_name','')
if value.get('draft') or value.get('prerelease') or not isinstance(tag,str) or len(tag)>64 or not re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)',tag):
    raise SystemExit('官方最新发行不是受支持的稳定版本')
print(tag)
PYCODE
)"
  filename="netshare_${tag}_linux_${ARCH}.tar.gz"
  download_https "https://github.com/$OFFICIAL_REPO/releases/download/$tag/SHA256SUMS" "$TMP_DIR/SHA256SUMS" 1048576 || die "下载官方校验表失败"
  EXPECTED_SHA="$(python3 - "$TMP_DIR/SHA256SUMS" "$filename" <<'PYCODE'
import pathlib,re,sys
matches=[]
for line in pathlib.Path(sys.argv[1]).read_text().splitlines():
    parts=line.split()
    if len(parts)==2 and parts[1] in (sys.argv[2], '*'+sys.argv[2]):
        if not re.fullmatch(r'[0-9a-fA-F]{64}',parts[0]):raise SystemExit('目标包校验和格式无效')
        matches.append(parts[0].lower())
if len(matches)!=1:raise SystemExit('校验表必须包含唯一且精确匹配的目标包记录')
print(matches[0])
PYCODE
)"
  SOURCE="https://github.com/$OFFICIAL_REPO/releases/download/$tag/$filename"
  say "已选择官方发行版 ${tag}，架构 ${ARCH}；下载后会校验 SHA256。" >&2
}

# 归档只解到本次 mktemp 目录，不接受链接、设备或穿越路径。
prepare_package() {
  if [[ -z "$SOURCE" ]]; then latest_official_release; fi
  local supplied="$SOURCE" archive=""
  if [[ "$supplied" == https://* ]]; then
    [[ "$EXPECTED_SHA" =~ ^[a-fA-F0-9]{64}$ ]] || die "远程包必须提供 --sha256"
    archive="$TMP_DIR/release.tar.gz"
    download_https "$supplied" "$archive" 157286400 || die "下载失败"
  elif [[ "$supplied" == *://* ]]; then
    die "下载地址只允许 HTTPS"
  elif [[ -d "$supplied" && ! -L "$supplied" ]]; then
    PACKAGE_DIR="$(cd -- "$supplied" && pwd)"
  elif [[ -f "$supplied" && ! -L "$supplied" ]]; then archive="$supplied"
  else die "发行目录或压缩包不存在"; fi
  if [[ -n "$archive" ]]; then
    python3 - "$archive" "$TMP_DIR/unpacked" "$EXPECTED_SHA" <<'PY'
import hashlib, pathlib, sys, tarfile
archive,dest,expected=sys.argv[1:];p=pathlib.Path(archive)
if p.stat().st_size>150*1024*1024:raise SystemExit('发行包超过150MiB')
if expected and (len(expected)!=64 or hashlib.sha256(p.read_bytes()).hexdigest().lower()!=expected.lower()):raise SystemExit('发行包SHA256不一致')
out=pathlib.Path(dest);out.mkdir(mode=0o700)
with tarfile.open(p,'r:gz') as tf:
    members=tf.getmembers()
    if len(members)>4096 or sum(m.size for m in members)>300*1024*1024:raise SystemExit('发行包体积或文件数异常')
    roots=set()
    for m in members:
        x=pathlib.PurePosixPath(m.name)
        if x.is_absolute() or '..' in x.parts or not x.parts or not(m.isfile() or m.isdir()):raise SystemExit('发行包含不安全路径或文件类型')
        roots.add(x.parts[0])
    if len(roots)!=1:raise SystemExit('发行包必须只有一个顶层目录')
    tf.extractall(out,members=members)
    root=out/roots.pop()
    if not root.is_dir():raise SystemExit('发行包根目录无效')
    (out/'package-root.txt').write_text(str(root))
PY
    PACKAGE_DIR="$(cat "$TMP_DIR/unpacked/package-root.txt")"
  elif [[ -n "$EXPECTED_SHA" ]]; then die "目录来源不适用 --sha256，请校验压缩包"; fi
  for item in netshare manage.sh deploy/netshare.service deploy/netshare-agent.service; do
    [[ -f "$PACKAGE_DIR/$item" && ! -L "$PACKAGE_DIR/$item" ]] || die "发行包缺少普通文件：$item"
  done
  if ! $TEST_MODE; then
    python3 - "$PACKAGE_DIR/netshare" "$ARCH" <<'PY'
import pathlib,struct,sys
b=pathlib.Path(sys.argv[1]).read_bytes()[:64]
if len(b)<64 or b[:4]!=b'\x7fELF' or b[4]!=2 or b[5]!=1:raise SystemExit('二进制不是受支持的64位Linux ELF')
if struct.unpack('<H',b[18:20])[0]!={'amd64':62,'arm64':183}[sys.argv[2]]:raise SystemExit('发行包CPU架构与当前主机不符')
PY
  fi
  atomic_copy "$PACKAGE_DIR/netshare" "$TMP_DIR/package-binary" 0755
  local version
  version="$("$TMP_DIR/package-binary" version)"
  [[ "$version" =~ ^NetShare\ [0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]] || die "无法识别发行包版本"
}

snapshot_begin() {
  TX_ROLE="$1"
  if [[ -f "$(config_for "$TX_ROLE")" ]]; then validate_config_layout "$(config_for "$TX_ROLE")" "$TX_ROLE"; fi
  local service stamp cfgdir statedir
  service="$(unit_for "$TX_ROLE")"
  TX_ACTIVE=false;TX_ENABLED=false
  if systemctl is-active --quiet "$service"; then TX_ACTIVE=true; fi
  if systemctl is-enabled --quiet "$service"; then TX_ENABLED=true; fi
  local stop_before_snapshot="${2:-true}"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)-$(python3 -c 'import secrets;print(secrets.token_hex(4))')"
  TX_DIR="$(path_for /var/backups/netshare)/$stamp-$TX_ROLE"
  ensure_private_dir "$TX_DIR"
  TX_STARTED=true
  # 停止写入后再快照，回退不能覆盖停机前刚保存的用户变更。
  if [[ "$stop_before_snapshot" == true ]] && $TX_ACTIVE; then systemctl stop "$service"; fi
  if [[ "$TX_ROLE" == controller ]]; then cfgdir=/etc/netshare;statedir=/var/lib/netshare; else cfgdir=/etc/netshare-agent;statedir=/var/lib/netshare-agent; fi
  python3 - "$ROOT_PREFIX" "$TX_DIR" "$cfgdir" "$statedir" "$(unit_for "$TX_ROLE")" <<'PY'
import json,os,pathlib,shutil,sys
root,dest,cfg,state,unit=sys.argv[1:];target=pathlib.Path(dest);items=['/usr/local/bin/netshare','/usr/local/sbin/netshare-manage',cfg,state,'/etc/systemd/system/'+unit+'.service']
if unit=='netshare-agent':items+=['/etc/sysctl.d/90-netshare-agent.conf']
else:items+=['/var/lib/netshare-bootstrap']
records=[]
for index,item in enumerate(items):
    p=pathlib.Path(root+item)
    for q in [p,*p.parents]:
        if q.is_symlink():raise SystemExit('受管路径包含符号链接，拒绝继续')
    record={'path':item,'exists':p.exists(),'backup':str(index)}
    if p.exists():
        if not(p.is_file() or p.is_dir()):raise SystemExit('受管路径类型不支持')
        if p.is_dir():
            if any(x.is_symlink() for x in p.rglob('*')):raise SystemExit('配置或状态包含符号链接，拒绝继续')
            shutil.copytree(p,target/str(index),copy_function=shutil.copy2)
        else:shutil.copy2(p,target/str(index))
        entries=[p,*p.rglob('*')] if p.is_dir() else [p]
        record['owners']=[{'relative':str(x.relative_to(p)),'uid':x.stat().st_uid,'gid':x.stat().st_gid,'mode':x.stat().st_mode&0o777} for x in entries]
    records.append(record)
(target/'manifest.json').write_text(json.dumps(records))
os.chmod(target/'manifest.json',0o600)
PY
  if [[ "$TX_ROLE" == agent ]] && command -v nft >/dev/null; then
    if nft list table ip netshare > "$TX_DIR/nft-before.txt" 2>/dev/null; then
      printf 'present\n' > "$TX_DIR/nft-state"
    else
      printf 'absent\n' > "$TX_DIR/nft-state"
      rm -f -- "$TX_DIR/nft-before.txt"
    fi
  fi
  TX_STARTED=true
}
rollback() {
  [[ "$TX_STARTED" == true && -n "$TX_DIR" ]] || return 0
  TX_STARTED=false
  local service
  service="$(unit_for "$TX_ROLE")"
  systemctl stop "$service" >/dev/null 2>&1 || true
  python3 - "$ROOT_PREFIX" "$TX_DIR" <<'PY' || return 1
import json,os,pathlib,shutil,sys
root,dest=sys.argv[1:];base=pathlib.Path(dest)
if not (base/'manifest.json').exists():raise SystemExit(0)
for r in json.loads((base/'manifest.json').read_text()):
    p=pathlib.Path(root+r['path'])
    if p.is_symlink():raise SystemExit('回退目标变成符号链接，已停止自动恢复')
    if p.is_dir():shutil.rmtree(p)
    elif p.exists():p.unlink()
    if not r['exists']:continue
    p.parent.mkdir(parents=True,exist_ok=True);src=base/r['backup']
    if src.is_dir():shutil.copytree(src,p,copy_function=shutil.copy2)
    else:shutil.copy2(src,p)
    for own in r['owners']:
        q=p/own['relative'] if own['relative']!='.' else p
        os.chmod(q,own['mode'])
        if os.geteuid()==0:os.chown(q,own['uid'],own['gid'])
PY
  if [[ "$TX_ROLE" == agent && -f "$TX_DIR/nft-state" ]]; then
    if nft list table ip netshare >/dev/null 2>&1; then nft delete table ip netshare >/dev/null 2>&1 || true; fi
    if [[ "$(cat "$TX_DIR/nft-state")" == present ]]; then nft -f "$TX_DIR/nft-before.txt" >/dev/null 2>&1 || return 1; fi
  fi
  systemctl daemon-reload >/dev/null 2>&1 || true
  if $TX_ENABLED; then systemctl enable "$service" >/dev/null 2>&1 || true; else systemctl disable "$service" >/dev/null 2>&1 || true; fi
  if $TX_ACTIVE; then systemctl start "$service" >/dev/null 2>&1 || return 1;systemctl is-active --quiet "$service" || return 1; fi
  say "操作未完成，程序、配置和原服务状态已请求回退；备份：$TX_DIR" >&2
}
on_error() { local result=$?; trap - ERR; rollback || say "自动回退未完整完成，请按备份恢复。" >&2; exit "$result"; }
trap on_error ERR
on_signal() { trap - ERR INT TERM; rollback || say "中断后的自动回退未完整完成，请按备份恢复。" >&2; exit "$1"; }
trap 'on_signal 130' INT
trap 'on_signal 143' TERM

write_unit() {
  local role="$1" dest
  dest="$(path_for "/etc/systemd/system/$(unit_for "$role").service")"
  # 使用本脚本附带的固定模板，避免从任意发行包执行未经检查的服务内容。
  if [[ "$role" == controller ]]; then
    cat > "$TMP_DIR/unit" <<'EOF'
[Unit]
Description=NetShare 映射管理台
Wants=network-online.target
After=network-online.target
[Service]
Type=simple
User=netshare
Group=netshare
ExecStart=/usr/local/bin/netshare serve --config /etc/netshare/controller.json
Restart=on-failure
RestartSec=3
TimeoutStopSec=20
UMask=0077
StateDirectory=netshare
StateDirectoryMode=0700
NoNewPrivileges=true
CapabilityBoundingSet=
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
ReadWritePaths=/var/lib/netshare /etc/netshare
[Install]
WantedBy=multi-user.target
EOF
  else
    cat > "$TMP_DIR/unit" <<'EOF'
[Unit]
Description=NetShare 私网映射节点
Wants=network-online.target
After=network-online.target
[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/netshare agent --config /etc/netshare-agent/agent.json
Restart=on-failure
RestartSec=3
TimeoutStopSec=20
UMask=0077
StateDirectory=netshare-agent
StateDirectoryMode=0700
NoNewPrivileges=true
CapabilityBoundingSet=CAP_NET_ADMIN
AmbientCapabilities=CAP_NET_ADMIN
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=false
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
ReadWritePaths=/var/lib/netshare-agent /etc/sysctl.d /proc/sys/net/ipv4
[Install]
WantedBy=multi-user.target
EOF
  fi
  atomic_copy "$TMP_DIR/unit" "$dest" 0644
}
validate_config_layout() {
  python3 - "$1" "$2" <<'PYCODE'
import json,sys
c=json.load(open(sys.argv[1]));role=sys.argv[2]
expected={'state_file':'/var/lib/netshare/state.json','ca_file':'/etc/netshare/ca.pem','cert_file':'/etc/netshare/controller.pem','key_file':'/etc/netshare/controller-key.pem'} if role=='controller' else {'state_file':'/var/lib/netshare-agent/state.json','ca_file':'/etc/netshare-agent/ca.pem','cert_file':'/etc/netshare-agent/server.pem','key_file':'/etc/netshare-agent/server-key.pem'}
if any(c.get(k)!=v for k,v in expected.items()):raise SystemExit('管理脚本只支持标准配置/状态布局；自定义路径请按README手动维护')
PYCODE
}
validate_bundle() {
  [[ -n "$BUNDLE" && -d "$BUNDLE" && ! -L "$BUNDLE" ]] || die "安装新角色需要 --bundle 对应的叶子凭据目录"
  python3 - "$BUNDLE" "$ROLE" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);role=sys.argv[2]
files=['controller.json','ca.pem','controller.pem','controller-key.pem'] if role=='controller' else ['agent.json','ca.pem','server.pem','server-key.pem']
for name in files:
    f=p/name
    if f.is_symlink() or not f.is_file() or f.stat().st_size>1024*1024:raise SystemExit('凭据目录缺少普通文件或文件异常：'+name)
c=json.loads((p/files[0]).read_text())
expected={'state_file':'/var/lib/netshare/state.json','ca_file':'/etc/netshare/ca.pem','cert_file':'/etc/netshare/controller.pem','key_file':'/etc/netshare/controller-key.pem'} if role=='controller' else {'state_file':'/var/lib/netshare-agent/state.json','ca_file':'/etc/netshare-agent/ca.pem','cert_file':'/etc/netshare-agent/server.pem','key_file':'/etc/netshare-agent/server-key.pem'}
if any(c.get(k)!=v for k,v in expected.items()):raise SystemExit('凭据配置路径与标准安装位置不符；请先按说明准备bundle')
PY
}
# 首次安装自行生成凭据；CA 资料只交给 root 管理，控制台服务仅得到叶子凭据。
detect_access_host() {
  HOST="$(python3 - <<'PYCODE'
import ipaddress,json,subprocess
addresses=[];default_devices=[]
try:
    routes=json.loads(subprocess.check_output(['ip','-j','route','show','default'],stderr=subprocess.DEVNULL,timeout=5))
    default_devices=[route.get('dev') for route in routes if route.get('dev')]
    for interface in json.loads(subprocess.check_output(['ip','-j','address','show'],stderr=subprocess.DEVNULL,timeout=5)):
        for address in interface.get('addr_info',[]):
            if address.get('family')!='inet':continue
            value=ipaddress.ip_address(address.get('local',''))
            if not value.is_loopback and not value.is_link_local and not value.is_unspecified and not value.is_multicast:
                addresses.append((value.is_global,interface['ifname'] in default_devices,str(value)))
except (OSError,ValueError,subprocess.SubprocessError):pass
if addresses:print(max(addresses)[2])
PYCODE
)"
  [[ -n "$HOST" ]] || die "未找到可用 IPv4，请用 --host 访问IP 指定本机地址"
  AUTO_HOST=true
}

initialize_controller() {
  if [[ -z "$PORT" ]];then
    if [[ -t 0 ]];then read -r -p '控制台端口 [18440]：' PORT || die "已取消";fi
    PORT="${PORT:-18440}"
  fi
  [[ "$PORT" =~ ^[1-9][0-9]{0,4}$ && "$PORT" -le 65535 && "$PORT" != 22 ]] || die "控制台端口必须为 1–65535 且不能使用 SSH 22"
  [[ -n "$HOST" ]] || detect_access_host
  HOST="$(python3 - "$HOST" <<'PYCODE'
import ipaddress,sys
try:value=ipaddress.ip_address(sys.argv[1])
except ValueError:raise SystemExit('访问地址需要 IPv4，不要带协议、端口或路径')
if value.version!=4 or value.is_unspecified or value.is_multicast or str(value)!=sys.argv[1]:raise SystemExit('访问地址需要规范 IPv4，不能使用 0.0.0.0 或组播地址')
print(value)
PYCODE
)"
  LISTEN="${LISTEN:-0.0.0.0:$PORT}"
  say "正在生成管理员密码、随机入口和节点配对所需凭据…"
  "$TMP_DIR/package-binary" init --standalone --external-url "http://$HOST:$PORT" --listen "$LISTEN" --dir "$TMP_DIR/bootstrap" >/dev/null
  BUNDLE="$TMP_DIR/bootstrap/controller"
  AUTO_BOOTSTRAP=true
  validate_bundle
  if ! $TEST_MODE;then
    python3 - "$BUNDLE/controller.json" <<'PYCODE'
import json,socket,sys
cfg=json.load(open(sys.argv[1]));host,port=cfg['listen'].rsplit(':',1)
try:
    with socket.socket(socket.AF_INET,socket.SOCK_STREAM) as sock:sock.bind((host,int(port)))
except OSError:raise SystemExit('控制台监听端口已被占用或地址不在本机，请换端口后重试；不会停止其他服务')
PYCODE
  fi
}

install_bootstrap() {
  local target
  target="$(path_for /var/lib/netshare-bootstrap)"
  assert_no_symlink "$target"
  [[ ! -e "$target" ]] || die "已有初始化资料但控制台配置缺失；请先恢复配置，不自动替换原有 CA"
  ensure_private_dir "$target"
  python3 - "$TMP_DIR/bootstrap" "$target" <<'PYCODE'
import os,pathlib,shutil,sys
source,target=map(pathlib.Path,sys.argv[1:])
for item in source.rglob('*'):
    if item.is_symlink() or not (item.is_file() or item.is_dir()):raise SystemExit('初始化资料包含不受支持的文件')
shutil.copytree(source,target,dirs_exist_ok=True)
for item in [target,*target.rglob('*')]:os.chmod(item,0o700 if item.is_dir() else 0o600)
PYCODE
}

prepare_pairing() {
  [[ -n "$PAIRING" && -f "$PAIRING" && ! -L "$PAIRING" ]] || die "节点首次安装需要控制台生成的配对包；请用 --pairing /路径/节点配对包.tar.gz"
  python3 - "$PAIRING" "$TMP_DIR/pairing" <<'PYCODE'
import pathlib,sys,tarfile
archive,dest=map(pathlib.Path,sys.argv[1:]);names={'agent.json','ca.pem','server.pem','server-key.pem'}
if archive.stat().st_size>4*1024*1024:raise SystemExit('节点配对包超过4MiB')
with tarfile.open(archive,'r:gz') as tf:
    members=tf.getmembers()
    if len(members)>8 or sum(x.size for x in members)>4*1024*1024:raise SystemExit('节点配对包内容异常')
    roots=set();files=set()
    for member in members:
        path=pathlib.PurePosixPath(member.name)
        if path.is_absolute() or '..' in path.parts or not path.parts or not(member.isfile() or member.isdir()):raise SystemExit('节点配对包包含不安全路径或文件类型')
        roots.add(path.parts[0])
        if member.isdir():
            if len(path.parts)!=1:raise SystemExit('节点配对包目录层级无效')
        else:
            if len(path.parts)!=2 or path.parts[1] not in names or path.parts[1] in files:raise SystemExit('节点配对包必须只含本节点的4个叶子文件')
            files.add(path.parts[1])
    if len(roots)!=1 or files!=names:raise SystemExit('节点配对包不完整')
    dest.mkdir(mode=0o700);tf.extractall(dest,members=members)
    (dest/'bundle-root.txt').write_text(str(dest/roots.pop()))
PYCODE
  BUNDLE="$(cat "$TMP_DIR/pairing/bundle-root.txt")"
  validate_bundle
}

show_controller_delivery() {
  say '管理入口：'
  "$(path_for /usr/local/bin/netshare)" access-path show --config "$(config_for controller)"
  if $AUTO_BOOTSTRAP; then
    if $AUTO_HOST;then
      say '访问 IP 来自本机网卡探测。NAT 主机首次安装请用 --host 公网IP；已安装则按文档修改 external_url 后重启，仅替换浏览器地址无法登录。'
    fi
    say '管理员账号：admin'
    say "首次密码已保存：$(path_for /var/lib/netshare-bootstrap/controller/admin-password.txt)"
    if [[ -t 1 ]]; then
      printf '首次密码：'
      cat -- "$(path_for /var/lib/netshare-bootstrap/controller/admin-password.txt)"
    fi
  else
    say '登录账号、密码、映射和现有配置已保留。'
  fi
}

install_bundle() {
  local cfgdir state files name
  if [[ "$ROLE" == controller ]]; then
    cfgdir="$(path_for /etc/netshare)";state="$(path_for /var/lib/netshare)"
    files='controller.json ca.pem controller.pem controller-key.pem'
    if ! $TEST_MODE; then
      if ! getent passwd netshare >/dev/null; then useradd --system --home-dir /var/lib/netshare --shell /usr/sbin/nologin netshare; fi
    fi
  else cfgdir="$(path_for /etc/netshare-agent)";state="$(path_for /var/lib/netshare-agent)";files='agent.json ca.pem server.pem server-key.pem'; fi
  ensure_private_dir "$cfgdir";ensure_private_dir "$state"
  # 仅允许拷贝明确列出的叶子文件；authority 和首次明文密码永不部署。
  for name in $files; do atomic_copy "$BUNDLE/$name" "$cfgdir/$name" 0600; done
  if [[ "$ROLE" == controller ]] && ! $TEST_MODE; then chown -R netshare:netshare -- "$cfgdir" "$state"; fi
}
controller_health() {
  python3 - "$(config_for controller)" <<'PY'
import ipaddress,json,socket,subprocess,sys,time,urllib.request
with open(sys.argv[1]) as config:c=json.load(config)
host,port=c['listen'].rsplit(':',1);host=host.strip('[]')
address=ipaddress.ip_address(host)
if host=='0.0.0.0':host='127.0.0.1'
elif not address.is_loopback:
    if address.version!=4:raise SystemExit('拒绝探测非本机控制器')
    try:
        interfaces=json.loads(subprocess.check_output(['ip','-j','-4','address','show'],stderr=subprocess.DEVNULL,timeout=5))
        local=any(item.get('family')=='inet' and item.get('local')==host for interface in interfaces for item in interface.get('addr_info',[]))
    except (OSError,ValueError,subprocess.SubprocessError):raise SystemExit('无法确认控制器监听地址属于本机，拒绝探测')
    if not local:raise SystemExit('控制器监听地址不属于本机，拒绝探测远程地址')
url='http://'+('['+host+']' if ':' in host else host)+':'+port+c.get('access_path','')+'/healthz'
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}));ok=False
for _ in range(20):
    try:
        with opener.open(url,timeout=2) as response:ok=response.status==200 and json.load(response).get('ok') is True
        if ok:break
    except Exception:pass
    time.sleep(.25)
if not ok:raise SystemExit('控制器健康检查失败（未输出管理入口）')
PY
}
preflight_agent() {
  local candidate="$1" cfg="$2" credentials="$3"
  local -a args
  if $TEST_MODE && [[ "${NETSHARE_TEST_REAL_BINARY:-}" != 1 ]];then return 0;fi
  args=(check-agent --config "$cfg" --credential-dir "$credentials")
  if [[ -f "$(path_for /var/lib/netshare-agent/state.json)" ]];then args+=(--state-file "$(path_for /var/lib/netshare-agent/state.json)");fi
  if $TEST_MODE;then args+=(--skip-address-check);fi
  "$candidate" "${args[@]}" >/dev/null
}

agent_health() {
  if $TEST_MODE;then systemctl is-active --quiet netshare-agent;return;fi
  python3 - "$(config_for agent)" <<'PYCODE'
import json,os,pathlib,socket,subprocess,sys,time
cfg=json.load(open(sys.argv[1]));host,port=cfg['listen'].rsplit(':',1)
expected=socket.inet_aton(host)[::-1].hex().upper()+':'+format(int(port),'04X')
def ready():
    if subprocess.run(['systemctl','is-active','--quiet','netshare-agent'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL).returncode:return False
    pid=subprocess.check_output(['systemctl','show','--property=MainPID','--value','netshare-agent'],text=True).strip()
    if not pid.isdecimal() or int(pid)<1:return False
    sockets=set()
    try:
        for fd in (pathlib.Path('/proc')/pid/'fd').iterdir():
            try:
                value=os.readlink(fd)
                if value.startswith('socket:['):sockets.add(value[8:-1])
            except FileNotFoundError:pass
        with open('/proc/net/tcp') as table:
            for line in table:
                parts=line.split()
                if len(parts)>9 and parts[1]==expected and parts[3]=='0A' and parts[9] in sockets:return True
    except (OSError,subprocess.SubprocessError):return False
    return False
stable=0
for _ in range(20):
    stable=stable+1 if ready() else 0
    if stable>=3:break
    time.sleep(.5)
else:raise SystemExit('节点尚未稳定监听配置私网地址，安装已停止；请核对节点安装包、私网 IP 和网卡')
PYCODE
}

service_health() {
  systemctl is-active --quiet "$(unit_for "$ROLE")"
  if [[ "$ROLE" == controller ]]; then controller_health;else agent_health;fi
}
ensure_random_path() {
  local cfg
  cfg="$(config_for controller)"
  if python3 - "$cfg" <<'PY'
import json,sys
raise SystemExit(0 if not json.load(open(sys.argv[1])).get('access_path') else 1)
PY
  then "$(path_for /usr/local/bin/netshare)" access-path rotate --config "$cfg" >/dev/null; fi
}
install_role() {
  role_check "$ROLE"
  prepare_package
  local cfg service existing=false replace_pairing=false
  cfg="$(config_for "$ROLE")";service="$(unit_for "$ROLE")"
  if [[ "$ROLE" == agent && -n "$PAIRING" ]];then
    prepare_pairing
    replace_pairing=true
    if [[ -f "$cfg" ]];then
      existing=true
      python3 - "$cfg" "$BUNDLE/agent.json" "$(path_for /etc/netshare-agent/ca.pem)" "$BUNDLE/ca.pem" <<'PYCODE'
import json,pathlib,sys
before,after,old_ca,new_ca=map(pathlib.Path,sys.argv[1:])
if json.loads(before.read_text()).get('node',{}).get('id')!=json.loads(after.read_text()).get('node',{}).get('id'):raise SystemExit('节点安装包 ID 与本机已有节点不符，拒绝覆盖')
if old_ca.read_bytes()!=new_ca.read_bytes():raise SystemExit('节点安装包 CA 与本机已有节点不符，拒绝覆盖')
PYCODE
    fi
  elif [[ -f "$cfg" ]]; then
    existing=true
  elif [[ -n "$BUNDLE" ]]; then
    validate_bundle
  elif [[ "$ROLE" == controller ]]; then
    initialize_controller
  else
    prepare_pairing
  fi
  if [[ "$ROLE" == agent ]]; then
    for tool in nft ip ss; do command -v "$tool" >/dev/null || die "节点缺少依赖：${tool}（Ubuntu：apt-get install nftables iproute2）"; done
    if $replace_pairing || ! $existing;then preflight_agent "$TMP_DIR/package-binary" "$BUNDLE/agent.json" "$BUNDLE";else preflight_agent "$TMP_DIR/package-binary" "$cfg" "$(path_for /etc/netshare-agent)";fi
  fi
  lock_mutation;snapshot_begin "$ROLE"
  atomic_copy "$PACKAGE_DIR/netshare" "$(path_for /usr/local/bin/netshare)" 0755
  atomic_copy "$PACKAGE_DIR/manage.sh" "$(path_for /usr/local/sbin/netshare-manage)" 0755
  if ! $existing || $replace_pairing; then
    if $AUTO_BOOTSTRAP; then install_bootstrap; fi
    install_bundle
  fi
  if [[ "$ROLE" == controller ]]; then ensure_random_path; fi
  write_unit "$ROLE"
  systemctl daemon-reload
  if ! $NO_START; then
    systemctl enable "$service";systemctl start "$service";service_health
  fi
  TX_STARTED=false
  say "安装完成：${ROLE}。配置和映射已保留；备份：$TX_DIR"
  if $NO_START; then say "服务尚未启动。"; fi
  if [[ "$ROLE" == controller ]]; then show_controller_delivery; fi
}
upgrade_role() {
  role_check "$ROLE";[[ -f "$(config_for "$ROLE")" ]] || die "该角色未安装"
  prepare_package
  if [[ "$ROLE" == agent ]];then preflight_agent "$TMP_DIR/package-binary" "$(config_for agent)" "$(path_for /etc/netshare-agent)";fi
  lock_mutation;snapshot_begin "$ROLE"
  local service
  service="$(unit_for "$ROLE")"
  atomic_copy "$PACKAGE_DIR/netshare" "$(path_for /usr/local/bin/netshare)" 0755
  atomic_copy "$PACKAGE_DIR/manage.sh" "$(path_for /usr/local/sbin/netshare-manage)" 0755
  if [[ "$ROLE" == controller ]]; then ensure_random_path; fi
  if $TX_ACTIVE; then systemctl start "$service";service_health; fi
  TX_STARTED=false
  say "升级完成：${ROLE}。原启停状态已保留；备份：$TX_DIR"
  say "控制器与节点共用程序文件，另一角色的运行进程保持原样；需要更新其进程时单独重启该角色。"
}
prompt_value() {
  local target="$1" label="$2" default="${3:-}" value
  [[ -t 0 ]] || die "缺少 $target，请提供节点信息参数"
  if [[ -n "$default" ]];then read -r -p "$label [$default]：" value || die "已取消";else read -r -p "$label：" value || die "已取消";fi
  printf -v "$target" '%s' "${value:-$default}"
}

detect_node_defaults() {
  python3 - "$TMP_DIR/node-defaults" <<'PYCODE'
import ipaddress,json,pathlib,subprocess,sys
public_ip=private_ip=public_interface=private_interface=''
try:
    interfaces=json.loads(subprocess.check_output(['ip','-j','address','show'],stderr=subprocess.DEVNULL,timeout=5))
    networks=[ipaddress.ip_network(value) for value in ('10.0.0.0/8','172.16.0.0/12','192.168.0.0/16')]
    for interface in interfaces:
        for address in interface.get('addr_info',[]):
            if address.get('family')!='inet':continue
            value=ipaddress.ip_address(address.get('local',''))
            if any(value in network for network in networks):
                if not private_ip:private_ip,private_interface=str(value),interface['ifname']
            elif value.is_global and not public_ip:public_ip,public_interface=str(value),interface['ifname']
except (OSError,ValueError,subprocess.SubprocessError):pass
pathlib.Path(sys.argv[1]).write_text('\n'.join((public_ip,private_ip,public_interface,private_interface))+'\n')
PYCODE
  { read -r DEFAULT_PUBLIC_IP;read -r DEFAULT_PRIVATE_IP;read -r DEFAULT_PUBLIC_INTERFACE;read -r DEFAULT_PRIVATE_INTERFACE; } < "$TMP_DIR/node-defaults"
}

add_node() {
  ROLE=controller
  local cfg authority output pairings stamp
  local DEFAULT_PUBLIC_IP="" DEFAULT_PRIVATE_IP="" DEFAULT_PUBLIC_INTERFACE="" DEFAULT_PRIVATE_INTERFACE=""
  local -a add_args
  cfg="$(config_for controller)"
  authority="$(path_for /var/lib/netshare-bootstrap/authority)"
  [[ -f "$cfg" && -x "$(path_for /usr/local/bin/netshare)" ]] || die "请先安装控制台"
  [[ -f "$authority/ca-key.pem" && -f "$authority/ca.pem" ]] || die "本控制台未保留节点签发资料；请恢复 /var/lib/netshare-bootstrap/authority 后重试，不会重新生成 CA"
  if [[ -t 0 ]];then
    say '填写要添加的那台机器；私网必须已开通，两张网卡不能相同。'
    say '自动探测的默认值属于本机；添加另一台机器时请填写那台机器的值。'
    detect_node_defaults
  fi
  [[ -n "$NODE_ID" ]] || prompt_value NODE_ID '节点 ID（例如 node-a）'
  [[ -n "$NODE_NAME" ]] || prompt_value NODE_NAME '节点名称' "$NODE_ID"
  [[ -n "$PUBLIC_IP" ]] || prompt_value PUBLIC_IP '该节点公网 IPv4' "$DEFAULT_PUBLIC_IP"
  [[ -n "$PRIVATE_IP" ]] || prompt_value PRIVATE_IP '该节点私网 IPv4' "$DEFAULT_PRIVATE_IP"
  [[ -n "$PUBLIC_INTERFACE" ]] || prompt_value PUBLIC_INTERFACE '公网网卡名称（例如 eth0）' "$DEFAULT_PUBLIC_INTERFACE"
  [[ -n "$PRIVATE_INTERFACE" ]] || prompt_value PRIVATE_INTERFACE '私网网卡名称（例如 eth1）' "$DEFAULT_PRIVATE_INTERFACE"
  lock_mutation;snapshot_begin controller
  output="$TMP_DIR/node-output"
  add_args=(node add --config "$cfg" --authority "$authority" --id "$NODE_ID" --name "$NODE_NAME" --public-ip "$PUBLIC_IP" --private-ip "$PRIVATE_IP" --public-interface "$PUBLIC_INTERFACE" --private-interface "$PRIVATE_INTERFACE" --dir "$output")
  if $TEST_MODE;then add_args+=(--credential-dir "$(path_for /etc/netshare)");fi
  "$(path_for /usr/local/bin/netshare)" "${add_args[@]}" >/dev/null
  validate_config_layout "$output/controller.json" controller
  stamp="$(date -u +%Y%m%dT%H%M%SZ)-$(python3 -c 'import secrets;print(secrets.token_hex(4))')"
  pairings="$(path_for /var/lib/netshare-bootstrap/pairings)/$stamp"
  ensure_private_dir "$pairings"
  python3 - "$output" "$pairings" <<'PYCODE'
import os,pathlib,sys,tarfile
output,dest=map(pathlib.Path,sys.argv[1:]);allowed={'agent.json','ca.pem','server.pem','server-key.pem'}
for directory in sorted((output/'agents').iterdir()):
    if directory.is_symlink() or not directory.is_dir() or {f.name for f in directory.iterdir()}!=allowed:raise SystemExit('签发结果不完整')
    archive=dest/('netshare-node-'+directory.name+'.tar.gz')
    with tarfile.open(archive,'w:gz') as tf:
        for name in sorted(allowed):
            leaf=directory/name
            if leaf.is_symlink() or not leaf.is_file():raise SystemExit('节点凭据必须为普通文件')
            info=tf.gettarinfo(str(leaf),arcname=directory.name+'/'+name);info.uid=info.gid=0;info.uname=info.gname='';info.mode=0o600
            with leaf.open('rb') as content:tf.addfile(info,content)
    os.chmod(archive,0o600)
PYCODE
  python3 - "$output/controller.json" "$cfg" <<'PYCODE'
import os,pathlib,sys,tempfile
source,target=map(pathlib.Path,sys.argv[1:]);original=target.stat();fd,tmp=tempfile.mkstemp(dir=target.parent,prefix='.'+target.name+'.')
try:
    with os.fdopen(fd,'wb') as stream:stream.write(source.read_bytes());stream.flush();os.fsync(stream.fileno())
    os.chmod(tmp,original.st_mode&0o777)
    if os.geteuid()==0:os.chown(tmp,original.st_uid,original.st_gid)
    os.replace(tmp,target)
finally:
    if os.path.exists(tmp):os.unlink(tmp)
PYCODE
  if $TX_ACTIVE;then systemctl start netshare;controller_health;fi
  TX_STARTED=false
  say "节点已添加，控制台原有映射与启停状态保留。节点安装包目录：$pairings"
  say '请把每个安装包安全复制到对应节点；包含私钥，勿上传公开网盘。'
  say '所有已有节点也要导入这批更新包，才能允许与新节点互通；每台只用自己的包，历史映射状态会保留。'
  say '在对应节点执行：netshare-manage install agent --pairing /安全路径/netshare-node-节点ID.tar.gz'
}

rotate_path() {
  ROLE=controller
  local cfg
  cfg="$(config_for controller)";[[ -f "$cfg" ]] || die "控制台未安装"
  lock_mutation;snapshot_begin controller
  "$(path_for /usr/local/bin/netshare)" access-path rotate --config "$cfg" >/dev/null
  if $TX_ACTIVE; then systemctl start netshare;controller_health; fi
  TX_STARTED=false
  say "入口已更换，旧入口失效；使用 netshare-manage entry 查看。配置备份：$TX_DIR"
}
backup_role() {
  role_check "$ROLE";lock_mutation;snapshot_begin "$ROLE" false;TX_STARTED=false
  say "备份完成：$TX_DIR"
  say "备份含私钥，请保持目录权限并离机保管；本次未停止服务。"
}

uninstall_role() {
  role_check "$ROLE"
  local service cfgdir statedir other
  service="$(unit_for "$ROLE")"
  if $PURGE && [[ -t 0 ]]; then
    local response
    read -r -p "将删除 $ROLE 的配置、证书和映射状态；输入 PURGE 继续：" response
    [[ "$response" == PURGE ]] || die "已取消"
  fi
  if [[ "$ROLE" == agent ]]; then command -v nft >/dev/null || die "缺少 nft，无法确认并清理本工具的规则表"; fi
  lock_mutation;snapshot_begin "$ROLE"
  systemctl disable --now "$service"
  if [[ "$ROLE" == agent ]]; then
    if nft list table ip netshare >/dev/null 2>&1; then nft delete table ip netshare; fi
    rm -f -- "$(path_for /etc/sysctl.d/90-netshare-agent.conf)"
    cfgdir="$(path_for /etc/netshare-agent)";statedir="$(path_for /var/lib/netshare-agent)";other=netshare
  else cfgdir="$(path_for /etc/netshare)";statedir="$(path_for /var/lib/netshare)";other=netshare-agent; fi
  rm -f -- "$(path_for "/etc/systemd/system/$service.service")"
  systemctl daemon-reload
  if $PURGE; then
    # 删除目标固定为本角色标准目录，绝不接受用户传入的任意目录。
    local -a purge_paths=("$cfgdir" "$statedir")
    if [[ "$ROLE" == controller ]]; then purge_paths+=("$(path_for /var/lib/netshare-bootstrap)");fi
    python3 - "${purge_paths[@]}" <<'PY'
import pathlib,shutil,sys
for name in sys.argv[1:]:
    p=pathlib.Path(name)
    if p.is_symlink():raise SystemExit('拒绝清理符号链接目录')
    if p.exists():shutil.rmtree(p)
PY
  fi
  if [[ ! -f "$(path_for "/etc/systemd/system/$other.service")" ]] && ! systemctl is-active --quiet "$other"; then rm -f -- "$(path_for /usr/local/bin/netshare)"; fi
  # 保留管理脚本，便于查看备份和重新安装；不删除系统用户或 Caddy 站点。
  TX_STARTED=false
  say "卸载完成：${ROLE}。备份：$TX_DIR"
  if ! $PURGE; then say "配置、证书和映射状态已保留，可直接重新安装。"; fi
  if [[ "$ROLE" == agent ]]; then say "只移除了 NetShare 独立规则表；未重置全局 ip_forward，也未清空连接跟踪。"; fi
}

menu_role() {
  local choice
  while :; do
    say '选择角色：1 控制台  2 节点  0 返回'
    read -r -p '角色 [1/2]：' choice || return 1
    case "$choice" in 1|controller) MENU_ROLE=controller;return 0;;2|agent)MENU_ROLE=agent;return 0;;0)return 1;;*)say '请输入 1 或 2。';;esac
  done
}
menu_action() {
  # 独立进程保留严格错误与事务回退；操作失败不会让菜单进程退出。
  local result
  if bash "$SCRIPT_PATH" "$@"; then result=0;say '操作完成。';else result=$?;say "本次操作未完成（退出码 ${result}），可以继续选择其他操作。";fi
}
menu() {
  [[ -t 0 ]] || { usage;return 0; }
  setup_environment
  local choice action pairing purge_choice response
  local -a args
  while :; do
    say '';say 'NetShare 管理'
    say '1 安装控制台  2 安装节点  3 升级  4 查看状态  5 查看日志'
    say '6 启动  7 停止  8 重启  9 查看入口  10 更换入口  11 备份  12 卸载'
    say '13 添加节点 / 生成节点安装包  0 退出'
    read -r -p '选择：' choice || break
    args=()
    case "$choice" in
      0)break;;
      1)
        args=(install controller)
        ;;
      2)
        args=(install agent)
        say '先在控制台的管理菜单生成节点安装包，安全复制到本机。'
        read -r -p '节点安装包路径（已安装可回车保留现有配置，0 返回）：' pairing || break
        [[ "$pairing" == 0 ]] && continue
        [[ -n "$pairing" ]] && args+=(--pairing "$pairing")
        ;;
      3)
        menu_role || continue
        args=(upgrade "$MENU_ROLE")
        ;;
      13)args=(node add);;
      9)args=(entry);;
      10)
        say '将更换控制台入口，旧书签会失效；登录密码、映射与私网保持原样。'
        read -r -p '确认更换？[y/N]：' response || break
        [[ "$response" == y || "$response" == Y ]] || continue
        args=(path rotate)
        ;;
      4|5|6|7|8|11|12)
        menu_role || continue
        case "$choice" in 4)action=status;;5)action=logs;;6)action=start;;7)action=stop;;8)action=restart;;11)action=backup;;12)action=uninstall;;esac
        args=("$action" "$MENU_ROLE")
        if [[ "$choice" == 7 ]];then
          read -r -p "确认停止 $MENU_ROLE 服务？[y/N]：" response || break
          [[ "$response" == y || "$response" == Y ]] || continue
        fi
        if [[ "$choice" == 12 ]];then
          say '1 保留配置、证书和映射状态（默认）  2 彻底清理该角色数据  0 返回'
          read -r -p '卸载方式 [1]：' purge_choice || break
          case "$purge_choice" in ''|1) ;;2)args+=(--purge);;0)continue;;*)say '无效选择，未卸载。';continue;;esac
          say "将停止并卸载 ${MENU_ROLE}；节点卸载还会移除本工具的映射规则表，备份保留。"
          if [[ "$purge_choice" == 2 ]];then
            read -r -p '确认彻底清理？输入 PURGE：' response || break
            [[ "$response" == PURGE ]] || continue
            # 非终端输入避免卸载子进程重复询问；本菜单已经明确确认。
            if bash "$SCRIPT_PATH" "${args[@]}" </dev/null;then say '操作完成。';else say '本次卸载未完成，可继续选择其他操作。';fi
            continue
          else
            read -r -p '确认卸载并保留配置？[y/N]：' response || break
            [[ "$response" == y || "$response" == Y ]] || continue
          fi
        fi
        ;;
      *)say '无效选择，请重新输入。';continue;;
    esac
    menu_action "${args[@]}"
  done
  say '已退出 NetShare 管理。'
}

main() {
  local command="${1:-menu}"
  if [[ "$command" == help || "$command" == --help || "$command" == -h ]];then usage;return;fi
  if [[ "$command" == menu ]];then menu;return;fi
  shift
  if [[ "$command" == node ]];then [[ "${1:-}" == add ]] || die '用法：netshare-manage node add';shift;ROLE=controller
  elif [[ "$command" == path ]];then [[ "${1:-}" == rotate ]] || die '用法：netshare-manage path rotate';shift
  elif [[ "$command" != entry ]];then ROLE="${1:-}";[[ $# -gt 0 ]] && shift;role_check "$ROLE";fi
  while [[ $# -gt 0 ]];do
    case "$1" in
      --source|--bundle|--pairing|--host|--port|--listen|--sha256|--lines|--id|--name|--public-ip|--private-ip|--public-interface|--private-interface) [[ $# -ge 2 ]] || die "缺少 $1 的值";case "$1" in --source)SOURCE="$2";;--bundle)BUNDLE="$2";;--pairing)PAIRING="$2";;--host)HOST="$2";;--port)PORT="$2";;--listen)LISTEN="$2";;--id)NODE_ID="$2";;--name)NODE_NAME="$2";;--public-ip)PUBLIC_IP="$2";;--private-ip)PRIVATE_IP="$2";;--public-interface)PUBLIC_INTERFACE="$2";;--private-interface)PRIVATE_INTERFACE="$2";;--sha256)EXPECTED_SHA="$2";;--lines)LOG_LINES="$2";;esac;shift 2 ;;
      --purge)PURGE=true;shift;;--no-start)NO_START=true;shift;;*)die "未知参数：$1";;
    esac
  done
  setup_environment
  case "$command" in
    install)install_role;;upgrade)upgrade_role;;node)add_node;;path)rotate_path;;backup)backup_role;;uninstall)uninstall_role;;
    entry) [[ -f "$(config_for controller)" && -x "$(path_for /usr/local/bin/netshare)" ]] || die '控制台未安装';"$(path_for /usr/local/bin/netshare)" access-path show --config "$(config_for controller)" ;;
    start|stop|restart)lock_mutation;systemctl "$command" "$(unit_for "$ROLE")";if [[ "$command" != stop ]];then service_health;fi;;
    status)systemctl status "$(unit_for "$ROLE")" --no-pager;;
    logs)[[ "$LOG_LINES" =~ ^[0-9]+$ && "$LOG_LINES" -gt 0 && "$LOG_LINES" -le 10000 ]] || die '--lines应为1–10000';command -v journalctl >/dev/null || die '缺少journalctl';journalctl -u "$(unit_for "$ROLE")" -n "$LOG_LINES" --no-pager;;
    *)die "未知命令：$command";;
  esac
}
main "$@"
