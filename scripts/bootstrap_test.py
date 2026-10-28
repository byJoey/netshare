#!/usr/bin/env python3
"""远程Bash引导器隔离验证：所有下载与提权均为本地替身。"""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

PROJECT = Path(__file__).resolve().parents[1]
PAYLOAD = '#!/usr/bin/env bash\nprintf "%s\\n" "$*" > "$INSTALL_CASE_ROOT/ran"\n'
CURL = r'''#!/usr/bin/env python3
import hashlib,os,pathlib,sys
a=sys.argv[1:];url=next(x for x in a if x.startswith('https://'))
root=pathlib.Path(os.environ['INSTALL_CASE_ROOT']);payload=(root/'payload').read_bytes()
case=os.environ.get('INSTALL_CASE','ok')
if url.endswith('/releases/latest'):
 print('https://github.com/byJoey/netshare/releases/tag/'+('v1.2.0' if case!='tag' else 'not-a-version'),end='');raise SystemExit(0)
out=pathlib.Path(a[a.index('--output')+1])
if url.endswith('/SHA256SUMS'):
 digest=hashlib.sha256(payload).hexdigest()
 if case=='sha':digest='0'*64
 text=digest+'  manage.sh\n'
 if case=='duplicate':text+=text
 out.write_text(text)
elif url.endswith('/manage.sh'):out.write_bytes(payload)
else:raise SystemExit(2)
'''


class Bootstrap(unittest.TestCase):
    def run_case(self, case):
        with tempfile.TemporaryDirectory(prefix="netshare-bootstrap-test-") as raw:
            root = Path(raw).resolve()
            tools = root / "bin"
            tools.mkdir()
            (root / "tmp").mkdir()
            for name, script in {
                "curl": CURL,
                "uname": "#!/usr/bin/env bash\nprintf 'Linux\\n'\n",
                "sudo": '#!/usr/bin/env bash\nexec "$@"\n',
            }.items():
                (tools / name).write_text(script)
                (tools / name).chmod(0o755)
            (root / "payload").write_text(PAYLOAD)
            env = os.environ | {"PATH": str(tools) + os.pathsep + os.environ["PATH"], "INSTALL_CASE_ROOT": str(root), "INSTALL_CASE": case, "TMPDIR": str(root / "tmp")}
            result = subprocess.run(["bash", str(PROJECT / "install.sh"), "--help"], env=env, text=True, capture_output=True, timeout=15)
            executed = (root / "ran").read_text().strip() if (root / "ran").exists() else None
            self.assertEqual(list((root / "tmp").iterdir()), [], "引导器临时目录未清理")
            return result, executed

    def test_verified_script_and_arguments(self):
        result, executed = self.run_case("ok")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(executed, "--help")

    def test_bad_checksum_never_executes(self):
        result, executed = self.run_case("sha")
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(executed)

    def test_duplicate_checksum_never_executes(self):
        result, executed = self.run_case("duplicate")
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(executed)

    def test_invalid_release_tag_never_executes(self):
        result, executed = self.run_case("tag")
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(executed)


if __name__ == "__main__":
    unittest.main(verbosity=2)
