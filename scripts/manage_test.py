#!/usr/bin/env python3
"""隔离管理脚本生命周期验证：真实文件操作，systemd/nft 仅使用沙箱模拟。"""
import hashlib
import http.server
import io
import json
import os
import errno
import pty
import select
import socket
import time
import types
from pathlib import Path
import shutil
import stat
import subprocess
import tarfile
import tempfile
import threading
import unittest
from unittest import mock

PROJECT = Path(__file__).resolve().parents[1]
MANAGER = PROJECT / "manage.sh"

SYSTEMCTL = r'''#!/usr/bin/env python3
import json,os,pathlib,sys
r=pathlib.Path(os.environ['NETSHARE_TEST_ROOT']);p=r/'services.json'
s=json.loads(p.read_text()) if p.exists() else {}
a=sys.argv[1:];cmd=a[0];args=[x for x in a[1:] if not x.startswith('-')];unit=args[0] if args else ''
with (r/'commands.log').open('a') as f:f.write('systemctl '+' '.join(a)+'\n')
x=s.setdefault(unit,{'active':False,'enabled':False})
if cmd=='is-active':raise SystemExit(0 if x['active'] else 3)
if cmd=='is-enabled':raise SystemExit(0 if x['enabled'] else 1)
if cmd=='stop':
 x['active']=False
 marker=r/'save-on-stop.json'
 if unit=='netshare' and marker.exists():
  event=json.loads(marker.read_text());marker.unlink()
  for key,rel in [('state','var/lib/netshare/state.json'),('config','etc/netshare/controller.json')]:
   target=r/rel
   if key in event:
    data=json.loads(target.read_text()) if target.exists() else {};data.update(event[key]);target.write_text(json.dumps(data));target.chmod(0o600)
elif cmd=='start':
 fail=r/'fail-start-once'
 if fail.exists():fail.unlink();raise SystemExit(1)
 x['active']=True
elif cmd=='restart':x['active']=True
elif cmd=='enable':x['enabled']=True
elif cmd=='disable':
 x['enabled']=False
 if '--now' in a:x['active']=False
elif cmd=='status':print(json.dumps(x))
elif cmd!='daemon-reload':raise SystemExit(2)
p.write_text(json.dumps(s))
'''

FAKE_BINARY = r'''#!/usr/bin/env python3
import json,os,pathlib,secrets,sys,tempfile
r=pathlib.Path(os.environ['NETSHARE_TEST_ROOT']);a=sys.argv[1:]
if a==['version']:print('NetShare FAKE_VERSION');raise SystemExit(0)
if a[:2]==['signer','init']:
 p=pathlib.Path(a[a.index('--dir')+1]);p.mkdir(parents=True,exist_ok=True)
 for name in ['ca.pem','ca-key.pem']:
  if not (p/name).exists():(p/name).write_text('隔离签发占位资料')
  (p/name).chmod(0o600)
 raise SystemExit(0)

if a[:2] not in [['access-path','show'],['access-path','rotate']]:raise SystemExit(2)
p=pathlib.Path(a[a.index('--config')+1]);c=json.loads(p.read_text())
if a[1]=='rotate':
 services=json.loads((r/'services.json').read_text())
 if services.get('netshare',{}).get('active'):raise SystemExit('控制器运行中不应轮换')
 with (r/'commands.log').open('a') as f:f.write('binary rotate\n')
 c['access_path']='/ns-'+secrets.token_hex(16)
 fd,tmp=tempfile.mkstemp(dir=p.parent)
 with os.fdopen(fd,'w') as f:json.dump(c,f)
 os.chmod(tmp,p.stat().st_mode&0o777);os.replace(tmp,p)
print(c['external_url'].rstrip('/')+c.get('access_path','')+'/')
'''

NFT = r'''#!/usr/bin/env python3
import os,pathlib,sys
r=pathlib.Path(os.environ['NETSHARE_TEST_ROOT']);p=r/'own-nft.txt';a=sys.argv[1:]
with (r/'commands.log').open('a') as f:f.write('nft '+' '.join(a)+'\n')
if a==['list','table','ip','netshare']:
 if not p.exists():raise SystemExit(1)
 print(p.read_text());raise SystemExit(0)
if a==['delete','table','ip','netshare']:
 p.unlink(missing_ok=True);raise SystemExit(0)
if len(a)==2 and a[0]=='-f':p.write_text(pathlib.Path(a[1]).read_text());raise SystemExit(0)
raise SystemExit('不允许触碰其他规则表')
'''


CURL = r'''#!/usr/bin/env python3
import json,os,pathlib,shutil,sys
r=pathlib.Path(os.environ['NETSHARE_TEST_ROOT']);a=sys.argv[1:];url=a[-1]
with (r/'downloads.log').open('a') as f:f.write(url+'\n')
if '--proto' not in a or a[a.index('--proto')+1]!='=https' or '--proto-redir' not in a or a[a.index('--proto-redir')+1]!='=https':raise SystemExit('下载没有HTTPS限制')
dest=pathlib.Path(a[a.index('--output')+1]);cfg=json.loads((r/'release-fixture.json').read_text())
base='https://github.com/byJoey/netshare/releases/download/'+cfg['tag']+'/'
if url=='https://api.github.com/repos/byJoey/netshare/releases/latest':src=cfg['metadata']
elif url==base+'SHA256SUMS':src=cfg['sums']
elif url==base+cfg['filename']:src=cfg['archive']
else:raise SystemExit('意外的下载目标')
shutil.copyfile(src,dest)
'''


class Lifecycle(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.real_build = tempfile.TemporaryDirectory(prefix="netshare-manager-real-binary-")
        cls.real_binary = Path(cls.real_build.name) / "netshare"
        subprocess.run(["go", "build", "-o", str(cls.real_binary), "."], cwd=PROJECT, check=True, capture_output=True, timeout=120)

    @classmethod
    def tearDownClass(cls):
        cls.real_build.cleanup()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="netshare-manager-test-")
        self.base = Path(self.temp.name).resolve()
        self.root = self.base / "root"
        self.root.mkdir()
        (self.root / ".netshare-test-root").touch()
        (self.root / "services.json").write_text("{}")
        self.tools = self.root / "test-bin"
        self.tools.mkdir()
        self.env = {**os.environ, "NETSHARE_TEST_MODE": "1", "NETSHARE_TEST_ROOT": str(self.root), "PATH": str(self.tools) + os.pathsep + os.environ["PATH"]}
        self.write_exec(self.tools / "systemctl", SYSTEMCTL)
        self.write_exec(self.tools / "nft", NFT)
        self.write_exec(self.tools / "ip", "#!/usr/bin/env python3\nimport json,sys\na=sys.argv[1:]\nif a==['-j','route','show','default']:print(json.dumps([{'dev':'eth0'}]))\nelif a in (['-j','address','show'],['-j','-4','address','show']):print(json.dumps([{'ifname':'eth0','addr_info':[{'family':'inet','local':'203.0.113.10'}]}]))\nelse:print('[]')\n")
        for name in ("ss", "journalctl"):
            self.write_exec(self.tools / name, "#!/usr/bin/env bash\nprintf '%s\\n' '测试命令'\n")
        self.package = self.make_package("1.2.0", "release")
        root = self.root
        class Health(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_GET(self):
                try:
                    cfg = json.loads((root / "etc/netshare/controller.json").read_text())
                    services = json.loads((root / "services.json").read_text())
                    ok = self.path == cfg.get("access_path", "") + "/healthz" and services["netshare"]["active"]
                except Exception:
                    ok = False
                self.send_response(200 if ok else 503)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"ok": ok, "version": (root / "running-version.txt").read_text().strip() if (root / "running-version.txt").exists() else "1.2.2"}).encode())
            def do_POST(self):
                cfg = json.loads((root / "etc/netshare/controller.json").read_text())
                prefix = cfg.get("access_path", "") + "/api/enroll/"
                with (root / "post-paths.log").open("a") as log:
                    log.write(self.path + "\n")
                token = self.path[len(prefix):] if self.path.startswith(prefix) else ""
                source = root / "var/lib/netshare/enrollments" / (token + ".json")
                if not token or "/" in token or not source.is_file():
                    self.send_response(404)
                    self.end_headers()
                    return
                payload = source.read_bytes()
                source.unlink()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Health)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.controller_bundle = self.make_bundle("controller")
        self.agent_bundle = self.make_bundle("agent")

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.temp.cleanup()

    @staticmethod
    def write_exec(path, content):
        path.write_text(content)
        path.chmod(0o755)

    def make_package(self, version, name):
        directory = self.base / name
        (directory / "deploy").mkdir(parents=True)
        self.write_exec(directory / "netshare", FAKE_BINARY.replace("FAKE_VERSION", version))
        shutil.copy2(MANAGER, directory / "manage.sh")
        for name in ("netshare.service", "netshare-agent.service"):
            shutil.copy2(PROJECT / "deploy" / name, directory / "deploy" / name)
        return directory

    def make_bundle(self, role):
        directory = self.base / (role + "-bundle")
        directory.mkdir()
        if role == "controller":
            cfg = {"listen": "127.0.0.1:" + str(self.server.server_port), "external_url": "https://example.test", "password_hash": "original-hash", "access_path": "", "state_file": "/var/lib/netshare/state.json", "ca_file": "/etc/netshare/ca.pem", "cert_file": "/etc/netshare/controller.pem", "key_file": "/etc/netshare/controller-key.pem"}
            files = ["ca.pem", "controller.pem", "controller-key.pem"]
            (directory / "controller.json").write_text(json.dumps(cfg))
        else:
            cfg = {"state_file": "/var/lib/netshare-agent/state.json", "ca_file": "/etc/netshare-agent/ca.pem", "cert_file": "/etc/netshare-agent/server.pem", "key_file": "/etc/netshare-agent/server-key.pem"}
            files = ["ca.pem", "server.pem", "server-key.pem"]
            (directory / "agent.json").write_text(json.dumps(cfg))
        for name in files:
            (directory / name).write_text("测试叶子凭据")
        (directory / "admin-password.txt").write_text("不应复制的首次密码")
        (directory / "authority").mkdir()
        (directory / "authority/ca-key.pem").write_text("不应复制的CA私钥")
        return directory

    def run_manager(self, *args, ok=True, env=None):
        result = subprocess.run(["bash", str(MANAGER), *map(str, args)], env=env or self.env, text=True, errors="replace", capture_output=True, timeout=30)
        if ok:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("unbound variable", result.stderr, "脚本出现未绑定变量错误")
        self.assertFalse((self.root / "run/netshare-manage/lock").exists(), "锁未释放")
        return result

    def install(self, role, **kwargs):
        return self.run_manager("install", role, "--source", self.package, "--bundle", self.controller_bundle if role == "controller" else self.agent_bundle, **kwargs)

    def config(self):
        return json.loads((self.root / "etc/netshare/controller.json").read_text())

    def service(self, name):
        return json.loads((self.root / "services.json").read_text()).get(name, {"active": False, "enabled": False})

    def commands(self):
        return (self.root / "commands.log").read_text().splitlines()

    def release_fixture(self, *, bad_sum=False, tag="v1.2.0", duplicate=False):
        import platform
        arch = "arm64" if platform.machine() in ("arm64", "aarch64") else "amd64"
        filename = f"netshare_v1.2.0_linux_{arch}.tar.gz"
        archive = self.base / filename
        with tarfile.open(archive, "w:gz") as tf:
            tf.add(self.package, arcname="netshare_v1.2.0_linux_" + arch)
        digest = "0" * 64 if bad_sum else hashlib.sha256(archive.read_bytes()).hexdigest()
        metadata = self.base / "release.json"
        metadata.write_text(json.dumps({"tag_name": tag, "draft": False, "prerelease": False}))
        sums = self.base / "SHA256SUMS"
        line = digest + "  " + filename + "\n"
        sums.write_text(line + (line if duplicate else "") + "1" * 64 + "  manage.sh\n")
        config = {"tag": tag, "filename": filename, "archive": str(archive), "metadata": str(metadata), "sums": str(sums)}
        (self.root / "release-fixture.json").write_text(json.dumps(config))
        self.write_exec(self.tools / "curl", CURL)
        return config

    def run_menu(self, input_text):
        master, slave = pty.openpty()
        proc = subprocess.Popen(["bash", str(MANAGER)], env=self.env, stdin=slave, stdout=slave, stderr=slave, close_fds=True)
        os.close(slave)
        output = bytearray()
        try:
            os.write(master, input_text.encode())
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                ready, _, _ = select.select([master], [], [], .2)
                if ready:
                    try:
                        value = os.read(master, 65536)
                    except OSError as error:
                        if error.errno == errno.EIO:
                            break
                        raise
                    if not value:
                        break
                    output.extend(value)
                elif proc.poll() is not None:
                    break
            else:
                proc.kill()
                self.fail("交互菜单未按输入退出")
            self.assertEqual(proc.wait(timeout=5), 0, output.decode(errors="replace"))
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            os.close(master)
        self.assertFalse((self.root / "run/netshare-manage/lock").exists())
        text = output.decode(errors="replace")
        self.assertNotIn("unbound variable", text)
        return text

    def real_package(self):
        package = self.make_package("1.2.1", "real-release")
        shutil.copy2(self.real_binary, package / "netshare")
        self.env["NETSHARE_TEST_REAL_BINARY"] = "1"
        self.current_real_package = package
        return package

    def install_real_controller(self):
        package = self.real_package()
        result = self.run_manager("install", "controller", "--host", "203.0.113.50", "--port", str(self.server.server_port), "--source", package, "--no-start")
        return package, result

    def add_real_node(self, node_id, public_ip, private_ip):
        return self.run_manager("node", "add", "--source", self.current_real_package, "--id", node_id, "--name", node_id.upper(), "--public-ip", public_ip, "--private-ip", private_ip, "--public-interface", "eth0", "--private-interface", "eth1")

    def latest_pairing(self, node_id):
        candidates = list((self.root / "var/lib/netshare-bootstrap/pairings").glob("*/netshare-node-" + node_id + ".tar.gz"))
        self.assertTrue(candidates)
        return max(candidates, key=lambda p: p.stat().st_mtime_ns)

    def test_real_standalone_init_automatic_credentials_and_http_layout(self):
        package, result = self.install_real_controller()
        cfg = self.config()
        self.assertEqual(cfg["nodes"], [])
        self.assertEqual(cfg["external_url"], "http://203.0.113.50:" + str(self.server.server_port))
        self.assertEqual(cfg["listen"], "0.0.0.0:" + str(self.server.server_port))
        self.assertFalse(cfg["cookie_secure"])
        self.assertRegex(cfg["access_path"], r"^/ns-[0-9a-f]{32}$")
        bootstrap = self.root / "var/lib/netshare-bootstrap"
        password = (bootstrap / "controller/admin-password.txt").read_text().strip()
        self.assertGreaterEqual(len(password), 24)
        self.assertNotIn(password, result.stdout + result.stderr)
        self.assertTrue((bootstrap / "authority/ca-key.pem").is_file())
        self.assertFalse((self.root / "etc/netshare/authority").exists())
        self.assertFalse((self.root / "etc/netshare/admin-password.txt").exists())
        for item in bootstrap.rglob("*"):
            self.assertEqual(stat.S_IMODE(item.stat().st_mode), 0o700 if item.is_dir() else 0o600)
        self.assertFalse((self.root / "etc/netshare-web").exists())
        self.assertFalse((self.root / "usr/local/libexec/netshare-caddy").exists())
        self.assertFalse(self.service("netshare")["active"])
        original = (self.root / "etc/netshare/controller.json").read_bytes()
        ca = (bootstrap / "authority/ca-key.pem").read_bytes()
        self.run_manager("install", "controller", "--source", package, "--no-start")
        self.assertEqual((self.root / "etc/netshare/controller.json").read_bytes(), original)
        self.assertEqual((bootstrap / "authority/ca-key.pem").read_bytes(), ca)

    def test_real_node_pairing_updates_existing_node_without_losing_state(self):
        package, _ = self.install_real_controller()
        state = self.root / "var/lib/netshare/state.json"
        state.write_text('{"revision":7,"mappings":[],"events":[]}')
        original_state = state.read_bytes()
        original_password = (self.root / "var/lib/netshare-bootstrap/controller/admin-password.txt").read_bytes()
        self.add_real_node("node-a", "203.0.113.10", "10.42.0.10")
        first = self.latest_pairing("node-a")
        self.run_manager("install", "agent", "--source", package, "--pairing", first, "--no-start")
        agent_cfg = self.root / "etc/netshare-agent/agent.json"
        self.assertEqual(json.loads(agent_cfg.read_text())["allowed_targets"], [])
        agent_state = self.root / "var/lib/netshare-agent/state.json"
        agent_state.write_text('{"node_id":"node-a","revision":11,"rules":[]}')
        saved_agent_state = agent_state.read_bytes()
        self.add_real_node("node-b", "203.0.113.11", "10.42.0.11")
        update = self.latest_pairing("node-a")
        self.run_manager("install", "agent", "--source", package, "--pairing", update, "--no-start")
        self.assertEqual(json.loads(agent_cfg.read_text())["allowed_targets"], ["10.42.0.11"])
        self.assertEqual(agent_state.read_bytes(), saved_agent_state)
        self.assertEqual(state.read_bytes(), original_state)
        self.assertEqual((self.root / "var/lib/netshare-bootstrap/controller/admin-password.txt").read_bytes(), original_password)
        before = agent_cfg.read_bytes()
        wrong_node = self.latest_pairing("node-b")
        result = self.run_manager("install", "agent", "--source", package, "--pairing", wrong_node, "--no-start", ok=False)
        self.assertIn("ID", result.stderr)
        self.assertEqual(agent_cfg.read_bytes(), before)
        for archive in (first, update, wrong_node):
            self.assertEqual(stat.S_IMODE(archive.stat().st_mode), 0o600)
            with tarfile.open(archive) as tf:
                self.assertEqual({Path(m.name).name for m in tf.getmembers()}, {"agent.json", "ca.pem", "server.pem", "server-key.pem"})

    def test_real_automatic_install_failure_rolls_back_generated_authority(self):
        package = self.real_package()
        (self.root / "fail-start-once").touch()
        self.run_manager("install", "controller", "--host", "203.0.113.50", "--port", str(self.server.server_port), "--source", package, ok=False)
        for item in ("etc/netshare", "var/lib/netshare", "var/lib/netshare-bootstrap"):
            self.assertFalse((self.root / item).exists(), item)
        self.assertFalse(self.service("netshare")["active"])

    def test_real_default_release_install_needs_only_port(self):
        self.package = self.real_package()
        self.release_fixture()
        result = self.run_manager("install", "controller", "--host", "203.0.113.50", "--port", str(self.server.server_port))
        self.assertTrue(self.service("netshare")["active"])
        self.assertEqual(self.config()["nodes"], [])
        self.assertEqual(len((self.root / "downloads.log").read_text().splitlines()), 3)
        self.assertIn("管理入口", result.stdout)
        self.assertTrue((self.root / "var/lib/netshare-bootstrap/authority/ca-key.pem").exists())

    def test_fresh_menu_only_asks_port_and_returns_after_invalid_input(self):
        self.package = self.real_package()
        self.release_fixture()
        text = self.run_menu("1\n0\n1\n" + str(self.server.server_port) + "\n0\n")
        self.assertIn("控制台端口 [18440]", text)
        self.assertIn("本次操作未完成", text)
        self.assertIn("安装完成：controller", text)
        self.assertNotIn("管理台域名", text)
        self.assertNotIn("叶子凭据目录", text)
        self.assertNotIn("发行来源", text)
        self.assertEqual(self.config()["external_url"], "http://203.0.113.10:" + str(self.server.server_port))
        self.assertFalse(self.config()["cookie_secure"])
        self.assertTrue(self.service("netshare")["active"])

    def test_real_invalid_pairing_certificate_is_rejected_before_transaction(self):
        package, _ = self.install_real_controller()
        self.add_real_node("node-a", "203.0.113.10", "10.42.0.10")
        pairing = self.latest_pairing("node-a")
        self.run_manager("install", "agent", "--source", package, "--pairing", pairing, "--no-start")
        before = (self.root / "etc/netshare-agent/agent.json").read_bytes()
        commands = len(self.commands())
        invalid = self.base / "invalid-pairing.tar.gz"
        content_dir = self.base / "invalid-pairing"
        content_dir.mkdir()
        with tarfile.open(pairing) as source:
            source.extractall(content_dir)
        (content_dir / "node-a/server-key.pem").write_text("不是节点私钥")
        with tarfile.open(invalid, "w:gz") as target:
            target.add(content_dir / "node-a", arcname="node-a")
        result = self.run_manager("install", "agent", "--source", package, "--pairing", invalid, "--no-start", ok=False)
        self.assertIn("节点证书", result.stderr)
        self.assertEqual((self.root / "etc/netshare-agent/agent.json").read_bytes(), before)
        self.assertFalse(any("systemctl stop netshare-agent" in value for value in self.commands()[commands:]))
        self.assertFalse(list((self.root / "var/backups/netshare").glob("*agent/manifest.json"))[1:])

    def test_http_default_port_and_keep_uninstall_reinstall_purge(self):
        package = self.real_package()
        self.run_manager("install", "controller", "--source", package, "--no-start")
        self.assertEqual(self.config()["listen"], "0.0.0.0:18440")
        self.assertEqual(self.config()["external_url"], "http://203.0.113.10:18440")
        self.assertFalse(self.config()["cookie_secure"])
        bootstrap = (self.root / "var/lib/netshare-bootstrap/authority/ca-key.pem").read_bytes()
        cfg = (self.root / "etc/netshare/controller.json").read_bytes()
        self.run_manager("uninstall", "controller")
        self.assertTrue((self.root / "var/lib/netshare-bootstrap").exists())
        self.run_manager("install", "controller", "--source", package, "--no-start")
        self.assertEqual((self.root / "etc/netshare/controller.json").read_bytes(), cfg)
        self.assertEqual((self.root / "var/lib/netshare-bootstrap/authority/ca-key.pem").read_bytes(), bootstrap)
        self.run_manager("uninstall", "controller", "--purge")
        self.assertFalse((self.root / "var/lib/netshare-bootstrap").exists())
        self.assertFalse((self.root / "etc/netshare").exists())

    def test_direct_die_inside_transaction_restores_original_service_state(self):
        package = self.real_package()
        bootstrap = self.root / "var/lib/netshare-bootstrap/authority"
        bootstrap.mkdir(parents=True)
        (bootstrap / "ca-key.pem").write_text("保留原 CA，不应被替换")
        original = self.root / "usr/local/bin/netshare"
        original.parent.mkdir(parents=True)
        self.write_exec(original, "#!/usr/bin/env bash\nprintf '%s\\n' '原程序'\n")
        old_program = original.read_bytes()
        (self.root / "services.json").write_text(json.dumps({"netshare": {"active": True, "enabled": True}}))
        result = self.run_manager("install", "controller", "--host", "203.0.113.50", "--source", package, ok=False)
        self.assertIn("已有初始化资料", result.stderr)
        self.assertIn("回退", result.stderr)
        self.assertEqual(original.read_bytes(), old_program)
        self.assertEqual((bootstrap / "ca-key.pem").read_text(), "保留原 CA，不应被替换")
        self.assertTrue(self.service("netshare")["active"])
        self.assertTrue(self.service("netshare")["enabled"])
        self.assertFalse((self.root / "etc/netshare").exists())

    def test_controller_health_only_allows_verified_local_ipv4(self):
        # 执行实际脚本内的 Python 段；网卡清单和 HTTP 响应隔离，确保远程地址在发请求前拒绝。
        source = MANAGER.read_text().split("controller_health() {\n", 1)[1].split("<<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
        script = compile(source, str(MANAGER) + ":controller_health", "exec")
        cfg = self.base / "health-config.json"
        fixture = json.dumps([{"ifname": "eth0", "addr_info": [{"family": "inet", "local": "192.0.2.10"}]}]).encode()
        class Response(io.BytesIO):
            status = 200
        class Opener:
            def __init__(self):
                self.urls = []
            def open(self, url, **_):
                self.urls.append(url)
                return Response(b'{"ok":true}')
        for host, allowed in (("192.0.2.10", True), ("192.0.2.20", False), ("0.0.0.0", True), ("127.0.0.1", True), ("[::1]", True)):
            with self.subTest(host=host):
                cfg.write_text(json.dumps({"listen": host + ":18440", "access_path": "/ns-isolated-test-entry"}))
                opener = Opener()
                with mock.patch("sys.argv", ["-", str(cfg)]), mock.patch("subprocess.check_output", return_value=fixture) as ip_command, mock.patch("urllib.request.build_opener", return_value=opener):
                    if allowed:
                        exec(script, {"__name__": "__main__"})
                        expected = "127.0.0.1" if host == "0.0.0.0" else host
                        self.assertEqual(opener.urls, ["http://" + expected + ":18440/ns-isolated-test-entry/healthz"])
                    else:
                        with self.assertRaisesRegex(SystemExit, "拒绝探测远程地址"):
                            exec(script, {"__name__": "__main__"})
                        self.assertEqual(opener.urls, [])
                if host.startswith("192."):
                    ip_command.assert_called_once_with(["ip", "-j", "-4", "address", "show"], stderr=subprocess.DEVNULL, timeout=5)
                else:
                    ip_command.assert_not_called()

    def test_empty_remote_agent_input_does_not_download_or_install(self):
        self.release_fixture()
        text = self.run_menu("2\n\n0\n")
        self.assertIn("接入", text)
        self.assertFalse((self.root / "downloads.log").exists(), "空节点输入不应查询或下载发行")
        self.assertFalse((self.root / "usr/local/bin/netshare").exists())
        self.assertFalse((self.root / "etc/netshare-agent").exists())

    def test_local_controller_without_private_interface_does_not_download(self):
        self.install_real_controller()
        self.release_fixture()
        text = self.run_menu("2\n\n0\n")
        self.assertIn("私网", text)
        self.assertFalse((self.root / "downloads.log").exists(), "本机没有私网时不能先下载节点程序")
        self.assertFalse((self.root / "etc/netshare-agent").exists())

    def set_local_network(self, *, multiple=False):
        interfaces = [{"ifname": "eth0", "addr_info": [{"family": "inet", "local": "203.0.113.10"}]}, {"ifname": "eth1", "addr_info": [{"family": "inet", "local": "10.42.0.10"}]}]
        if multiple:
            interfaces.append({"ifname": "eth2", "addr_info": [{"family": "inet", "local": "10.43.0.10"}]})
        code = "#!/usr/bin/env python3\nimport json,sys\na=sys.argv[1:]\nif a==['-j','route','show','default']:print(json.dumps([{'dev':'eth0'}]))\nelse:print(" + repr(json.dumps(interfaces)) + ")\n"
        self.write_exec(self.tools / "ip", code)

    def test_local_menu_registers_and_updates_same_node_without_copying_files(self):
        self.package, _ = self.install_real_controller()
        self.set_local_network()
        self.release_fixture()
        text = self.run_menu("2\n0\n")
        self.assertIn("本机节点", text)
        self.assertIn("安装完成：agent", text)
        self.assertNotIn("节点安装包路径", text)
        nodes = self.config()["nodes"]
        self.assertEqual(len(nodes), 1)
        node_id = nodes[0]["id"]
        self.assertEqual(json.loads((self.root / "etc/netshare-agent/agent.json").read_text())["node"]["id"], node_id)
        self.add_real_node("node-b", "203.0.113.11", "10.42.0.11")
        saved = self.root / "var/lib/netshare-agent/state.json"
        saved.write_text(json.dumps({"node_id": node_id, "revision": 17, "rules": []}))
        before = saved.read_bytes()
        text = self.run_menu("2\n0\n")
        self.assertIn("安装完成：agent", text)
        self.assertEqual(len(self.config()["nodes"]), 2)
        self.assertEqual(saved.read_bytes(), before)
        self.assertEqual(json.loads((self.root / "etc/netshare-agent/agent.json").read_text())["allowed_targets"], ["10.42.0.11"])

    def test_local_ambiguous_interfaces_cancel_before_download(self):
        self.package, _ = self.install_real_controller()
        self.set_local_network(multiple=True)
        self.release_fixture()
        text = self.run_menu("2\n0\n0\n")
        self.assertIn("选择本机网卡组合", text)
        self.assertFalse((self.root / "downloads.log").exists())
        self.assertEqual(self.config()["nodes"], [])

    def test_join_receipt_fetch_one_time_and_resend_existing_node(self):
        self.package, _ = self.install_real_controller()
        cfg = self.config()
        cfg["external_url"] = "http://127.0.0.1:" + str(self.server.server_port)
        (self.root / "etc/netshare/controller.json").write_text(json.dumps(cfg))
        self.add_real_node("node-a", "203.0.113.10", "10.42.0.10")
        receipt_file = self.latest_pairing("node-a").parent / "node-node-a-invite.json"
        receipt = json.loads(receipt_file.read_text())
        self.assertEqual(stat.S_IMODE(receipt_file.stat().st_mode), 0o600)
        self.run_manager("install", "agent", "--source", self.package, "--join", receipt["invitation"], "--no-start")
        posts = (self.root / "post-paths.log").read_text()
        self.assertNotIn(receipt["key"], posts)
        self.assertNotIn("#", posts)
        retained = list((self.root / "var/backups/netshare-imports").glob("*/pairing.tar.gz"))
        self.assertEqual(len(retained), 1)
        self.assertEqual(stat.S_IMODE(retained[0].stat().st_mode), 0o600)
        result = self.run_manager("install", "agent", "--source", self.package, "--join", receipt["invitation"], "--no-start", ok=False)
        self.assertIn("重新生成", result.stderr)
        self.run_manager("node", "invite", "--id", "node-a", "--source", self.package)
        self.assertEqual(len(self.config()["nodes"]), 1)
        new_receipt = json.loads((self.latest_pairing("node-a").parent / "node-node-a-invite.json").read_text())
        self.assertNotEqual(new_receipt["invitation"], receipt["invitation"])
        self.run_manager("install", "agent", "--source", self.package, "--join", new_receipt["invitation"], "--no-start")

    def test_invalid_pairing_or_join_input_fails_before_release_download(self):
        self.release_fixture()
        self.run_manager("install", "agent", "--pairing", self.base / "does-not-exist.tar.gz", ok=False)
        self.run_manager("install", "agent", "--join", "not-an-invitation", ok=False)
        self.assertFalse((self.root / "downloads.log").exists())
        self.assertFalse((self.root / "etc/netshare-agent").exists())

    def test_missing_python_only_bootstraps_after_valid_material(self):
        prefix = MANAGER.read_text().rsplit('main "$@"', 1)[0]
        invitation = 'http://127.0.0.1:18440/ns-test-entry/api/enroll/' + 'a' * 32 + '#' + 'A' * 43
        for value, expected in (("", False), ("invalid", False), (invitation, True)):
            with self.subTest(value="valid" if expected else value):
                marker = self.base / "python-installed"
                marker.unlink(missing_ok=True)
                harness = self.base / "inspect-python.sh"
                harness.write_text(prefix + '''
ROLE=agent
ROOT_PREFIX="$NETSHARE_TEST_ROOT"
JOIN="$INSPECT_JOIN"
command() {
  if [[ "$1" == -v && "${2:-}" == python3 && ! -f "$INSPECT_MARKER" ]];then return 1;fi
  builtin command "$@"
}
ensure_dependencies() {
  [[ "${1:-}" == base ]] || exit 91
  printf 'base\\n' > "$INSPECT_MARKER"
}
ensure_inspection_python
''')
                env = {**self.env, "INSPECT_JOIN": value, "INSPECT_MARKER": str(marker)}
                result = subprocess.run(["bash", str(harness)], env=env, capture_output=True, text=True, timeout=10)
                if value == "invalid":
                    self.assertNotEqual(result.returncode, 0)
                else:
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(marker.exists(), expected)

    def test_invitation_access_is_granted_before_commands_are_printed(self):
        self.package, _ = self.install_real_controller()
        self.add_real_node("node-a", "203.0.113.10", "10.42.0.10")
        pairings = self.latest_pairing("node-a").parent
        audit = self.base / "publish-order.log"
        harness = self.base / "publish-order.sh"
        prefix = MANAGER.read_text().rsplit('main "$@"', 1)[0]
        harness.write_text(prefix + '''
ROOT_PREFIX="$NETSHARE_TEST_ROOT"
TEST_MODE=true
grant_enrollment_access() { printf 'granted\\n' >> "$INVITE_AUDIT"; }
say() {
  if [[ "$*" == *'的接入命令'* ]];then printf 'printed\\n' >> "$INVITE_AUDIT";fi
  printf '%s\\n' "$*"
}
emit_invitations "$INVITE_PAIRINGS" node-a
''')
        result = subprocess.run(["bash", str(harness)], env={**self.env, "INVITE_AUDIT": str(audit), "INVITE_PAIRINGS": str(pairings)}, text=True, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(audit.read_text().splitlines(), ["granted", "printed"])
        source = MANAGER.read_text().split("grant_enrollment_access() {\n", 1)[1].split("<<'PYCODE'\n", 1)[1].split("\nPYCODE\n", 1)[0]
        directory = self.base / "cipher-directory"
        directory.mkdir()
        ciphertext = directory / "token.json"
        ciphertext.write_text("ciphertext")
        receipt = self.base / "root-receipt.json"
        receipt.write_text("root-only key")
        with mock.patch("sys.argv", ["-", str(directory)]), mock.patch("pwd.getpwnam", return_value=types.SimpleNamespace(pw_uid=1234, pw_gid=1234)), mock.patch("os.chown") as chown:
            exec(compile(source, str(MANAGER) + ":grant_enrollment_access", "exec"), {"__name__": "__main__"})
        self.assertEqual({str(value.args[0]) for value in chown.call_args_list}, {str(directory), str(ciphertext)})
        for value in chown.call_args_list:
            self.assertEqual(value.args[1:], (1234, 1234))
            self.assertFalse(value.kwargs["follow_symlinks"])
        self.assertNotIn(str(receipt), {str(value.args[0]) for value in chown.call_args_list})

    def test_agent_creates_shared_sysctl_directory_with_traversable_permissions(self):
        target = self.root / "etc/sysctl.d"
        self.assertFalse(target.exists())
        self.install("agent")
        self.assertTrue(target.is_dir())
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o755)
        foreign = target / "50-other-service.conf"
        foreign.write_text("别的服务配置")
        self.run_manager("uninstall", "agent", "--purge")
        self.assertTrue(target.is_dir())
        self.assertEqual(foreign.read_text(), "别的服务配置")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o755)

    def test_agent_preserves_existing_shared_directory_permissions_and_rejects_symlink(self):
        target = self.root / "etc/sysctl.d"
        target.mkdir(parents=True)
        target.chmod(0o750)
        foreign = target / "50-foreign.conf"
        foreign.write_text("保留")
        self.install("agent")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o750)
        self.run_manager("uninstall", "agent", "--purge")
        outside = self.base / "outside-sysctl"
        target.rename(outside)
        target.symlink_to(outside, target_is_directory=True)
        before = len(self.commands())
        self.install("agent", ok=False)
        self.assertTrue(target.is_symlink())
        self.assertEqual((outside / "50-foreign.conf").read_text(), "保留")
        self.assertFalse(any("systemctl stop netshare-agent" in item for item in self.commands()[before:]))
        self.assertFalse((self.root / "etc/netshare-agent").exists())

    def test_agent_failure_rolls_back_owned_paths_and_keeps_shared_sysctl_directory(self):
        (self.root / "fail-start-once").touch()
        self.install("agent", ok=False)
        target = self.root / "etc/sysctl.d"
        self.assertTrue(target.is_dir())
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o755)
        for item in ("etc/netshare-agent", "var/lib/netshare-agent", "usr/local/bin/netshare", "etc/systemd/system/netshare-agent.service", "usr/local/sbin/netshare-manage", "etc/sysctl.d/90-netshare-agent.conf"):
            self.assertFalse((self.root / item).exists(), item)
        self.assertFalse(self.service("netshare-agent")["active"])

    def test_local_node_retry_after_agent_start_failure_reuses_registered_id(self):
        self.package, _ = self.install_real_controller()
        self.set_local_network()
        self.release_fixture()
        (self.root / "fail-start-once").touch()
        first = self.run_menu("2\n0\n")
        self.assertIn("本次操作未完成", first)
        self.assertEqual(len(self.config()["nodes"]), 1)
        node_id = self.config()["nodes"][0]["id"]
        self.assertFalse((self.root / "etc/netshare-agent").exists())
        second = self.run_menu("2\n0\n")
        self.assertIn("安装完成：agent", second)
        self.assertEqual(len(self.config()["nodes"]), 1)
        self.assertEqual(self.config()["nodes"][0]["id"], node_id)
        self.assertTrue(self.service("netshare-agent")["active"])
        self.assertEqual(stat.S_IMODE((self.root / "etc/sysctl.d").stat().st_mode), 0o755)

    def test_controller_port_probe_allows_time_wait_but_rejects_active_listener(self):
        source = MANAGER.read_text().split("initialize_controller() {\n", 1)[1].split("python3 - \"$BUNDLE/controller.json\" <<'PYCODE'\n", 1)[1].split("\nPYCODE\n", 1)[0]
        script = compile(source, str(MANAGER) + ":controller_port_probe", "exec")
        cfg = self.base / "port-probe-config.json"
        listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        client = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        peer = None
        try:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(("127.0.0.1", 0))
            listener.listen(1)
            port = listener.getsockname()[1]
            cfg.write_text(json.dumps({"listen": "127.0.0.1:" + str(port)}))
            with mock.patch("sys.argv", ["-", str(cfg)]):
                with self.assertRaisesRegex(SystemExit, "已被占用"):
                    exec(script, {"__name__": "__main__"})
            client.settimeout(3)
            client.connect(("127.0.0.1", port))
            peer, _ = listener.accept()
            peer.settimeout(3)
            peer.shutdown(socket.SHUT_WR)
            self.assertEqual(client.recv(1), b"")
            client.close()
            self.assertEqual(peer.recv(1), b"")
            peer.close()
            listener.close()
            # 服务端先发 FIN：端口只有 TIME_WAIT，旧的无 REUSEADDR 探测必须精确复现假冲突。
            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as old_probe:
                with self.assertRaises(OSError) as failure:
                    old_probe.bind(("127.0.0.1", port))
                self.assertEqual(failure.exception.errno, errno.EADDRINUSE)
            with mock.patch("sys.argv", ["-", str(cfg)]):
                exec(script, {"__name__": "__main__"})
        finally:
            if peer is not None:
                peer.close()
            client.close()
            listener.close()

    def test_default_official_release_download_and_exact_checksum(self):
        config = self.release_fixture()
        self.run_manager("install", "controller", "--bundle", self.controller_bundle)
        self.assertTrue(self.service("netshare")["active"])
        urls = (self.root / "downloads.log").read_text().splitlines()
        self.assertEqual(urls, ["https://api.github.com/repos/byJoey/netshare/releases/latest", "https://github.com/byJoey/netshare/releases/download/v1.2.0/SHA256SUMS", "https://github.com/byJoey/netshare/releases/download/v1.2.0/" + config["filename"]])
        before = len(urls)
        self.run_manager("upgrade", "controller")
        self.assertEqual(len((self.root / "downloads.log").read_text().splitlines()), before + 3)

    def test_official_release_rejects_bad_tag_checksum_and_duplicate(self):
        for options in ({"tag": "v1.2.0/evil"}, {"bad_sum": True}, {"duplicate": True}):
            with self.subTest(options=options):
                self.release_fixture(**options)
                self.run_manager("install", "controller", "--bundle", self.controller_bundle, ok=False)
                self.assertFalse((self.root / "usr/local/bin/netshare").exists())
                self.assertFalse((self.root / "etc/netshare").exists())

    def test_menu_loops_after_failure_and_install_defaults_release(self):
        self.release_fixture()
        self.install("controller")
        text = self.run_menu("99\n13\n1\n3\n1\n4\n1\n0\n")
        self.assertIn("无效选择", text)
        self.assertIn("本次操作未完成", text)
        self.assertIn("安装完成：controller", text)
        self.assertIn("升级完成：controller", text)
        self.assertEqual(len((self.root / "downloads.log").read_text().splitlines()), 6)
        self.assertGreaterEqual(text.count("NetShare 管理"), 4)
        self.assertTrue(self.service("netshare")["active"])
        self.assertTrue((self.root / "downloads.log").exists())

    def test_menu_uninstall_defaults_keep_and_explicit_purge(self):
        self.install("controller")
        keep = self.run_menu("12\n1\n\ny\n0\n")
        self.assertIn("配置、证书和映射状态已保留", keep)
        self.assertTrue((self.root / "etc/netshare/controller.json").exists())
        self.run_manager("install", "controller", "--source", self.package)
        cancel = self.run_menu("12\n1\n2\nNO\n0\n")
        self.assertNotIn("卸载完成", cancel)
        self.assertTrue((self.root / "etc/netshare/controller.json").exists())
        purge = self.run_menu("12\n1\n2\nPURGE\n0\n")
        self.assertIn("卸载完成", purge)
        self.assertFalse((self.root / "etc/netshare").exists())

    def test_install_preserve_rotate_and_role_isolation(self):
        result = self.install("controller")
        initial = self.config()
        self.assertRegex(initial["access_path"], r"^/ns-[0-9a-f]{32}$")
        self.assertIn(initial["access_path"], result.stdout)
        self.assertNotIn("不应复制的首次密码", result.stdout + result.stderr)
        self.assertTrue(self.service("netshare")["active"])
        self.assertEqual(stat.S_IMODE((self.root / "etc/netshare/controller.json").stat().st_mode), 0o600)
        self.assertFalse((self.root / "etc/netshare/admin-password.txt").exists())
        self.assertFalse((self.root / "etc/netshare/authority").exists())
        self.install("agent")
        before = len(self.commands())
        state = self.root / "var/lib/netshare/state.json"
        state.write_text('{"mappings":[{"id":"keep-user"}]}')
        self.run_manager("install", "controller", "--source", self.package)
        self.assertEqual(json.loads(state.read_text())["mappings"][0]["id"], "keep-user")
        self.assertEqual(self.config(), initial)
        changed = self.commands()[before:]
        self.assertFalse(any("systemctl stop netshare-agent" in x or "systemctl start netshare-agent" in x for x in changed))
        before = len(self.commands())
        self.run_manager("path", "rotate")
        self.assertNotEqual(self.config()["access_path"], initial["access_path"])
        calls = self.commands()[before:]
        self.assertLess(calls.index("systemctl stop netshare"), calls.index("binary rotate"))
        self.assertLess(calls.index("binary rotate"), calls.index("systemctl start netshare"))
        shown = self.run_manager("entry").stdout.strip()
        self.assertEqual(shown, "https://example.test" + self.config()["access_path"] + "/")
        self.run_manager("stop", "controller")
        before = len(self.commands())
        self.run_manager("path", "rotate")
        self.assertFalse(self.service("netshare")["active"])
        self.assertFalse(any("systemctl start netshare" == x for x in self.commands()[before:]))

    def test_upgrade_failure_keeps_last_save_and_ownership(self):
        self.install("controller")
        self.install("agent")
        state = self.root / "var/lib/netshare/state.json"
        state.write_text('{"mappings":[]}')
        original_binary = (self.root / "usr/local/bin/netshare").read_bytes()
        old_path = self.config()["access_path"]
        (self.root / "save-on-stop.json").write_text(json.dumps({"state": {"mappings": [{"id": "last-second-save"}]}, "config": {"password_hash": "just-saved-hash"}}))
        (self.root / "fail-start-once").touch()
        new = self.make_package("1.2.1", "new-release")
        before = len(self.commands())
        result = self.run_manager("upgrade", "controller", "--source", new, ok=False)
        self.assertIn("回退", result.stderr)
        self.assertEqual((self.root / "usr/local/bin/netshare").read_bytes(), original_binary)
        self.assertEqual(json.loads(state.read_text())["mappings"][0]["id"], "last-second-save")
        self.assertEqual(self.config()["password_hash"], "just-saved-hash")
        self.assertEqual(self.config()["access_path"], old_path)
        self.assertTrue(self.service("netshare")["active"])
        self.assertTrue(self.service("netshare-agent")["active"])
        self.assertFalse(any("systemctl stop netshare-agent" in x for x in self.commands()[before:]))
        self.assertEqual(stat.S_IMODE((self.root / "etc/netshare/controller.json").stat().st_mode), 0o600)

    def test_rotate_failure_restores_path_but_keeps_latest_password(self):
        self.install("controller")
        original = self.config()["access_path"]
        (self.root / "save-on-stop.json").write_text(json.dumps({"config": {"password_hash": "new-hash-at-stop"}}))
        (self.root / "fail-start-once").touch()
        self.run_manager("path", "rotate", ok=False)
        self.assertEqual(self.config()["access_path"], original)
        self.assertEqual(self.config()["password_hash"], "new-hash-at-stop")
        self.assertTrue(self.service("netshare")["active"])

    def test_backup_default_uninstall_and_purge(self):
        self.install("controller")
        self.install("agent")
        (self.root / "own-nft.txt").write_text("table ip netshare { }\n")
        (self.root / "foreign-nft.txt").write_text("别的服务规则")
        before = len(self.commands())
        self.run_manager("backup", "controller")
        self.assertFalse(any("systemctl stop" in x for x in self.commands()[before:]))
        self.run_manager("uninstall", "agent")
        self.assertFalse((self.root / "own-nft.txt").exists())
        self.assertEqual((self.root / "foreign-nft.txt").read_text(), "别的服务规则")
        self.assertTrue((self.root / "etc/netshare-agent/agent.json").exists())
        self.assertTrue((self.root / "usr/local/bin/netshare").exists(), "共享控制器还在不应删程序")
        self.assertTrue(self.service("netshare")["active"])
        self.run_manager("uninstall", "controller", "--purge")
        self.assertFalse((self.root / "etc/netshare").exists())
        self.assertFalse((self.root / "var/lib/netshare").exists())
        self.assertFalse((self.root / "usr/local/bin/netshare").exists())
        self.assertTrue((self.root / "usr/local/sbin/netshare-manage").exists())
        self.assertTrue(list((self.root / "var/backups/netshare").glob("*/manifest.json")))
        self.assertTrue((self.root / "etc/netshare-agent").exists())

    def test_new_install_failure_rolls_back_all_owned_paths(self):
        (self.root / "fail-start-once").touch()
        self.install("controller", ok=False)
        for item in ("etc/netshare", "var/lib/netshare", "usr/local/bin/netshare", "etc/systemd/system/netshare.service", "usr/local/sbin/netshare-manage"):
            self.assertFalse((self.root / item).exists(), item)
        self.assertFalse(self.service("netshare")["active"])

    def test_archive_checks_and_symlink_refusal(self):
        archive = self.base / "good.tar.gz"
        with tarfile.open(archive, "w:gz") as tf:
            tf.add(self.package, arcname="netshare_v1.2.0_linux_test")
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        self.run_manager("install", "controller", "--bundle", self.controller_bundle, "--source", archive, "--sha256", digest)
        self.run_manager("upgrade", "controller", "--source", archive, "--sha256", "0" * 64, ok=False)
        bad = self.base / "bad.tar.gz"
        with tarfile.open(bad, "w:gz") as tf:
            member = tarfile.TarInfo("../escape")
            tf.addfile(member)
        self.run_manager("upgrade", "controller", "--source", bad, ok=False)
        self.assertFalse((self.base / "escape").exists())
        config = self.root / "etc/netshare/controller.json"
        saved = self.base / "saved.json"
        config.rename(saved)
        config.symlink_to(saved)
        self.run_manager("path", "rotate", ok=False)
        self.assertTrue(config.is_symlink())
        self.assertTrue(self.service("netshare")["active"])

    def test_foreign_lock_is_not_removed(self):
        self.install("controller")
        lock = self.root / "run/netshare-manage/lock"
        lock.mkdir(parents=True)
        result = subprocess.run(["bash", str(MANAGER), "path", "rotate"], env=self.env, text=True, errors="replace", capture_output=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(lock.is_dir(), "拒绝并发后不能删除其他操作的锁")

    def test_test_mode_refuses_outside_system_commands(self):
        outside = self.base / "outside-bin"
        outside.mkdir()
        marker = self.base / "should-not-run"
        self.write_exec(outside / "systemctl", "#!/usr/bin/env bash\ntouch '" + str(marker) + "'\n")
        env = {**self.env, "PATH": str(outside) + os.pathsep + self.env["PATH"]}
        self.run_manager("status", "controller", ok=False, env=env)
        self.assertFalse(marker.exists(), "测试模式不应执行沙箱外systemctl")

    def test_test_mode_refuses_linked_command_directory(self):
        outside = self.base / "linked-tools"
        self.tools.rename(outside)
        self.tools.symlink_to(outside, target_is_directory=True)
        self.run_manager("status", "controller", ok=False)
        self.assertFalse((self.root / "commands.log").exists(), "不应执行符号链接目录中的模拟命令")

    def test_explicit_test_sentinel_and_no_start(self):
        invalid = self.base / "invalid"
        invalid.mkdir()
        env = {**self.env, "NETSHARE_TEST_ROOT": str(invalid)}
        self.run_manager("status", "controller", ok=False, env=env)
        self.assertFalse(list(invalid.iterdir()))
        self.run_manager("install", "controller", "--source", self.package, "--bundle", self.controller_bundle, "--no-start")
        self.assertFalse(self.service("netshare")["active"])
        self.assertTrue((self.root / "etc/netshare/controller.json").exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
