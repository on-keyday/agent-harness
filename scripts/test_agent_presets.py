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
            out[out.index("--agent-oneshot-argv") + 1], "{args} -p {prompt}"
        )
        self.assertEqual(
            out[out.index("--agent-resume-oneshot-argv") + 1],
            "{args} --continue -p {prompt}",
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
        self.assertEqual(codex["oneshotArgv"], ["exec", "{args}", "{prompt}"])
        self.assertEqual(
            codex["resumeOneshotArgv"],
            ["exec", "resume", "--last", "{args}", "{prompt}"],
        )
        self.assertEqual(
            codex["resumeInteractiveArgv"], ["resume", "--last", "{args}"]
        )

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


if __name__ == "__main__":
    unittest.main()
