"""Build native distribution archives; Python and Go are build-time only."""

from __future__ import annotations

import argparse
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import zipfile


ROOT = Path(__file__).resolve().parents[1]
APP = "cloudreve-s3"
TARGETS = ("linux-amd64", "windows-amd64")
PACKAGE_FILES = (
    "deploy/gateway.local.example.json",
    "docs/running.md",
    "third-party/NOTICES.txt",
)


def archive_filter(info: tarfile.TarInfo) -> tarfile.TarInfo:
    info.uid = info.gid = 0
    info.uname = info.gname = ""
    info.mode = 0o755 if info.isdir() or Path(info.name).name == APP else 0o644
    return info


def build(go: str, output: Path, target: str) -> Path:
    goos, goarch = target.split("-")
    name = f"{APP}-{target}"
    with tempfile.TemporaryDirectory(prefix=f".{name}-", dir=output) as temporary:
        package = Path(temporary) / name
        package.mkdir()
        binary = package / (APP + (".exe" if goos == "windows" else ""))
        env = os.environ.copy()
        env.update(GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
        subprocess.run(
            [go, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", str(binary), f"./cmd/{APP}"],
            cwd=ROOT,
            env=env,
            check=True,
        )
        binary.chmod(0o755)
        for relative in PACKAGE_FILES:
            destination = package / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / relative, destination)
        if goos == "windows":
            # Only the checked-in template is copied; never read a live config.
            shutil.copyfile(ROOT / "deploy/gateway.local.example.json", package / "gateway.json")
            for launcher_name in ("start.cmd", "login.cmd", "Start-Gateway.ps1"):
                source = ROOT / "deploy/windows" / launcher_name
                if launcher_name.endswith(".cmd"):
                    (package / launcher_name).write_bytes(source.read_bytes().replace(b"\r\n", b"\n").replace(b"\n", b"\r\n"))
                else:
                    shutil.copyfile(source, package / launcher_name)
        (package / "README.txt").write_text(
            "Run this native package using docs/running.md.\n"
            + ("Windows: edit site and buckets_root in gateway.json, then double-click start.cmd.\n"
               "site must be an origin without webpage/API paths. HTTPS is the default.\n"
               "Intentional HTTP on a trusted network requires allow_http: true; credentials travel unencrypted.\n"
               "First launch asks for your login; the session and generated S3 keys are saved privately.\n"
               "S3 keys: .state/s3-credentials.json. Use login.cmd to sign in again.\n"
               if goos == "windows" else "Copy deploy/gateway.local.example.json to gateway.json and edit it first.\n")
            + "Each instance needs its own login session and S3 credentials.\n"
            "Config comments are in _comments; keep the file valid JSON.\n"
            "single_bucket_mode: true exposes buckets_root itself as its same-name bucket.\n"
            "single_bucket_mode: false (default) exposes its immediate child folders as buckets.\n"
            "No credentials or live configuration are included.\n"
            "Docker builds require the source repository.\n",
            encoding="utf-8",
        )
        if goos == "windows":
            result = output / f"{name}.zip"
            staged = Path(temporary) / result.name
            with zipfile.ZipFile(staged, "w", compression=zipfile.ZIP_DEFLATED) as archive:
                for path in sorted(package.rglob("*")):
                    if path.is_file():
                        archive.write(path, path.relative_to(package.parent).as_posix())
        else:
            result = output / f"{name}.tar.gz"
            staged = Path(temporary) / result.name
            with tarfile.open(staged, "w:gz") as archive:
                archive.add(package, arcname=name, filter=archive_filter)
        staged.replace(result)
    return result


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go", help="Go executable (default: go on PATH)")
    parser.add_argument("--output", type=Path, default=ROOT / "dist", help="archive output directory")
    parser.add_argument("--target", action="append", choices=TARGETS, help="repeat to select targets; default: both")
    args = parser.parse_args()
    executable = shutil.which(args.go)
    if executable is None:
        parser.error("Go executable not found; install Go or pass --go PATH")
    executable = str(Path(executable).resolve())
    for relative in PACKAGE_FILES:
        if not (ROOT / relative).is_file():
            parser.error(f"missing distribution file: {relative}")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    results = []
    for target in dict.fromkeys(args.target or TARGETS):
        print(f"Building {target}", flush=True)
        result = build(executable, output, target)
        digest = hashlib.sha256()
        with result.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        results.append(f"{digest.hexdigest()}  {result.name}\n")
        print(f"Created {result.name}", flush=True)
    (output / "SHA256SUMS").write_text("".join(results), encoding="ascii")


if __name__ == "__main__":
    main()
