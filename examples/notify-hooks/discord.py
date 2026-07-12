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
   *server* runs. Either way the URL stays out of this (public) repo:

       chmod +x examples/notify-hooks/discord.py

       # option A (recommended — survives server restarts): a file that holds
       # the URL (set once, gitignored), passed as an argument:
       printf '%s' 'https://discord.com/api/webhooks/XXXX/YYYY' > /secret/discord-url
       chmod 600 /secret/discord-url
       # then --notify-hook '/abs/discord.py --url-file /secret/discord-url'

       # option B — env vars, inherited from the server's environment (these
       # are forgotten when the server is respawned from a clean shell):
       export DISCORD_WEBHOOK_URL='https://discord.com/api/webhooks/XXXX/YYYY'
       # or point at the URL file instead:
       export DISCORD_WEBHOOK_FILE=/secret/discord-url

   Resolution order is --url, --url-file, DISCORD_WEBHOOK_URL, then
   DISCORD_WEBHOOK_FILE. There is no hardcoded default path.

3. Start the server with the hook (absolute path; invoked directly, no shell;
   the hook command line is whitespace-split, so the args work but no path in
   it may contain spaces). To make the hook survive restarts, write the same
   command line into `<data-dir>/notify-hook` once instead of passing the flag:

       harness-server --listen <addr> \
           --notify-hook '/abs/discord.py --url-file /secret/discord-url'
       # or, persistent across restarts:
       echo '/abs/discord.py --url-file /secret/discord-url' > harness-data/notify-hook

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

# Discord / its Cloudflare front reject the default urllib User-Agent
# ("Python-urllib/x.y") with 403, so send an explicit one.
USER_AGENT = "harness-notify/1.0 (+https://github.com/on-keyday/agent-harness)"


def _read_url_file(path: str, label: str) -> str:
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read().strip()
    except OSError as e:
        print(f"discord notify: cannot read {label} {path!r}: {e}", file=sys.stderr)
        return ""


def resolve_url(argv: list[str]) -> str:
    """Resolve the webhook URL: --url VALUE, --url-file PATH (argv, preferred —
    argv comes from the persisted notify-hook command line, so it survives
    server restarts), then DISCORD_WEBHOOK_URL / DISCORD_WEBHOOK_FILE env. No
    secret is hardcoded and no default path is assumed, so nothing leaks into
    the (public) repo. Returns "" when nothing is configured."""
    i = 0
    while i < len(argv):
        if argv[i] == "--url" and i + 1 < len(argv):
            url = argv[i + 1].strip()
            if url:
                return url
        elif argv[i] == "--url-file" and i + 1 < len(argv):
            url = _read_url_file(argv[i + 1], "--url-file")
            if url:
                return url
        i += 1
    url = os.environ.get("DISCORD_WEBHOOK_URL", "").strip()
    if url:
        return url
    path = os.environ.get("DISCORD_WEBHOOK_FILE", "").strip()
    if path:
        return _read_url_file(path, "DISCORD_WEBHOOK_FILE")
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
    url = resolve_url(sys.argv[1:])
    if not url:
        print(
            "discord notify: no webhook URL "
            "(pass --url URL / --url-file PATH, or set DISCORD_WEBHOOK_URL / "
            "DISCORD_WEBHOOK_FILE)",
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
        url,
        data=data,
        headers={"Content-Type": "application/json", "User-Agent": USER_AGENT},
        method="POST",
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
