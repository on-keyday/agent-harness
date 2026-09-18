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

# C0 control bytes a body is allowed to carry verbatim; everything else
# (including \x7f and the C1 range U+0080-U+009F) is rendered as a visible
# \xNN escape so a hostile body can neither repaint nor erase the reader's
# terminal. C1 is included because U+009B is CSI: a terminal honouring 8-bit
# controls treats it exactly like ESC-[, so it must not be left to the emulator.
VERBATIM_CONTROLS = {"\n", "\t"}


def _sanitize(text: str) -> str:
    r"""Make control characters visible instead of sending them to the terminal.

    Every C0 byte other than newline and tab, DEL (\x7f), and the C1 range
    (U+0080-U+009F, e.g. U+009B = CSI) renders as its literal \xNN escape. This
    applies to body text AND header fields: a body
    arrives from an untrusted peer, and the renderer's whole job is to make
    those bytes safe to read.
    """
    return "".join(
        ch if ch in VERBATIM_CONTROLS
        else f"\\x{ord(ch):02x}" if ord(ch) < 0x20 or 0x7F <= ord(ch) <= 0x9F
        else ch
        for ch in text
    )


def _header(rec: dict) -> str:
    sender = rec.get("from") or {}
    agent = _sanitize(str(sender.get("agent") or ""))
    hostname = _sanitize(str(sender.get("hostname", "")))
    task_id = _sanitize(str(sender.get("task_id", "")))
    topic = _sanitize(str(rec.get("topic", "")))
    line = f"#{rec.get('seq', 0)} {topic}  from={agent or '?'}@{hostname} task={task_id[:8]}"
    reply_to_topic = rec.get("reply_to_topic")
    if reply_to_topic:
        line += f"  reply-to={_sanitize(str(reply_to_topic))}"
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
        # json.dumps escapes C0 inside strings, but with ensure_ascii=False it
        # leaves C1 (U+0080-U+009F) raw - so the rendered JSON still goes
        # through _sanitize to keep the terminal-safety invariant unconditional.
        return _sanitize(json.dumps(rec["payload"], indent=2, ensure_ascii=False))
    if "payload_text" in rec:
        return _sanitize(rec["payload_text"])
    if "payload_b64" in rec:
        try:
            data = base64.b64decode(rec["payload_b64"])
        except (binascii.Error, ValueError):
            return "<empty body>"
        if not data:
            return "<empty body>"
        try:
            return _sanitize(data.decode("utf-8"))
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
        body = _body_text(rec)
        if not body:
            body = "<empty body>"
        out.extend(_indent_block(body, pad + BODY_INDENT))

    return "\n".join(out) + ("\n" if out else ""), err, 1 if err else 0


def main() -> int:
    text, err, status = render_lines(sys.stdin.readlines())
    sys.stdout.write(text)
    for line in err:
        print(line, file=sys.stderr)
    return status


if __name__ == "__main__":
    sys.exit(main())
