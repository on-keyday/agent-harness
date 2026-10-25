#!/usr/bin/env python3
"""Inspect official upstream releases and optionally probe them through podman.

Downloaded candidates live in temporary directories. Probes have an empty HOME,
no harness credentials, no network, and make no model/API requests. This checks
packaging and executable startup, not authenticated inference or tool semantics.
"""

import argparse
import base64
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tarfile
import tempfile
import urllib.parse
import urllib.request

import check_codex_release as codex

AGENTS = ("claude", "codex", "agy", "opencode", "pi", "bash")
AGY_UPDATER = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app"


def fetch_json(url):
    headers = {"User-Agent": "agent-harness-sandbox-check", "Accept": "application/json"}
    if urllib.parse.urlparse(url).hostname == "api.github.com" and (token := os.environ.get("GITHUB_TOKEN")):
        headers["Authorization"] = "Bearer " + token
    with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=30) as response:
        return json.load(response)


def npm_metadata(package, version="latest"):
    return fetch_json("https://registry.npmjs.org/" + urllib.parse.quote(package, safe="@") + "/" + urllib.parse.quote(version, safe=""))


def npm_asset(package):
    dist = package["dist"]
    if urllib.parse.urlparse(dist["tarball"]).hostname != "registry.npmjs.org":
        raise ValueError("unexpected npm tarball host")
    algorithm, encoded = dist["integrity"].split("-", 1)
    if algorithm != "sha512":
        raise ValueError("expected npm SHA-512 integrity")
    digest = base64.b64decode(encoded, validate=True)
    if len(digest) != 64:
        raise ValueError("invalid npm SHA-512 integrity")
    return {"browser_download_url": dist["tarball"], "digest": "sha512:" + digest.hex()}


def upstream(agent, arch):
    if agent == "bash":
        return {"agent": agent, "source": "sandbox image", "version": None}
    if agent == "codex":
        release = codex.fetch_release("latest")
        return {"agent": agent, "source": release["html_url"], "version": release["tag_name"].removeprefix("rust-v"),
                "asset": codex.release_asset(release, ("aarch64" if arch == "arm64" else "x86_64") + "-unknown-linux-musl")}
    if agent == "claude":
        release = npm_metadata("@anthropic-ai/claude-code")
        name = "@anthropic-ai/claude-code-linux-" + arch
        native = npm_metadata(name, release["optionalDependencies"][name])
        return {"agent": agent, "source": "https://registry.npmjs.org/@anthropic-ai%2fclaude-code/latest",
                "version": release["version"], "asset": npm_asset(native), "binary_name": "claude"}
    if agent == "pi":
        release = npm_metadata("@earendil-works/pi-coding-agent")
        return {"agent": agent, "source": "https://registry.npmjs.org/@earendil-works%2fpi-coding-agent/latest",
                "version": release["version"], "asset": npm_asset(release), "binary_name": "cli.js"}
    if agent == "agy":
        source = AGY_UPDATER + "/manifests/linux_" + ("amd64" if arch == "x64" else arch) + ".json"
        release = fetch_json(source)
        if not release["url"].startswith("https://storage.googleapis.com/antigravity-public/antigravity-cli/"):
            raise ValueError("unexpected Antigravity download URL")
        if not re.fullmatch(r"[0-9a-f]{128}", release["sha512"]):
            raise ValueError("invalid Antigravity SHA-512 digest")
        return {"agent": agent, "source": source, "version": release["version"],
                "asset": {"browser_download_url": release["url"], "digest": "sha512:" + release["sha512"]}, "binary_name": "antigravity"}
    if agent == "opencode":
        release = fetch_json("https://api.github.com/repos/anomalyco/opencode/releases/latest")
        if release.get("draft") or release.get("prerelease"):
            raise ValueError("expected a stable OpenCode release")
        name = "opencode-linux-" + ("x64-baseline" if arch == "x64" else arch) + ".tar.gz"
        assets = [asset for asset in release["assets"] if asset["name"] == name]
        if len(assets) != 1 or not re.fullmatch(r"sha256:[0-9a-f]{64}", assets[0].get("digest") or ""):
            raise ValueError("OpenCode bundle or SHA-256 digest missing")
        if not assets[0]["browser_download_url"].startswith("https://github.com/anomalyco/opencode/releases/download/"):
            raise ValueError("unexpected OpenCode download URL")
        return {"agent": agent, "source": release["html_url"], "version": release["tag_name"].removeprefix("v"),
                "asset": assets[0], "binary_name": "opencode"}
    raise ValueError("unsupported agent: " + agent)


def candidate(info, directory):
    agent = info["agent"]
    if agent == "bash":
        return None
    if agent == "codex":
        return codex.unpack_release(info["asset"], directory)
    archive = codex.download_verified(info["asset"], directory)
    if agent == "pi":
        npm = shutil.which("npm")
        if npm is None:
            raise ValueError("npm is required to prepare the Pi dependency tree")
        home = directory / "npm-home"
        home.mkdir()
        env = {key: os.environ[key] for key in ("PATH",) if key in os.environ}
        env.update(HOME=str(home), npm_config_cache=str(directory / "npm-cache"), npm_config_userconfig="/dev/null", npm_config_registry="https://registry.npmjs.org/")
        prefix = directory / "npm-install"
        # Match the documented global package installation, using the exact
        # verified tarball. Installing inside the extracted project's root
        # would apply its development overrides and build a different tree.
        result = subprocess.run([npm, "install", "--global", "--ignore-scripts", "--omit=dev", "--no-audit", "--no-fund", "--prefix", str(prefix), str(archive)],
                                env=env, capture_output=True, text=True, timeout=180)
        if result.returncode:
            raise ValueError("Pi dependency preparation failed: " + result.stderr[-2000:])
        binary = prefix / "lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"
    else:
        extracted = directory / "package"
        with tarfile.open(archive, "r:gz") as source:
            source.extractall(extracted, filter="data")
        binaries = [path for path in extracted.rglob(info["binary_name"]) if path.is_file()]
        if len(binaries) != 1:
            raise ValueError(f"{agent} package layout changed: expected exactly one {info['binary_name']}")
        binary = binaries[0]
    if not os.access(binary, os.X_OK):
        raise ValueError(f"{agent} package executable has no execute permission")
    return binary


def check_agent(agent, arch, image, smoke):
    report = {"agent": agent, "status": "error", "runtime_tested": False}
    try:
        info = upstream(agent, arch)
        if info["version"] is not None and not re.fullmatch(r"\d+\.\d+\.\d+", info["version"]):
            raise ValueError("unrecognized upstream version")
        report.update(source=info["source"], version=info["version"])
        if smoke:
            with tempfile.TemporaryDirectory(prefix="harness-upstream-" + agent + "-") as temp:
                directory = Path(temp)
                binary = candidate(info, directory)
                report["runtime_tested"] = True
                output = codex.smoke_bundle(binary, image, directory, agent=agent)
                if info["version"] and not re.search(r"(?<!\d)" + re.escape(info["version"]) + r"(?!\d)", output):
                    raise ValueError("sandbox version output does not match upstream release")
                report.update(runtime_tested=True, runtime_output=output)
        report["status"] = "ok"
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError, subprocess.SubprocessError) as error:
        report["error"] = str(error)
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", choices=("all",) + AGENTS, default="all")
    parser.add_argument("--smoke", action="store_true", help="download exact upstream candidates and run isolated startup probes")
    parser.add_argument("--image", default=os.environ.get("HARNESS_SANDBOX_IMAGE", "harness-agent-sandbox:latest"))
    args = parser.parse_args(argv)
    machine = platform.machine()
    if machine not in ("x86_64", "aarch64") or platform.system() != "Linux":
        parser.error("runtime release checks currently support Linux x86_64/aarch64")
    arch = "arm64" if machine == "aarch64" else "x64"
    agents = AGENTS if args.agent == "all" else (args.agent,)
    reports = [check_agent(agent, arch, args.image, args.smoke) for agent in agents]
    print(json.dumps({"checks": reports}, indent=2))
    return int(any(report["status"] != "ok" for report in reports))


if __name__ == "__main__":
    raise SystemExit(main())
