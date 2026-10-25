#!/usr/bin/env python3
"""Check the installed Codex bundle, or smoke-test an upstream release in podman.

No login, model request, installation, or host configuration change is performed.
The upstream probe is deliberately opt-in; ordinary unit tests remain offline.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse
import urllib.request
from pathlib import Path

RELEASE_API = "https://api.github.com/repos/openai/codex/releases/"
WRAPPER = Path(__file__).with_name("agent-in-podman.sh")
REQUIRED_BINARIES = ("codex", "codex-code-mode-host")


def fetch_release(tag: str) -> dict:
    suffix = "latest" if tag == "latest" else "tags/" + urllib.parse.quote(tag, safe="")
    headers = {"Accept": "application/vnd.github+json", "User-Agent": "agent-harness-sandbox-check"}
    # Only the API request receives this token, never the asset/CDN request.
    if token := os.environ.get("GITHUB_TOKEN"):
        headers["Authorization"] = "Bearer " + token
    with urllib.request.urlopen(urllib.request.Request(RELEASE_API + suffix, headers=headers), timeout=30) as response:
        release = json.load(response)
    if release.get("draft") or release.get("prerelease"):
        raise ValueError("expected a published stable release")
    if not re.fullmatch(r"rust-v\d+\.\d+\.\d+", release.get("tag_name", "")):
        raise ValueError("unrecognized Codex release tag")
    return release


def release_asset(release: dict, target: str) -> dict:
    name = f"codex-package-{target}.tar.gz"
    matches = [asset for asset in release.get("assets", []) if asset.get("name") == name]
    if len(matches) != 1:
        raise ValueError(f"upstream bundle asset missing or ambiguous: {name}")
    asset = matches[0]
    expected_url = f"https://github.com/openai/codex/releases/download/{release['tag_name']}/{name}"
    if asset.get("browser_download_url") != expected_url:
        raise ValueError("unexpected upstream asset URL")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", asset.get("digest") or ""):
        raise ValueError("upstream asset has no SHA-256 digest; cannot validate download")
    return asset


def download_verified(asset: dict, directory: Path) -> Path:
    archive = directory / "release.tar.gz"
    algorithm, expected = asset["digest"].split(":", 1)
    if algorithm not in ("sha256", "sha512") or not re.fullmatch(r"[0-9a-f]+", expected):
        raise ValueError("unsupported release digest")
    digest = hashlib.new(algorithm)
    size = 0
    request = urllib.request.Request(asset["browser_download_url"], headers={"User-Agent": "agent-harness-sandbox-check"})
    with urllib.request.urlopen(request, timeout=60) as response, archive.open("wb") as output:
        while chunk := response.read(1024 * 1024):
            size += len(chunk)
            if size > 512 * 1024 * 1024:
                raise ValueError("release archive exceeds download limit")
            digest.update(chunk)
            output.write(chunk)
    if size != asset.get("size", size) or digest.hexdigest() != expected:
        raise ValueError("release archive size or SHA-256 mismatch" if algorithm == "sha256" else "release archive size or SHA-512 mismatch")
    return archive


def unpack_release(asset: dict, directory: Path) -> Path:
    archive = download_verified(asset, directory)
    extracted = directory / "package"
    with tarfile.open(archive, "r:gz") as source:
        source.extractall(extracted, filter="data")
    candidates = list(extracted.glob("**/bin/codex"))
    if len(candidates) != 1:
        raise ValueError("upstream package layout changed: expected exactly one bin/codex")
    return candidates[0]


def inspect_bundle(binary: Path, *, run_version: bool = True) -> dict:
    binary = binary.resolve(strict=True)
    missing = [name for name in REQUIRED_BINARIES if not os.access(binary.parent / name, os.X_OK)]
    if missing:
        raise ValueError("Codex bundle is missing executable siblings: " + ", ".join(missing))
    version = None
    if run_version:
        result = subprocess.run([str(binary), "--version"], capture_output=True, text=True, timeout=15, check=True)
        match = re.fullmatch(r"codex-cli (\d+\.\d+\.\d+)\s*", result.stdout)
        if match is None:
            raise ValueError("unrecognized codex --version output")
        version = match[1]
    return {"binary": str(binary), "version": version, "executables": sorted(p.name for p in binary.parent.iterdir() if p.is_file() and os.access(p, os.X_OK))}


def smoke_bundle(binary: Path | None, image: str, directory: Path, *, agent: str = "codex") -> str:
    """Use the real wrapper mounts and image launcher, with an empty HOME.

    The podman shim restores podman's own HOME so rootless image storage stays
    usable. It replaces only the agent command with a deterministic probe; the
    production wrapper still assembles every mount and confinement option.
    """
    podman = shutil.which("podman")
    if podman is None:
        raise ValueError("podman is required for --smoke")
    home, tools, work = (directory / name for name in ("home", "tools", "work"))
    for path in (home / ".local/bin", tools, work):
        path.mkdir(parents=True)
    if binary is not None:
        (home / ".local/bin" / agent).symlink_to(binary.resolve())
    shim = tools / "podman"
    shim.write_text(f"#!{sys.executable}\n" + r'''import os, sys
args = sys.argv[1:]
if args and args[0] == "run":
    index = args.index("/usr/local/bin/sandbox-agent-launch.sh")
    commands = [
        '/usr/local/bin/sandbox-agent-launch.sh --version',
    ]
    if os.environ.get("SANDBOX_PROBE_AGENT") == "codex":
        commands += [
        'helper="$(dirname "$SANDBOX_AGENT_BIN")/codex-code-mode-host"',
        'test -x "$helper"',
        '"$helper" --help',
        ]
    args[index:] = ["/bin/sh", "-ec", '\n'.join(commands)]
    # Only this disposable, credential-free home is writable. Vendor version
    # commands may initialize state even though no model request is made.
    args[1:1] = ["--network=none", "-v", os.environ["HOME"] + ":" + os.environ["HOME"]]
env = dict(os.environ, HOME=os.environ["SANDBOX_PROBE_PODMAN_HOME"])
os.execve(os.environ["SANDBOX_PROBE_PODMAN"], [os.environ["SANDBOX_PROBE_PODMAN"], *args], env)
''')
    shim.chmod(0o755)
    env = {key: os.environ[key] for key in ("PATH", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "USER", "LOGNAME") if key in os.environ}
    env.update(HOME=str(home), PATH=str(tools) + os.pathsep + os.environ.get("PATH", ""),
               HARNESS_SANDBOX_IMAGE=image, SANDBOX_PROBE_PODMAN=podman,
               SANDBOX_PROBE_PODMAN_HOME=str(Path.home()), SANDBOX_PROBE_AGENT=agent)
    if agent == "opencode":
        env["HARNESS_SANDBOX_OPENCODE_BIN"] = str(binary.resolve())
    if agent == "pi":
        env["HARNESS_SANDBOX_PI_PKG"] = str(binary.resolve().parents[2])
    # --no-firewall: the shim already gives the container --network=none, and
    # the default proxy mode would start a firewall and a proxy inside it.
    result = subprocess.run(["bash", str(WRAPPER), "--sandbox-agent", agent, "--omit-harness-cli", "--no-firewall"],
                            cwd=work, env=env, capture_output=True, text=True, timeout=60)
    if result.returncode != 0:
        raise ValueError("sandbox runtime probe failed: " + (result.stderr + result.stdout)[-4000:])
    if agent == "codex" and ("codex-cli " not in result.stdout or "codex-code-mode-host" not in result.stdout):
        raise ValueError("sandbox probe did not confirm both Codex and its code-mode host")
    if not result.stdout.strip():
        raise ValueError("sandbox version probe produced no output")
    return result.stdout.strip()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--upstream", action="store_true", help="inspect the official stable GitHub release; with --smoke, download and test it")
    parser.add_argument("--release", default="latest", help="latest or an exact rust-vX.Y.Z tag")
    parser.add_argument("--binary", type=Path, help="local Codex executable (default: codex on PATH)")
    parser.add_argument("--smoke", action="store_true", help="start Codex and its helper through the real podman wrapper, without credentials or network")
    parser.add_argument("--image", default=os.environ.get("HARNESS_SANDBOX_IMAGE", "harness-agent-sandbox:latest"))
    parser.add_argument("--target", choices=("x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl"),
                        default=("aarch64" if platform.machine() == "aarch64" else "x86_64") + "-unknown-linux-musl")
    args = parser.parse_args(argv)
    report: dict = {"status": "error"}
    try:
        release = fetch_release(args.release) if args.upstream else None
        if release:
            asset = release_asset(release, args.target)
            report["upstream"] = {"tag": release["tag_name"], "published_at": release["published_at"], "url": release["html_url"], "asset": asset["name"]}
        binary = args.binary or (Path(path) if (path := shutil.which("codex")) else None)
        if binary:
            report["installed"] = inspect_bundle(binary)
            if release:
                report["installed_matches_upstream"] = report["installed"]["version"] == release["tag_name"].removeprefix("rust-v")
        elif not release:
            raise ValueError("codex was not found; pass --binary or --upstream")
        with tempfile.TemporaryDirectory(prefix="harness-codex-check-") as temp:
            directory = Path(temp)
            if args.smoke:
                candidate = unpack_release(asset, directory) if release else binary
                # Downloaded candidates execute inside podman only.
                report["candidate"] = inspect_bundle(candidate, run_version=False)
                report["runtime_probe"] = smoke_bundle(candidate, args.image, directory)
                match = re.search(r"codex-cli (\d+\.\d+\.\d+)", report["runtime_probe"])
                report["candidate"]["version"] = match[1] if match else None
                if release and report["candidate"]["version"] != release["tag_name"].removeprefix("rust-v"):
                    raise ValueError("downloaded binary version does not match release tag")
        report["status"] = "ok"
        print(json.dumps(report, indent=2))
        return 0
    except (OSError, ValueError, tarfile.TarError, subprocess.SubprocessError) as error:
        report["error"] = str(error)
        print(json.dumps(report, indent=2))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
