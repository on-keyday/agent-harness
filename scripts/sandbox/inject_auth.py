"""Credential injection (agent-in-podman.sh --inject-auth): read the host
agent's credential and write the config that points the agent at the
in-container injector (cmd/sandbox-connect-proxy/inject.go) instead.

    inject_auth.py pi --agent-dir ~/.pi/agent --config-out FILE --port 18081

On success FILE holds the agent's config and stdout carries two lines: the
upstream origin the injector forwards to, then the credential. stdout is read
by the wrapper into a shell variable, never onto an argv.

    inject_auth.py claude --credentials ~/.claude/.credentials.json

For an agent injected through the host relay, which reads the credential
itself: stdout carries NAME=VALUE lines of the agent environment the relay
cannot supply, and nothing secret.

Anything the injector cannot serve is a refusal (exit 2) naming the reason,
never a partial config.
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


def claude(credentials):
    """Return the environment lines for a claude behind the relay.

    The container's claude authenticates with a placeholder
    CLAUDE_CODE_OAUTH_TOKEN, and with that alone it knows nothing of the
    subscription: it labels itself "Claude API" and picks that tier's default
    model (claude-sonnet-5-5 where the host's claude picks claude-opus-5-5).
    It does not fetch the profile to find out — a placeholder credential FILE
    makes it try, directly at api.anthropic.com and past the base URL, and
    fail. These two variables are what the binary reads instead (claude
    2.1.312; neither is documented): with them the one-shot ran on the host's
    default model (measured 2026-10-29). Copied from the host's credential
    file, which holds them beside the token; they are not secret.
    """
    try:
        oauth = json.loads(credentials.read_text()).get("claudeAiOauth") or {}
    except FileNotFoundError:
        raise Refusal(f"no claude credential file at {credentials} (is claude logged in with /login?)")
    except (OSError, ValueError) as e:
        raise Refusal(f"cannot read {credentials}: {e}")
    if not oauth.get("accessToken"):
        raise Refusal(f"{credentials} holds no claudeAiOauth login")
    env = []
    for field, name in (("subscriptionType", "CLAUDE_CODE_SUBSCRIPTION_TYPE"), ("rateLimitTier", "CLAUDE_CODE_RATE_LIMIT_TIER")):
        v = oauth.get(field)
        if isinstance(v, str) and v and "\n" not in v:
            env.append(f"{name}={v}")
    return env


def main(argv=None):
    ap = argparse.ArgumentParser(prog="inject_auth.py")
    sub = ap.add_subparsers(dest="agent", required=True)
    p = sub.add_parser("pi")
    p.add_argument("--agent-dir", required=True, type=Path)
    p.add_argument("--config-out", required=True, type=Path)
    p.add_argument("--port", required=True, type=int)
    c = sub.add_parser("claude")
    c.add_argument("--credentials", required=True, type=Path)
    args = ap.parse_args(argv)
    try:
        if args.agent == "claude":
            sys.stdout.write("".join(f"{e}\n" for e in claude(args.credentials)))
            return 0
        config, upstream, key = pi(args.agent_dir, args.port)
    except Refusal as e:
        print(f"[inject_auth] {e}", file=sys.stderr)
        return 2
    args.config_out.write_text(json.dumps(config, indent=2) + "\n")
    sys.stdout.write(f"{upstream}\n{key}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
