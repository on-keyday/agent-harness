#!/usr/bin/env python3
"""Unit tests for scripts/agent_presets.py.

Pure stdlib, imports agent_presets directly (not runner.py) so this runs
without scripts/.venv or psutil — see agent_presets.py's module docstring.

Run directly::

    python3 scripts/test_agent_presets.py
"""

from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from agent_presets import AgentsPresetError, expand_agents_preset  # noqa: E402


class ExpandAgentsPresetTest(unittest.TestCase):
    def test_claude_codex_expansion(self) -> None:
        out = expand_agents_preset("claude,codex", [])

        # First name (claude) becomes the default profile: --agent-bin +
        # the three argv-template flags, all as flag-string values.
        self.assertIn("--agent-bin", out)
        self.assertEqual(out[out.index("--agent-bin") + 1], "claude")
        self.assertEqual(
            out[out.index("--agent-oneshot-argv") + 1],
            "--output-format stream-json --verbose {args} -p {prompt}",
        )
        self.assertEqual(
            out[out.index("--agent-resume-oneshot-argv") + 1],
            "--output-format stream-json --verbose {args} --continue -p {prompt}",
        )
        self.assertEqual(
            out[out.index("--agent-resume-interactive-argv") + 1],
            "{args} --continue",
        )

        # Remaining names (codex) go into a single --agent-profiles JSON
        # array flag, argv fields as JSON string arrays.
        self.assertIn("--agent-profiles", out)
        profiles = json.loads(out[out.index("--agent-profiles") + 1])
        self.assertEqual(len(profiles), 1)
        codex = profiles[0]
        self.assertEqual(codex["name"], "codex")
        self.assertEqual(codex["bin"], "codex")
        self.assertEqual(
            codex["oneshotArgv"], ["exec", "--json", "{args}", "{prompt}"]
        )
        self.assertEqual(
            codex["resumeOneshotArgv"],
            ["exec", "--json", "resume", "--last", "{args}", "{prompt}"],
        )
        self.assertEqual(
            codex["resumeInteractiveArgv"], ["resume", "--last", "{args}"]
        )

    def test_agy_preset_argv(self) -> None:
        out = expand_agents_preset("claude,agy", [])
        profiles = json.loads(out[out.index("--agent-profiles") + 1])
        self.assertEqual(len(profiles), 1)
        agy = profiles[0]
        self.assertEqual(agy["name"], "agy")
        self.assertEqual(agy["bin"], "agy")
        self.assertEqual(
            agy["oneshotArgv"], ["{args}", "--print", "{prompt}"]
        )
        self.assertEqual(
            agy["resumeOneshotArgv"],
            ["{args}", "--continue", "--print", "{prompt}"],
        )
        self.assertEqual(agy["resumeInteractiveArgv"], ["{args}", "--continue"])
        # agentlog has no decoder for agy's stream-json schema, so the preset
        # must NOT request structured output nor claim a decoder.
        self.assertEqual(agy["logFormat"], "")

    def test_single_agent_emits_no_profiles_flag(self) -> None:
        out = expand_agents_preset("claude", [])
        self.assertNotIn("--agent-profiles", out)
        self.assertEqual(out[out.index("--agent-bin") + 1], "claude")

    def test_bash_preset_argv_only_no_worktree_or_roots_injected(self) -> None:
        out = expand_agents_preset("bash", [])
        self.assertEqual(out[out.index("--agent-bin") + 1], "bash")
        self.assertNotIn("--no-worktree", out)
        self.assertNotIn("--roots", out)

    def test_unknown_agent_rejected_not_fabricated(self) -> None:
        with self.assertRaises(AgentsPresetError) as ctx:
            expand_agents_preset("gemini", [])
        msg = str(ctx.exception)
        self.assertIn("gemini", msg)
        self.assertIn("--agent-profiles", msg)

    def test_conflict_with_explicit_agent_bin_rejected(self) -> None:
        with self.assertRaises(AgentsPresetError):
            expand_agents_preset("claude,codex", ["--agent-bin", "claude"])

    def test_conflict_with_explicit_agent_profiles_rejected(self) -> None:
        with self.assertRaises(AgentsPresetError):
            expand_agents_preset("claude,codex", ["--agent-profiles", "[]"])

    def test_conflict_with_agent_oneshot_argv_eq_form_rejected(self) -> None:
        with self.assertRaises(AgentsPresetError):
            expand_agents_preset(
                "claude,codex", ["--agent-oneshot-argv={args} -p {prompt}"]
            )

    def test_no_conflict_with_unrelated_flags(self) -> None:
        out = expand_agents_preset(
            "claude,codex", ["--server-cid", "ws:127.0.0.1:8539-*", "--max-tasks", "8"]
        )
        self.assertIn("--agent-bin", out)

    def test_claude_default_profile_requests_stream_json(self) -> None:
        out = expand_agents_preset("claude", [])
        self.assertIn("--agent-log-format", out)
        self.assertEqual(
            out[out.index("--agent-log-format") + 1], "claude-stream-json"
        )
        oneshot = out[out.index("--agent-oneshot-argv") + 1]
        # Flags precede {args} so a per-task --claude-args can still override them.
        self.assertTrue(
            oneshot.startswith("--output-format stream-json --verbose {args}")
        )
        self.assertTrue(oneshot.endswith("-p {prompt}"))
        resume = out[out.index("--agent-resume-oneshot-argv") + 1]
        self.assertTrue(
            resume.startswith("--output-format stream-json --verbose {args}")
        )
        self.assertIn("--continue", resume)

    def test_codex_extra_profile_carries_log_format(self) -> None:
        out = expand_agents_preset("claude,codex", [])
        profiles = json.loads(out[out.index("--agent-profiles") + 1])
        codex = next(p for p in profiles if p["name"] == "codex")
        self.assertEqual(codex["logFormat"], "codex-jsonl")
        self.assertEqual(codex["oneshotArgv"][:2], ["exec", "--json"])
        self.assertEqual(codex["resumeOneshotArgv"][:2], ["exec", "--json"])

    def test_bash_preset_has_no_log_format(self) -> None:
        # bash is a shell sandbox, not a conversational agent: nothing to decode.
        out = expand_agents_preset("bash", [])
        self.assertEqual(out[out.index("--agent-log-format") + 1], "")
        self.assertNotIn("--agent-profiles", out)

    def test_sandbox_preset_matches_claude_except_bin(self) -> None:
        # The sandbox runs the same claude through a pass-through wrapper, so
        # every argv/logFormat value must equal claude's — this is what keeps
        # one-shot progress streaming instead of arriving as one final blob.
        sandbox = expand_agents_preset("sandbox", [])
        claude = expand_agents_preset("claude", [])
        for flag in (
            "--agent-oneshot-argv",
            "--agent-resume-oneshot-argv",
            "--agent-resume-interactive-argv",
            "--agent-log-format",
        ):
            self.assertEqual(
                sandbox[sandbox.index(flag) + 1],
                claude[claude.index(flag) + 1],
                f"{flag} drifted from the claude preset",
            )

        # A preset bin may be a path, not only a bare command name.
        bin_path = Path(sandbox[sandbox.index("--agent-bin") + 1])
        self.assertTrue(bin_path.is_absolute())
        self.assertEqual(bin_path.name, "claude-in-podman.sh")
        # Resolved against this module, so a runner started from any checkout
        # gets the wrapper that ships beside the presets it just expanded.
        self.assertTrue(bin_path.exists(), f"{bin_path} does not exist")


if __name__ == "__main__":
    unittest.main()
