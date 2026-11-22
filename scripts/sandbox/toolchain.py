#!/usr/bin/env python3
"""Resolve a named sandbox toolchain into podman argv tokens.

agent-in-podman.sh --toolchain=NAME reads scripts/sandbox/toolchains.json
(override: HARNESS_SANDBOX_TOOLCHAINS in the runner's environment) and asks this
helper to turn the named entry into mounts, env and PATH for `podman run`. The
helper runs the entry's host probe, resolves its placeholders, validates
everything and prints NUL-separated argv tokens on stdout; the wrapper never
parses the JSON.

    toolchain.py --config FILE --name NAME[,NAME...] [--container-path PATH]
    toolchain.py --config FILE --name NAME[,NAME...] --domains

--domains prints the merged allow_domains as one comma-separated line and runs
no probes, so the wrapper can fold a toolchain's domains into its allowlist
before the firewall is built. Every refusal exits 2 with a named reason on
stderr. See docs/superpowers/specs/2026-11-22-sandbox-toolchain-bridge-design.md.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys

# The image's PATH when the wrapper cannot read it from `podman image inspect`.
DEFAULT_BASE_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
# The same host-name shape agent-in-podman.sh enforces for --allow-domain.
HOST_NAME = re.compile(
    r"^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$"
)
PLACEHOLDER = re.compile(r"\{([A-Za-z_][A-Za-z0-9_]*)\}")
PROBE_TIMEOUT = 120


class Refusal(Exception):
    """A reason the wrapper prints and exits 2 on."""


def refuse(reason: str) -> None:
    raise Refusal(reason)


def load_config(path: str) -> dict:
    try:
        with open(path, encoding="utf-8") as handle:
            config = json.load(handle)
    except OSError as error:
        refuse(f"cannot read toolchain config {path}: {error.strerror or error}")
    except json.JSONDecodeError as error:
        refuse(f"toolchain config {path} is not valid JSON: {error}")
    if not isinstance(config, dict):
        refuse(f"toolchain config {path} must be a JSON object keyed by toolchain name")
    return config


def chosen_entries(config: dict, names: list[str]) -> list[tuple[str, dict]]:
    chosen = []
    for name in names:
        if not name:
            refuse("empty toolchain name (--toolchain=NAME[,NAME...])")
        entry = config.get(name)
        if entry is None:
            refuse(f"unknown toolchain: {name}")
        if not isinstance(entry, dict):
            refuse(f"toolchain {name}: the entry must be a JSON object")
        chosen.append((name, entry))
    return chosen


def run_probe(name: str, entry: dict) -> dict[str, str]:
    probe = entry.get("probe")
    if probe is None:
        return {}
    if not isinstance(probe, dict):
        refuse(f"toolchain {name}: probe must be a JSON object")
    command = probe.get("command")
    probe_args = probe.get("args", [])
    if not isinstance(command, str) or not command:
        refuse(f"toolchain {name}: probe.command must be a non-empty string")
    if not isinstance(probe_args, list) or not all(isinstance(arg, str) for arg in probe_args):
        refuse(f"toolchain {name}: probe.args must be a list of strings")
    try:
        result = subprocess.run(
            [command, *probe_args], capture_output=True, text=True, timeout=PROBE_TIMEOUT
        )
    except FileNotFoundError:
        refuse(f"toolchain {name}: probe command not found: {command}")
    except subprocess.TimeoutExpired:
        refuse(f"toolchain {name}: probe timed out after {PROBE_TIMEOUT}s: {command}")
    except OSError as error:
        refuse(f"toolchain {name}: cannot run probe {command}: {error.strerror or error}")
    if result.returncode != 0:
        refuse(f"toolchain {name}: probe exited {result.returncode}: {command} {' '.join(probe_args)}")
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        refuse(f"toolchain {name}: probe output is not JSON: {error}")
    if not isinstance(data, dict):
        refuse(f"toolchain {name}: probe output must be a JSON object")
    values = {}
    for key, value in data.items():
        if not isinstance(value, str):
            refuse(f"toolchain {name}: probe value {key} is not a string")
        values[key] = value
    return values


def substitute(expression: object, name: str, values: dict[str, str], where: str) -> str:
    if not isinstance(expression, str):
        refuse(f"toolchain {name}: {where} must be a string")

    def replace(match: re.Match[str]) -> str:
        key = match.group(1)
        if key == "HOME":
            return os.environ.get("HOME") or os.path.expanduser("~")
        if key not in values:
            refuse(f"toolchain {name}: unknown placeholder {{{key}}} in {where}")
        value = values[key]
        if value == "":
            refuse(f"toolchain {name}: placeholder {{{key}}} is empty in {where}")
        return value

    return PLACEHOLDER.sub(replace, expression)


def resolve_mounts(name: str, entry: dict, values: dict[str, str]) -> list[dict]:
    mounts = entry.get("mounts", [])
    if not isinstance(mounts, list):
        refuse(f"toolchain {name}: mounts must be a list")
    resolved = []
    for index, mount in enumerate(mounts):
        where = f"mounts[{index}]"
        if not isinstance(mount, dict):
            refuse(f"toolchain {name}: {where} must be a JSON object")
        path = substitute(mount.get("path"), name, values, f"{where}.path")
        mode = mount.get("mode", "ro")
        create = mount.get("create", False)
        if mode not in ("ro", "rw"):
            refuse(f"toolchain {name}: {where}.mode must be ro or rw")
        if not isinstance(create, bool):
            refuse(f"toolchain {name}: {where}.create must be true or false")
        if create and mode == "ro":
            refuse(f"toolchain {name}: {where} cannot be create:true with mode ro")
        if ":" in path or "\n" in path:
            refuse(f"toolchain {name}: {where}.path contains a colon or newline")
        if not os.path.isabs(path):
            refuse(f"toolchain {name}: {where}.path must be absolute: {path}")
        resolved.append(
            {
                "name": name,
                "path": path,
                "real": os.path.realpath(path),
                "mode": mode,
                "create": create,
            }
        )
    return resolved


def normalize_mounts(mounts: list[dict]) -> list[dict]:
    """Dedupe identical source+mode; refuse a path requested with both modes."""
    by_real: dict[str, dict] = {}
    order: list[str] = []
    for mount in mounts:
        real = mount["real"]
        previous = by_real.get(real)
        if previous is None:
            by_real[real] = mount
            order.append(real)
        elif previous["mode"] != mount["mode"]:
            refuse(
                f"mount {real} is requested both {previous['mode']} ({previous['name']}) "
                f"and {mount['mode']} ({mount['name']})"
            )
    unique = [by_real[real] for real in order]
    for outer in unique:
        prefix = outer["real"].rstrip("/") + "/"
        for inner in unique:
            if inner is outer:
                continue
            if inner["real"].startswith(prefix) and outer["mode"] != inner["mode"]:
                refuse(
                    f"mount {inner['real']} ({inner['mode']}) is inside "
                    f"{outer['real']} ({outer['mode']})"
                )
    return unique


def check_mounts_exist(mounts: list[dict]) -> None:
    for mount in mounts:
        if not mount["create"] and not os.path.exists(mount["path"]):
            refuse(f"mount path does not exist: {mount['path']}")


def create_mount_dirs(mounts: list[dict]) -> None:
    for mount in mounts:
        if not mount["create"] or os.path.isdir(mount["path"]):
            continue
        try:
            os.makedirs(mount["path"], exist_ok=True)
        except OSError as error:
            refuse(f"cannot create mount directory {mount['path']}: {error.strerror or error}")


def resolve_env(chosen: list[tuple[str, dict]], values_by_name: dict[str, dict[str, str]]) -> dict[str, str]:
    env: dict[str, str] = {}
    owner: dict[str, str] = {}
    for name, entry in chosen:
        table = entry.get("env", {})
        if not isinstance(table, dict):
            refuse(f"toolchain {name}: env must be a JSON object")
        for key, expression in table.items():
            if not key or "=" in key or "\n" in key:
                refuse(f"toolchain {name}: env name {key!r} is not a valid name")
            value = substitute(expression, name, values_by_name[name], f"env.{key}")
            if key in env and env[key] != value:
                refuse(
                    f"env {key} is set by {owner[key]} and {name} to different values"
                )
            env[key] = value
            owner[key] = name
    return env


def resolve_path_prepend(
    chosen: list[tuple[str, dict]], values_by_name: dict[str, dict[str, str]]
) -> list[str]:
    prepend: list[str] = []
    for name, entry in chosen:
        entries = entry.get("path_prepend", [])
        if not isinstance(entries, list):
            refuse(f"toolchain {name}: path_prepend must be a list")
        for index, expression in enumerate(entries):
            value = substitute(expression, name, values_by_name[name], f"path_prepend[{index}]")
            if value not in prepend:
                prepend.append(value)
    return prepend


def check_require(chosen: list[tuple[str, dict]], values_by_name: dict[str, dict[str, str]]) -> None:
    for name, entry in chosen:
        required = entry.get("require", [])
        if not isinstance(required, list) or not all(isinstance(key, str) for key in required):
            refuse(f"toolchain {name}: require must be a list of strings")
        for key in required:
            if not values_by_name[name].get(key):
                refuse(f"toolchain {name}: requires {{{key}}} but the probe did not provide it")


def resolve_domains(chosen: list[tuple[str, dict]]) -> list[str]:
    domains: list[str] = []
    for name, entry in chosen:
        declared = entry.get("allow_domains", [])
        if not isinstance(declared, list):
            refuse(f"toolchain {name}: allow_domains must be a list")
        for value in declared:
            if not isinstance(value, str):
                refuse(f"toolchain {name}: allow_domains entries must be strings")
            if not HOST_NAME.match(value):
                refuse(f"toolchain {name}: allow_domains entry is not a host name: {value}")
            if value not in domains:
                domains.append(value)
    return domains


def emit(mounts: list[dict], env: dict[str, str], prepend: list[str], base_path: str) -> None:
    tokens: list[str] = []
    for mount in mounts:
        tokens += ["-v", f"{mount['path']}:{mount['path']}:{mount['mode']}"]
    for key, value in env.items():
        tokens += ["--env", f"{key}={value}"]
    if prepend:
        tokens += ["--env", f"PATH={':'.join(prepend)}:{base_path}"]
    if tokens:
        sys.stdout.write("\0".join(tokens) + "\0")


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument("--config", required=True, help="toolchains.json")
    parser.add_argument("--name", required=True, help="toolchain name[,NAME...]")
    parser.add_argument("--domains", action="store_true", help="print allow_domains only")
    parser.add_argument("--container-path", default=DEFAULT_BASE_PATH, help="the image's PATH")
    args = parser.parse_args(argv)

    try:
        config = load_config(args.config)
        chosen = chosen_entries(config, args.name.split(","))
        if args.domains:
            print(",".join(resolve_domains(chosen)))
            return 0
        values_by_name = {name: run_probe(name, entry) for name, entry in chosen}
        check_require(chosen, values_by_name)
        mounts = normalize_mounts(
            [
                mount
                for name, entry in chosen
                for mount in resolve_mounts(name, entry, values_by_name[name])
            ]
        )
        check_mounts_exist(mounts)
        create_mount_dirs(mounts)
        emit(
            mounts,
            resolve_env(chosen, values_by_name),
            resolve_path_prepend(chosen, values_by_name),
            args.container_path,
        )
        return 0
    except Refusal as refusal:
        print(f"[agent-in-podman] --toolchain: {refusal}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
