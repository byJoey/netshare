#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# NetShare 运维入口；只管理本项目的文件、服务和独立 nftables 表。
SCRIPT_PATH="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/$(basename -- "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(dirname -- "$SCRIPT_PATH")"
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
EXPECTED_SHA=""
PURGE=false
NO_START=false
LOG_LINES=100
PACKAGE_DIR=""

say() { printf '%s\n' "$*"; }
die() { printf '错误：%s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'EOF'
NetShare 安装与管理

  bash manage.sh install controller --bundle /私密目录/controller [--source 发行目录或.tar.gz] [--no-start]
  bash manage.sh install agent --bundle /私密目录/agents/节点ID [--source 发行目录或.tar.gz] [--no-start]
  netshare-manage upgrade controller|agent --source 发行目录或.tar.gz
  netshare-manage start|stop|restart|status|logs controller|agent
  netshare-manage entry
  netshare-manage path rotate
  netshare-manage backup controller|agent
  netshare-manage uninstall controller|agent [--purge]

--source 也支持显式 HTTPS 地址，此时必须加 --sha256 64位校验和。
本地压缩包若提供 --sha256 也会校验。安装默认从脚本所在发行目录读取。
已有配置、证书和映射不会被安装/升级覆盖。卸载默认保留配置与状态；
--purge 才删除该角色的数据，备份始终保留。脚本不安装/修改 Caddy 或云私网。
不带参数时打开中文菜单；输入 help 查看本说明。
EOF
}

cleanup() {
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then rm -rf -- "$TMP_DIR"; fi
  if [[ -n "$LOCK_DIR" && -d "$LOCK_DIR" ]]; then rmdir -- "$LOCK_DIR" 2>/dev/null || true; fi
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

# 归档只解到本次 mktemp 目录，不接受链接、设备或穿越路径。
prepare_package() {
  local supplied="${SOURCE:-$SCRIPT_DIR}" archive=""
  if [[ "$supplied" == https://* ]]; then
    [[ "$EXPECTED_SHA" =~ ^[a-fA-F0-9]{64}$ ]] || die "远程包必须提供 --sha256"
    command -v curl >/dev/null 2>&1 || die "下载需要 curl"
    archive="$TMP_DIR/release.tar.gz"
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 300 --output "$archive" "$supplied" || die "下载失败"
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
import ipaddress,json,socket,sys,time,urllib.request
c=json.load(open(sys.argv[1]));host,port=c['listen'].rsplit(':',1);host=host.strip('[]')
if not ipaddress.ip_address(host).is_loopback:raise SystemExit('拒绝探测非回环控制器')
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
service_health() {
  systemctl is-active --quiet "$(unit_for "$ROLE")"
  if [[ "$ROLE" == controller ]]; then controller_health; fi
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
  local cfg service existing=false
  cfg="$(config_for "$ROLE")";service="$(unit_for "$ROLE")"
  if [[ -f "$cfg" ]]; then existing=true; else validate_bundle; fi
  if [[ "$ROLE" == agent ]]; then for tool in nft ip ss; do command -v "$tool" >/dev/null || die "节点缺少依赖：$tool（Ubuntu：apt-get install nftables iproute2）"; done; fi
  lock_mutation;snapshot_begin "$ROLE"
  atomic_copy "$PACKAGE_DIR/netshare" "$(path_for /usr/local/bin/netshare)" 0755
  atomic_copy "$PACKAGE_DIR/manage.sh" "$(path_for /usr/local/sbin/netshare-manage)" 0755
  if ! $existing; then install_bundle; fi
  if [[ "$ROLE" == controller ]]; then ensure_random_path; fi
  write_unit "$ROLE"
  systemctl daemon-reload
  if ! $NO_START; then systemctl enable "$service";systemctl start "$service";service_health; fi
  TX_STARTED=false
  say "安装完成：$ROLE。配置和映射已保留；备份：$TX_DIR"
  if $NO_START; then say "服务尚未启动。"; fi
  if [[ "$ROLE" == controller ]]; then say "使用 netshare-manage entry 查看管理入口。HTTPS 请按 deploy/Caddyfile 配置；脚本不会覆盖已有站点。"; fi
}
upgrade_role() {
  role_check "$ROLE";[[ -f "$(config_for "$ROLE")" ]] || die "该角色未安装"
  [[ -n "$SOURCE" ]] || die "升级必须显式给出 --source"
  prepare_package;lock_mutation;snapshot_begin "$ROLE"
  local service
  service="$(unit_for "$ROLE")"
  atomic_copy "$PACKAGE_DIR/netshare" "$(path_for /usr/local/bin/netshare)" 0755
  atomic_copy "$PACKAGE_DIR/manage.sh" "$(path_for /usr/local/sbin/netshare-manage)" 0755
  if [[ "$ROLE" == controller ]]; then ensure_random_path; fi
  if $TX_ACTIVE; then systemctl start "$service";service_health; fi
  TX_STARTED=false
  say "升级完成：$ROLE。原启停状态已保留；备份：$TX_DIR"
  say "控制器与节点共用程序文件，另一角色的运行进程保持原样；需要更新其进程时单独重启该角色。"
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
    python3 - "$cfgdir" "$statedir" <<'PY'
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
  say "卸载完成：$ROLE。备份：$TX_DIR"
  if ! $PURGE; then say "配置、证书和映射状态已保留，可直接重新安装。"; fi
  if [[ "$ROLE" == agent ]]; then say "只移除了 NetShare 独立规则表；未重置全局 ip_forward，也未清空连接跟踪。"; fi
}

menu() {
  [[ -t 0 ]] || { usage;exit 0; }
  say 'NetShare 管理';say '1 安装控制台  2 安装节点  3 升级  4 查看状态  5 查看日志'
  say '6 启动  7 停止  8 重启  9 查看入口  10 更换入口  11 备份  12 卸载  0 退出'
  local choice action role bundle source
  read -r -p '选择：' choice
  case "$choice" in
    0) exit 0 ;; 9) main entry;return ;; 10) main path rotate;return ;;
    1|2) if [[ "$choice" == 1 ]];then role=controller;else role=agent;fi
      read -r -p '对应叶子凭据目录：' bundle;read -r -p '发行目录或tar.gz（回车使用脚本目录）：' source
      main install "$role" --bundle "$bundle" --source "${source:-$SCRIPT_DIR}";return ;;
    3) read -r -p '角色 controller/agent：' role;read -r -p '发行目录或tar.gz：' source;main upgrade "$role" --source "$source";return ;;
    4) action=status ;;5)action=logs;;6)action=start;;7)action=stop;;8)action=restart;;11)action=backup;;12)action=uninstall;;*)die '无效选择';;
  esac
  read -r -p '角色 controller/agent：' role
  main "$action" "$role"
}
main() {
  local command="${1:-menu}"
  if [[ "$command" == help || "$command" == --help || "$command" == -h ]];then usage;return;fi
  if [[ "$command" == menu ]];then menu;return;fi
  shift
  if [[ "$command" == path ]];then [[ "${1:-}" == rotate ]] || die '用法：netshare-manage path rotate';shift
  elif [[ "$command" != entry ]];then ROLE="${1:-}";[[ $# -gt 0 ]] && shift;role_check "$ROLE";fi
  while [[ $# -gt 0 ]];do
    case "$1" in
      --source|--bundle|--sha256|--lines) [[ $# -ge 2 ]] || die "缺少 $1 的值";case "$1" in --source)SOURCE="$2";;--bundle)BUNDLE="$2";;--sha256)EXPECTED_SHA="$2";;--lines)LOG_LINES="$2";;esac;shift 2 ;;
      --purge)PURGE=true;shift;;--no-start)NO_START=true;shift;;*)die "未知参数：$1";;
    esac
  done
  setup_environment
  case "$command" in
    install)install_role;;upgrade)upgrade_role;;path)rotate_path;;backup)backup_role;;uninstall)uninstall_role;;
    entry) [[ -f "$(config_for controller)" && -x "$(path_for /usr/local/bin/netshare)" ]] || die '控制台未安装';"$(path_for /usr/local/bin/netshare)" access-path show --config "$(config_for controller)" ;;
    start|stop|restart)lock_mutation;systemctl "$command" "$(unit_for "$ROLE")";if [[ "$command" != stop ]];then service_health;fi;;
    status)systemctl status "$(unit_for "$ROLE")" --no-pager;;
    logs)[[ "$LOG_LINES" =~ ^[0-9]+$ && "$LOG_LINES" -gt 0 && "$LOG_LINES" -le 10000 ]] || die '--lines应为1–10000';command -v journalctl >/dev/null || die '缺少journalctl';journalctl -u "$(unit_for "$ROLE")" -n "$LOG_LINES" --no-pager;;
    *)die "未知命令：$command";;
  esac
}
main "$@"
