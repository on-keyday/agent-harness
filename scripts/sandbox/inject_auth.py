"""Credential injection (agent-in-podman.sh --inject-auth): read the host
agent's credential and write the config that points the agent at the
in-container injector (cmd/sandbox-connect-proxy/inject.go) instead.

    inject_auth.py pi --agent-dir ~/.pi/agent --config-out FILE --port 18081

On success FILE holds the agent's config and stdout carries two lines: the
upstream origin the injector forwards to, then the credential. stdout is read
by the wrapper into a shell variable, never onto an argv. Anything the
injector cannot serve is a refusal (exit 2) naming the reason, never a
partial config.
"""

import argparse
import json
from pathlib import Path
import sys
from urllib.parse import urlsplit

PLACEHOLDER = "sandbox-placeholder"

# Base URLs of pi's built-in providers, for the ones measured through the
# injector. pi 1.4.0 pi-ai/dist/providers: openrouter's is the one below
# (measured 2026-10-29: a one-shot through the injector answered, 200).
PI_BUILTIN_BASE = {
    "openrouter": "https://openrouter.ai/api/v1",
}


class Refusal(Exception):
    pass


def _load(path, what):
    try:
        return json.loads(path.read_text())
    except FileNotFoundError:
        raise Refusal(f"no {what} at {path}")
    except (OSError, ValueError) as e:
        raise Refusal(f"cannot read {what} {path}: {e}")


def pi(agent_dir, port):
    """Return (config, upstream origin, key) for pi's default provider.

    pi resolves a provider's credential from auth.json first and falls back
    to the provider's models.json apiKey only when auth.json has none
    (pi-ai dist/auth/resolve.js), so the container gets no auth.json and a
    models.json whose apiKey is a placeholder. baseUrl on a built-in provider
    applies to all of its built-in models (docs/models-configuration.md).
    """
    settings = _load(agent_dir / "settings.json", "pi settings")
    provider = settings.get("defaultProvider")
    if not provider:
        raise Refusal(f"{agent_dir}/settings.json names no defaultProvider")
    entry = _load(agent_dir / "auth.json", "pi credential file").get(provider)
    if not isinstance(entry, dict):
        raise Refusal(f"{agent_dir}/auth.json holds no credential for {provider}")
    if entry.get("type") != "api_key":
        # An OAuth credential expires and is refreshed by pi itself, which a
        # container without it cannot do; the injector does not refresh.
        raise Refusal(f"the {provider} credential is of type {entry.get('type')!r}; only an api_key can be injected")
    key = entry.get("key")
    if not isinstance(key, str) or not key or "\n" in key:
        raise Refusal(f"the {provider} credential has no usable key")

    models_path = agent_dir / "models.json"
    config = _load(models_path, "pi models config") if models_path.exists() else {}
    prov = config.setdefault("providers", {}).setdefault(provider, {})
    for model in prov.get("models", []):
        if isinstance(model, dict) and "baseUrl" in model:
            raise Refusal(f"{models_path}: a {provider} model sets its own baseUrl, which would bypass the injector")
    base = prov.get("baseUrl") or PI_BUILTIN_BASE.get(provider)
    if not base:
        raise Refusal(f"no known base URL for pi provider {provider} (known: {', '.join(sorted(PI_BUILTIN_BASE))})")
    u = urlsplit(base)
    if u.scheme != "https" or not u.netloc:
        raise Refusal(f"{provider} base URL {base} is not https")
    prov["baseUrl"] = f"http://127.0.0.1:{port}{u.path.rstrip('/')}"
    prov["apiKey"] = PLACEHOLDER
    return config, f"https://{u.netloc}", key


AGENTS = {"pi": pi}


def main(argv=None):
    ap = argparse.ArgumentParser(prog="inject_auth.py")
    ap.add_argument("agent", choices=sorted(AGENTS))
    ap.add_argument("--agent-dir", required=True, type=Path)
    ap.add_argument("--config-out", required=True, type=Path)
    ap.add_argument("--port", required=True, type=int)
    args = ap.parse_args(argv)
    try:
        config, upstream, key = AGENTS[args.agent](args.agent_dir, args.port)
    except Refusal as e:
        print(f"[inject_auth] {e}", file=sys.stderr)
        return 2
    args.config_out.write_text(json.dumps(config, indent=2) + "\n")
    sys.stdout.write(f"{upstream}\n{key}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
