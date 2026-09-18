#!/usr/bin/env python3
"""Offline renderer for agentboard message records.

`harness-cli agent inbox --json` emits one JSON record per message, but a
body whose bytes are neither JSON nor UTF-8 arrives ONLY as `payload_b64` -
unreadable by eye and easily confabulated. This tool reads that JSON Lines
stream on stdin and writes a transcript a reader can actually read: JSON is
pretty-printed, UTF-8 is decoded, and a body that genuinely cannot be text
says so explicitly (with a hex dump) instead of being shown as base64.

Usage:

    harness-cli agent inbox --json | python3 examples/board-render/board_render.py

Standard library only. Exit status is 1 if any input line was malformed,
0 otherwise.
"""
from __future__ import annotations

import base64
import binascii
import json
import sys

BODY_INDENT = "    "        # every body line is indented by 4 spaces
DEPTH_INDENT = "  "         # +2 spaces per nesting level for in-stream replies
HEX_BYTES_PER_LINE = 16
HEX_DUMP_MAX_BYTES = 64


def _header(rec: dict) -> str:
    sender = rec.get("from") or {}
    agent = sender.get("agent") or ""
    hostname = sender.get("hostname", "")
    task_id = str(sender.get("task_id", ""))
    line = f"#{rec.get('seq', 0)} {rec.get('topic', '')}  from={agent or '?'}@{hostname} task={task_id[:8]}"
    reply_to_topic = rec.get("reply_to_topic")
    if reply_to_topic:
        line += f"  reply-to={reply_to_topic}"
    in_reply_to = rec.get("in_reply_to", 0)
    if in_reply_to:
        line += f"  in-reply-to=#{in_reply_to}"
    return line


def _hex_dump(data: bytes) -> list[str]:
    chunks = [data[i:i + HEX_BYTES_PER_LINE]
              for i in range(0, min(len(data), HEX_DUMP_MAX_BYTES), HEX_BYTES_PER_LINE)]
    return [" ".join(f"{b:02x}" for b in chunk) for chunk in chunks]


def _body_text(rec: dict) -> str:
    """Select and decode the body per the priority rules; raw base64 never
    survives to the output."""
    if rec.get("payload_omitted"):
        return (f"<body omitted: {rec.get('payload_bytes', 0)} bytes"
                f" - run: {rec.get('read_with', '')}>")
    if "payload" in rec:
        return json.dumps(rec["payload"], indent=2, ensure_ascii=False)
    if "payload_text" in rec:
        return rec["payload_text"]
    if "payload_b64" in rec:
        try:
            data = base64.b64decode(rec["payload_b64"])
        except (binascii.Error, ValueError):
            return "<empty body>"
        try:
            return data.decode("utf-8")
        except UnicodeDecodeError:
            dump = "\n".join(_hex_dump(data))
            return f"<non-text body: {len(data)} bytes>\n{dump}"
    return "<empty body>"


def _indent_block(text: str, prefix: str) -> list[str]:
    """Split a body into lines, indenting each; empty lines stay empty so no
    trailing whitespace is emitted. One trailing newline is not a line."""
    lines = text.split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    return [prefix + line if line else "" for line in lines]


def render_lines(lines: list[str]) -> tuple[str, list[str], int]:
    """Render a stream of input lines.

    Returns (stdout text, stderr lines, exit status). Output order equals
    input order; malformed lines are reported on stderr and skipped.
    """
    out: list[str] = []
    err: list[str] = []
    depths: dict[int, int] = {}  # seq -> rendered depth, for replies seen so far

    for lineno, raw in enumerate(lines, start=1):
        line = raw.rstrip("\n")
        try:
            rec = json.loads(line)
        except ValueError:
            err.append(f"line {lineno}: not JSON: {line[:80]}")
            continue
        if not isinstance(rec, dict):
            err.append(f"line {lineno}: not JSON: {line[:80]}")
            continue

        in_reply_to = rec.get("in_reply_to", 0)
        depth = depths.get(in_reply_to, -1) + 1 if in_reply_to else 0
        seq = rec.get("seq", 0)
        depths[seq] = depth

        pad = DEPTH_INDENT * depth
        out.append(pad + _header(rec))
        out.extend(_indent_block(_body_text(rec), pad + BODY_INDENT))

    return "\n".join(out) + ("\n" if out else ""), err, 1 if err else 0


def main() -> int:
    text, err, status = render_lines(sys.stdin.readlines())
    sys.stdout.write(text)
    for line in err:
        print(line, file=sys.stderr)
    return status


if __name__ == "__main__":
    sys.exit(main())
