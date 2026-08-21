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

    def test_opencode_preset_argv(self) -> None:
        out = expand_agents_preset("claude,opencode", [])
        profiles = json.loads(out[out.index("--agent-profiles") + 1])
        self.assertEqual(len(profiles), 1)
        oc = profiles[0]
        self.assertEqual(oc["name"], "opencode")
        self.assertEqual(oc["bin"], "opencode")
        # `run` is a SUBCOMMAND and the prompt is its positional message, so
        # {prompt} carries no flag of its own — codex's shape, not claude's.
        self.assertEqual(oc["oneshotArgv"], ["run", "{args}", "{prompt}"])
        self.assertEqual(
            oc["resumeOneshotArgv"], ["run", "--continue", "{args}", "{prompt}"]
        )
        # The resume flag must stay on the `run` subcommand for the one-shot
        # and on the bare binary for the TUI — opencode accepts --continue in
        # both places and they are different launch paths.
        self.assertEqual(oc["resumeInteractiveArgv"], ["{args}", "--continue"])
        # agentlog has no decoder for opencode's `--format json` event schema,
        # so the preset must neither request it nor claim a decoder.
        self.assertEqual(oc["logFormat"], "")

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

    def test_claude_preset_supplies_the_event_stream_adapter(self) -> None:
        out = expand_agents_preset("claude", [])
        adapter = Path(out[out.index("--agent-stream-adapter") + 1])
        # Absolute and beside the agent-runner daemon.py launches, so the
        # adapter's protocol_version matches the runner that reads it.
        self.assertTrue(adapter.is_absolute())
        self.assertEqual(adapter.parent.name, "bin")
        self.assertEqual(adapter.stem, "harness-stream-adapter")
        # NOT asserted: that it exists. bin/ is a build artifact, unlike the
        # checked-in sandbox wrappers, so a fresh checkout has none until
        # `make build` — the runner LookPaths it per task and fails that task.

    def test_non_claude_presets_declare_no_stream_adapter(self) -> None:
        # An empty value makes the profile REFUSE an event-stream task. That
        # is a fact about the adapter, which speaks claude's protocol
        # specifically: handing it to codex would append claude's
        # --input-format/--permission-prompt-tool flags to a CLI without them.
        for name in ("codex", "agy", "opencode", "bash"):
            with self.subTest(agent=name):
                out = expand_agents_preset(name, [])
                self.assertEqual(out[out.index("--agent-stream-adapter") + 1], "")

    def test_stream_adapter_flag_always_emitted(self) -> None:
        # Emitted even when empty, so `--dry-run` states that the agent has no
        # adapter instead of leaving it to be inferred from an absent flag.
        for name in ("claude", "codex", "bash"):
            with self.subTest(agent=name):
                self.assertIn("--agent-stream-adapter", expand_agents_preset(name, []))

    def test_extra_profile_carries_stream_adapter(self) -> None:
        # The default profile takes the flag; every OTHER name rides the
        # --agent-profiles JSON, which needs the key or the second claude slot
        # in `--agents codex,claude` would silently lose event-stream support.
        out = expand_agents_preset("codex,claude", [])
        profiles = json.loads(out[out.index("--agent-profiles") + 1])
        claude = next(p for p in profiles if p["name"] == "claude")
        self.assertTrue(claude["streamAdapter"].endswith("harness-stream-adapter"))
        self.assertEqual(out[out.index("--agent-stream-adapter") + 1], "")

    def test_conflict_with_explicit_stream_adapter_rejected(self) -> None:
        # --agents now sets this flag, so it joins the conflict list: the
        # policy is that every flag --agents emits is refused rather than
        # silently overridden or merged.
        with self.assertRaises(AgentsPresetError):
            expand_agents_preset("claude", ["--agent-stream-adapter", "/opt/mine"])

    def test_sandbox_presets_derive_their_base_agent(self) -> None:
        # Each sandbox-* preset runs its BASE agent through a pass-through
        # wrapper, so its argv must be the base's with only the wrapper's own
        # --sandbox-agent selector prefixed. Anything else means a sandboxed
        # slot silently behaves unlike its unsandboxed twin — which is how the
        # first sandbox preset lost stream-json progress and delivered a whole
        # one-shot as one final blob.
        for base in ("claude", "codex", "agy", "bash", "opencode"):
            with self.subTest(base=base):
                sb = expand_agents_preset(f"sandbox-{base}", [])
                plain = expand_agents_preset(base, [])
                for flag in (
                    "--agent-oneshot-argv",
                    "--agent-resume-oneshot-argv",
                    "--agent-resume-interactive-argv",
                ):
                    self.assertEqual(
                        sb[sb.index(flag) + 1],
                        plain[plain.index(flag) + 1],
                        f"{flag} drifted from the {base} preset",
                    )
                self.assertEqual(
                    sb[sb.index("--agent-log-format") + 1],
                    plain[plain.index("--agent-log-format") + 1],
                )
                # The adapter is a HOST process that execs the wrapper as its
                # agent argv, so the path must be the base's unchanged — a
                # container path here would name a binary the host cannot run.
                self.assertEqual(
                    sb[sb.index("--agent-stream-adapter") + 1],
                    plain[plain.index("--agent-stream-adapter") + 1],
                )
                # A preset bin may be a path, not only a bare command name.
                # The agent is carried by the BIN, because a fresh interactive
                # launch passes no argv template at all — a selector in the
                # templates is absent exactly there, and `session new --agent
                # sandbox-bash` opened Claude Code when it lived there.
                bin_path = Path(sb[sb.index("--agent-bin") + 1])
                self.assertTrue(bin_path.is_absolute())
                self.assertEqual(bin_path.name, f"{base}-in-podman.sh")
                # Resolved against this module, so a runner started from any
                # checkout gets the wrapper that ships beside the presets it
                # just expanded.
                self.assertTrue(bin_path.exists(), f"{bin_path} does not exist")

    def test_sandbox_is_an_alias_of_sandbox_claude(self) -> None:
        # `sandbox` is what the claude-only kit was called; existing runner
        # slots and docs still name it.
        self.assertEqual(
            expand_agents_preset("sandbox", []),
            expand_agents_preset("sandbox-claude", []),
        )


if __name__ == "__main__":
    unittest.main()
