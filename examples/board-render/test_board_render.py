#!/usr/bin/env python3
"""Unit tests for examples/board-render/board_render.py.

Each test pins one rendering rule from the spec: body selection priority,
header composition, reply nesting, and malformed-line handling. Runs on the
standard library alone::

    python3 examples/board-render/test_board_render.py
"""
from __future__ import annotations

import base64
import json
import subprocess
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import board_render  # noqa: E402


def assert_terminal_safe(out: str, testcase: unittest.TestCase) -> None:
    """The terminal-safety invariant: no C0 other than \n/\t, no DEL, no C1."""
    for ch in out:
        testcase.assertTrue(
            ch in "\n\t" or not (ord(ch) < 0x20 or 0x7F <= ord(ch) <= 0x9F),
            f"raw control byte {ch!r} reached stdout")


def record(**overrides) -> dict:
    """A minimal valid record; tests override fields on top of it."""
    rec = {
        "seq": 1,
        "in_reply_to": 0,
        "topic": "chat.abc12345",
        "from": {
            "runner_id": "r1",
            "task_id": "70fbad4a6eb6f1e992be8a669f1bcefd",
            "hostname": "gmkhost",
            "agent": "claude",
        },
    }
    rec.update(overrides)
    return rec


def render(*records) -> tuple[str, list[str], int]:
    lines = [json.dumps(r) for r in records]
    return board_render.render_lines(lines)


class HeaderTest(unittest.TestCase):
    def test_header_composition(self) -> None:
        out, _, _ = render(record())
        self.assertIn("#1 chat.abc12345  from=claude@gmkhost task=70fbad4a", out)

    def test_server_originated_agent_is_a_question_mark(self) -> None:
        rec = record()
        rec["from"]["agent"] = ""
        rec["from"]["hostname"] = "server"
        out, _, _ = render(rec)
        self.assertIn("from=?@server", out)

    def test_in_reply_to_appended_when_nonzero(self) -> None:
        out, _, _ = render(record(in_reply_to=41))
        self.assertIn("in-reply-to=#41", out)


class BodySelectionTest(unittest.TestCase):
    def test_json_body_pretty_printed_and_no_base64(self) -> None:
        out, _, status = render(record(payload={"b64": "nope", "msg": "hi"}))
        self.assertEqual(status, 0)
        self.assertIn('{\n      "b64": "nope",\n      "msg": "hi"\n    }', out)
        # the raw record itself must never leak as a line
        self.assertNotIn('"seq": 1,', out)

    def test_prose_body_renders_verbatim(self) -> None:
        text = "line one\nline two with  spacing"
        out, _, _ = render(record(payload_text=text))
        self.assertIn("    line one\n    line two with  spacing", out)

    def test_b64_only_utf8_renders_decoded_text(self) -> None:
        raw = "decoded prose body"
        out, _, _ = render(record(payload_b64=base64.b64encode(raw.encode()).decode()))
        self.assertIn(f"    {raw}", out)
        self.assertNotIn(base64.b64encode(raw.encode()).decode(), out)

    def test_b64_only_non_utf8_renders_non_text_marker_and_hex_dump(self) -> None:
        data = bytes([0xFF, 0xFE, 0x00]) * 30  # 90 bytes, invalid UTF-8
        out, _, _ = render(record(payload_b64=base64.b64encode(data).decode()))
        self.assertIn("<non-text body: 90 bytes>", out)
        # 64-byte cap: first line has 16 space-separated lowercase hex bytes
        self.assertIn("    ff fe 00 ff fe 00 ff fe 00 ff fe 00 ff fe 00 ff", out)
        dump_lines = [l for l in out.split("\n") if l.strip() and
                      all(c in "0123456789abcdef " for c in l.strip())]
        self.assertEqual(len(dump_lines), 4)  # 64 bytes / 16 per line

    def test_omitted_body_renders_read_with_command(self) -> None:
        out, _, _ = render(record(payload_omitted=True, payload_bytes=70000,
                                  read_with="harness-cli agent read 42"))
        self.assertIn("<body omitted: 70000 bytes - run: harness-cli agent read 42>", out)

    def test_no_body_at_all_renders_empty_marker(self) -> None:
        out, _, _ = render(record())
        self.assertIn("<empty body>", out)


class NestingTest(unittest.TestCase):
    def test_reply_to_earlier_seq_is_indented_chains_nest_further(self) -> None:
        out, _, _ = render(
            record(seq=1),
            record(seq=2, in_reply_to=1),
            record(seq=3, in_reply_to=2),
        )
        self.assertIn("  #2 chat.abc12345", out)   # depth 1
        self.assertIn("    #3 chat.abc12345", out)  # depth 2

    def test_reply_to_absent_seq_renders_at_depth_zero(self) -> None:
        out, _, _ = render(record(seq=5, in_reply_to=99))
        self.assertIn("#5 chat.abc12345", out)
        headers = [l for l in out.split("\n") if l.strip().startswith("#")]
        self.assertEqual(headers, ["#5 chat.abc12345  from=claude@gmkhost task=70fbad4a  in-reply-to=#99"])

    def test_reply_to_later_seq_is_also_orphaned(self) -> None:
        out, _, _ = render(record(seq=7, in_reply_to=9), record(seq=9))
        self.assertIn("#7 chat.abc12345", out)
        headers = [l for l in out.split("\n") if l.strip().startswith("#")]
        self.assertEqual(headers[0], "#7 chat.abc12345  from=claude@gmkhost task=70fbad4a  in-reply-to=#9")


class SanitizationTest(unittest.TestCase):
    """Pins the terminal-safety invariant: a body arrives from an untrusted
    peer, so no C0 control byte (except \n and \t) and no \x7f may reach
    stdout, whatever the body holds."""

    def test_ansi_escapes_in_payload_text_render_as_visible_escapes(self) -> None:
        out, _, _ = render(record(payload_text="\x1b[31mRED\x1b[0m\x1b[2Jcleared\x7f\r"))
        assert_terminal_safe(out, self)
        self.assertIn("\\x1b[31mRED\\x1b[0m\\x1b[2Jcleared", out)
        self.assertIn("\\x7f", out)
        self.assertIn("\\x0d", out)

    def test_ansi_escapes_in_decoded_payload_b64_render_as_visible_escapes(self) -> None:
        raw = "\x1b[31mRED\x1b[0m\x1b[2Jcleared"
        out, _, _ = render(record(payload_b64=base64.b64encode(raw.encode()).decode()))
        assert_terminal_safe(out, self)
        self.assertIn("\\x1b[2Jcleared", out)

    def test_c1_controls_render_as_visible_escapes(self) -> None:
        # U+0085 (NEL) breaks lines and U+009B (CSI) is ESC-[ for terminals
        # honouring 8-bit controls; both must be escaped, not left raw.
        out, _, _ = render(record(payload_text="a\u0085b\u009b8mc\u009d"))
        assert_terminal_safe(out, self)
        self.assertIn("a\\x85b\\x9b8mc\\x9d", out)

        out, _, _ = render(record(payload_b64=base64.b64encode("x\u009by".encode()).decode()))
        assert_terminal_safe(out, self)
        self.assertIn("x\\x9by", out)

    def test_newline_and_tab_stay_verbatim_in_bodies(self) -> None:
        out, _, _ = render(record(payload_text="a\tb\nc"))
        self.assertIn("    a\tb\n    c", out)
        self.assertNotIn("\\x09", out)
        self.assertNotIn("\\x0a", out)

    def test_json_body_path_control_chars_are_escaped(self) -> None:
        # json.dumps escapes C0 inside strings, but leaves C1 raw with
        # ensure_ascii=False - so the rendered JSON goes through the sanitizer
        # too. C0 appears as json's \u001b, raw C1 as \x9b.
        out, _, _ = render(record(payload={"msg": "\x1b[2Jcleared\u009b8m"}))
        self.assertIn("\\u001b[2Jcleared\\x9b8m", out)
        assert_terminal_safe(out, self)

    def test_header_fields_are_sanitized(self) -> None:
        rec = record(seq=1)
        rec["from"]["hostname"] = "h\x1b[2J"
        rec["from"]["agent"] = "a\x1b[0m"
        rec["topic"] = "t\x1b"
        out, _, _ = render(rec)
        assert_terminal_safe(out, self)
        self.assertIn("\\x1b", out)


class EmptyBodyTest(unittest.TestCase):
    """Rule 5: an empty selected body must render <empty body>, not silence.
    The production form is payload_b64 == "" (zero-byte payload as emitted by
    cli/agent/json_emit.go), but an empty payload_text or no body key at all
    must render the same."""

    def test_empty_payload_b64_renders_empty_body_marker(self) -> None:
        out, _, _ = render(record(payload_b64=""))
        self.assertIn("    <empty body>", out)

    def test_empty_payload_text_renders_empty_body_marker(self) -> None:
        out, _, _ = render(record(payload_text=""))
        self.assertIn("    <empty body>", out)

    def test_no_body_key_at_all_renders_empty_body_marker(self) -> None:
        out, _, _ = render(record())
        self.assertIn("    <empty body>", out)


class MalformedInputTest(unittest.TestCase):
    def test_malformed_line_goes_to_stderr_others_render_exit_is_1(self) -> None:
        lines = [
            json.dumps(record(seq=1)),
            "this is not json {{{",
            json.dumps(record(seq=2)),
        ]
        out, err, status = board_render.render_lines(lines)
        self.assertEqual(status, 1)
        self.assertEqual(err, ["line 2: not JSON: this is not json {{{"])
        self.assertIn("#1 chat.abc12345", out)
        self.assertIn("#2 chat.abc12345", out)

    def test_clean_stream_exits_zero(self) -> None:
        _, _, status = render(record())
        self.assertEqual(status, 0)

    def test_main_wires_stdin_to_stdout_and_exit_status(self) -> None:
        proc = subprocess.run(
            [sys.executable, str(Path(__file__).resolve().parent / "board_render.py")],
            input=json.dumps(record(payload_text="hello")), capture_output=True,
            text=True,
        )
        self.assertEqual(proc.returncode, 0)
        self.assertIn("#1 chat.abc12345", proc.stdout)
        self.assertIn("    hello", proc.stdout)


class ReplyToTopicTest(unittest.TestCase):
    def test_reply_to_topic_appears_when_present_and_absent_otherwise(self) -> None:
        with_topic, _, _ = render(record(reply_to_topic="chat.70fbad4a"))
        self.assertIn("reply-to=chat.70fbad4a", with_topic)
        without, _, _ = render(record())
        self.assertNotIn("reply-to", without)


if __name__ == "__main__":
    unittest.main()
