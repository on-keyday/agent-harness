#!/usr/bin/env python3
"""Discord webhook notifier for the harness `--notify-hook`.

The harness server invokes the configured `--notify-hook` command once per
`harness-cli notify`, passing the notification as a JSON object on **stdin**
(plus HARNESS_NOTIFY_* convenience env vars). This script formats it as a
Discord embed and POSTs it to a webhook. It is fire-and-forget from the server's
side — a non-zero exit is logged but does not affect the notify response.

Setup
-----
1. Create a Discord webhook: Server Settings -> Integrations -> Webhooks ->
   New Webhook, then "Copy Webhook URL".
2. Make this file executable and provide the webhook URL where the harness
   *server* runs (its environment is inherited by the hook). Either way the URL
   stays out of this (public) repo:

       chmod +x examples/notify-hooks/discord.py

       # option A — env var:
       export DISCORD_WEBHOOK_URL='https://discord.com/api/webhooks/XXXX/YYYY'

       # option B — a file that holds the URL (set once, gitignored), and point
       # an env var at its path:
       printf '%s' 'https://discord.com/api/webhooks/XXXX/YYYY' > /secret/discord-url
       chmod 600 /secret/discord-url
       export DISCORD_WEBHOOK_FILE=/secret/discord-url

   Resolution order is DISCORD_WEBHOOK_URL, then DISCORD_WEBHOOK_FILE. There is
   no hardcoded default path.

3. Start the server with the hook (absolute path; invoked directly, no shell):

       harness-server --listen <addr> --notify-hook /abs/path/to/discord.py

   Then `harness-cli notify --level warn "needs your call"` reaches Discord.

No secret lives in this file — the URL comes from the environment (directly or
via a file the env points at), so it is safe to commit. To use a different sink
(ntfy, Telegram, Slack, …), copy this file and swap the `build_payload` /
endpoint; the stdin contract is identical.

stdin JSON contract (every field may be absent)
-----------------------------------------------
    level     "info" | "warn" | "error"
    origin    "worker" | "external"
    title     short heading (may be empty)
    text      body
    task_id   32-hex task id        ) present only when origin == "worker"
    runner_id runner connection id  )
    repo      worktree repo path    )
    hostname  runner host           )
    conn_id   sending connection id
    ts        unix seconds
"""

import datetime
import json
import os
import sys
import urllib.request

# Discord embed colour (decimal RGB) per level.
COLORS = {
    "info": 0x3498DB,   # blue
    "warn": 0xF1C40F,   # yellow
    "error": 0xE74C3C,  # red
}


def resolve_url() -> str:
    """Resolve the webhook URL: DISCORD_WEBHOOK_URL, else the contents of the
    file at DISCORD_WEBHOOK_FILE. No secret is hardcoded and no default path is
    assumed — both sources come from the environment, so nothing leaks into the
    (public) repo. Returns "" when neither is configured."""
    url = os.environ.get("DISCORD_WEBHOOK_URL", "").strip()
    if url:
        return url
    path = os.environ.get("DISCORD_WEBHOOK_FILE", "").strip()
    if path:
        try:
            with open(path, "r", encoding="utf-8") as f:
                return f.read().strip()
        except OSError as e:
            print(
                f"discord notify: cannot read DISCORD_WEBHOOK_FILE {path!r}: {e}",
                file=sys.stderr,
            )
    return ""


def build_payload(ev: dict) -> dict:
    """Turn a notify event dict into a Discord webhook payload."""
    level = str(ev.get("level") or "info")
    title = (ev.get("title") or "").strip()
    text = (ev.get("text") or "").strip()
    origin = ev.get("origin") or ""

    # Source line, e.g. "gmkhost · task 0f0d4dd6 · remote-agent-harness", or
    # "external" for an out-of-worker ping.
    if origin == "worker":
        bits = []
        host = (ev.get("hostname") or "").strip()
        task = (ev.get("task_id") or "")  # full id — copy-pasteable for task addressing
        repo = os.path.basename((ev.get("repo") or "").rstrip("/"))
        if host:
            bits.append(host)
        if task:
            bits.append(f"task {task}")
        if repo:
            bits.append(repo)
        source = " · ".join(bits) or "worker"
    else:
        source = "external"

    embed = {"color": COLORS.get(level, COLORS["info"])}
    if title:
        embed["title"] = title
    embed["description"] = text or "*(no body)*"
    embed["fields"] = [
        {"name": "level", "value": level, "inline": True},
        {"name": "source", "value": source, "inline": True},
    ]
    ts = ev.get("ts")
    if isinstance(ts, (int, float)) and ts > 0:
        embed["timestamp"] = datetime.datetime.fromtimestamp(
            ts, datetime.timezone.utc
        ).isoformat()

    return {"username": "harness", "embeds": [embed]}


def main() -> int:
    url = resolve_url()
    if not url:
        print(
            "discord notify: no webhook URL "
            "(set DISCORD_WEBHOOK_URL, or DISCORD_WEBHOOK_FILE to a file holding it)",
            file=sys.stderr,
        )
        return 1

    try:
        ev = json.load(sys.stdin)
        if not isinstance(ev, dict):
            raise ValueError("stdin is not a JSON object")
    except Exception as e:  # noqa: BLE001 — any parse failure is fatal for the hook
        print(f"discord notify: bad stdin JSON: {e}", file=sys.stderr)
        return 1

    data = json.dumps(build_payload(ev)).encode("utf-8")
    req = urllib.request.Request(
        url, data=data, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(req, timeout=8) as resp:
            # Discord returns 204 No Content on success.
            if resp.status not in (200, 204):
                body = resp.read().decode("utf-8", "replace")[:500]
                print(
                    f"discord notify: webhook returned {resp.status}: {body}",
                    file=sys.stderr,
                )
                return 1
    except Exception as e:  # noqa: BLE001 — network/HTTP errors should not crash
        print(f"discord notify: POST failed: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
