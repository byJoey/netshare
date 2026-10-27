#!/usr/bin/env python3
"""隔离管理脚本生命周期验证：真实文件操作，systemd/nft 仅使用沙箱模拟。"""
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tarfile
import tempfile
import threading
import unittest

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


class Lifecycle(unittest.TestCase):
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
        for name in ("ip", "ss", "journalctl"):
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
                self.wfile.write(json.dumps({"ok": ok}).encode())
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

    def test_install_preserve_rotate_and_role_isolation(self):
        result = self.install("controller")
        initial = self.config()
        self.assertRegex(initial["access_path"], r"^/ns-[0-9a-f]{32}$")
        self.assertNotIn(initial["access_path"], result.stdout + result.stderr)
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
