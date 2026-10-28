#!/usr/bin/env python3
"""用安装器的真实解包代码校验最终发行附件，兼容 Linux 与 macOS 构建机。"""
import argparse
import hashlib
import pathlib
import re
import struct
import subprocess
import sys
import tarfile
import tempfile


PROJECT = pathlib.Path(__file__).resolve().parents[1]


def checksum_for(sums, filename):
    records = []
    for line in sums.read_text().splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[1].removeprefix("*") == filename:
            if not re.fullmatch(r"[0-9a-fA-F]{64}", fields[0]):
                raise ValueError(f"{filename} 的校验和格式无效")
            records.append(fields[0].lower())
    if len(records) != 1:
        raise ValueError(f"{filename} 必须有唯一、精确匹配的校验和")
    return records[0]


def installer_unpacker(manager):
    # 直接执行产品中的校验/解包代码，避免另写一个宽松版本掩盖真实安装失败。
    source = manager.read_text()
    function = source.split("prepare_package() {", 1)[1]
    match = re.search(
        r"python3 - \"\$archive\" \"\$TMP_DIR/unpacked\" \"\$EXPECTED_SHA\" <<'PY'\n(.*?)\nPY\n",
        function,
        re.S,
    )
    if not match:
        raise ValueError("未找到安装器的真实归档校验入口，请同步此回归脚本")
    compile(match[1], str(manager) + ":prepare_package", "exec")
    return match[1]


def check_archive(archive, sums, unpacker):
    match = re.fullmatch(r"netshare_(v\d+\.\d+\.\d+(?:[.-][A-Za-z0-9.-]+)?)_linux_(amd64|arm64)\.tar\.gz", archive.name)
    if not match:
        raise ValueError(f"发行包名无效：{archive.name}")
    expected = checksum_for(sums, archive.name)
    actual = hashlib.sha256(archive.read_bytes()).hexdigest()
    if actual != expected:
        raise ValueError(f"{archive.name} 的 SHA256 不一致")
    with tempfile.TemporaryDirectory(prefix="netshare-release-package-") as temporary:
        output = pathlib.Path(temporary) / "unpacked"
        result = subprocess.run(
            [sys.executable, "-c", unpacker, str(archive), str(output), expected],
            capture_output=True,
            text=True,
            timeout=30,
        )
        if result.returncode:
            raise ValueError(f"{archive.name} 无法通过真实安装器解包：{result.stderr.strip()}")
        root = pathlib.Path((output / "package-root.txt").read_text())
        if root.name != archive.name.removesuffix(".tar.gz"):
            raise ValueError("发行包根目录与文件名不一致")
        with tarfile.open(archive, "r:gz") as package:
            for member in package.getmembers():
                if any(part.startswith("._") or part == "__MACOSX" for part in pathlib.PurePosixPath(member.name).parts):
                    raise ValueError("发行包含 macOS 元数据文件")
        for filename in ("netshare", "manage.sh", "install.sh", "README.md", "LICENSE", "deploy/netshare.service", "deploy/netshare-agent.service"):
            if not (root / filename).is_file():
                raise ValueError(f"发行包缺少 {filename}")
        binary = root / "netshare"
        header = binary.read_bytes()[:64]
        if len(header) != 64 or header[:6] != b"\x7fELF\x02\x01":
            raise ValueError("发行包必须含 64 位 Linux ELF 程序")
        if struct.unpack("<H", header[18:20])[0] != {"amd64": 62, "arm64": 183}[match[2]]:
            raise ValueError("发行包 CPU 架构与文件名不一致")
        if not binary.stat().st_mode & 0o111:
            raise ValueError("发行包程序缺少执行权限")
        if not (root / "licenses").is_dir() or not any((root / "licenses").iterdir()):
            raise ValueError("发行包缺少依赖许可")
        subprocess.run(["bash", "-n", str(root / "manage.sh")], check=True, timeout=10)
        subprocess.run(["bash", "-n", str(root / "install.sh")], check=True, timeout=10)
    print(f"通过：{archive.name}，校验和、真实安装器解包、Linux 架构及必要文件")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archives", nargs="+", type=pathlib.Path)
    parser.add_argument("--sums", required=True, type=pathlib.Path)
    parser.add_argument("--manager", type=pathlib.Path, default=PROJECT / "manage.sh")
    args = parser.parse_args()
    unpacker = installer_unpacker(args.manager)
    for archive in args.archives:
        check_archive(archive.resolve(), args.sums, unpacker)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(f"发行附件验收失败：{error}", file=sys.stderr)
        raise SystemExit(1)
