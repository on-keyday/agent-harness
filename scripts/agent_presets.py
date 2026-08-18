"""Built-in ``--agents`` preset expansion for ``scripts/runner.py``.

Pure stdlib, no third-party deps — deliberately importable without
``scripts/.venv`` (see ``scripts/test_agent_presets.py``, which exercises
this module directly rather than going through ``runner.py``'s
``bootstrap.ensure_venv()`` / ``psutil`` import chain).

``KNOWN_AGENT_PRESETS`` below is the SINGLE SOURCE OF TRUTH for the
built-in agent presets' bin + argv-template + logFormat shapes.
``.claude/commands/runner-up.md`` ("Codex preset details" / shell-sandbox
presets table) references this table by name and MUST NOT restate the
literal argv strings, so the doc and the code cannot diverge. This module
lets ``runner.py up --agents claude,codex`` expand to concrete
agent-runner flags without the caller hand-typing the JSON.

Every preset here MUST be verified against the real binary before being
added — inventing an argv would silently ship an unverified CLI invocation.
No preset exists for "gemini": the individual Code Assist tier it used is
discontinued and the installed CLI now fails at auth with
IneligibleTierError, so no invocation of it can be verified at all. Its
successor "agy" (Antigravity) is present instead, verified against 1.1.12.
``expand_agents_preset`` raises ``AgentsPresetError`` for any name not in
``KNOWN_AGENT_PRESETS``, telling the caller to pass ``--agent-profiles``
JSON directly instead.
"""

from __future__ import annotations

import json
from pathlib import Path

# name -> {bin, oneshotArgv, resumeOneshotArgv, resumeInteractiveArgv,
# logFormat}. The argv values are the flag-STRING template form
# ({args}/{prompt} tokens, shlex-split by agent-runner itself for the
# default profile — see cmd/agent-runner/main.go parseAgentArgsFlag). For
# extra profiles (carried via --agent-profiles JSON) expand_agents_preset()
# below splits these on whitespace into the argv-array form
# runner.ParseAgentProfilesJSON expects (runner/agent_profile.go); none of
# these templates need shell quoting, so a plain .split() matches what
# shlex.Split would produce. logFormat must be one of the names
# runner/agentlog.NewDecoder recognises ("", "claude-stream-json",
# "codex-jsonl") — see the module docstring above for the source-of-truth
# policy.
KNOWN_AGENT_PRESETS: dict[str, dict[str, str]] = {
    "claude": {
        "bin": "claude",
        # --output-format/--verbose precede {args} so a per-task --claude-args
        # can still override them (claude flags are last-wins). Verified
        # against claude 2.1.220: --output-format stream-json --verbose is
        # accepted ahead of -p.
        "oneshotArgv": "--output-format stream-json --verbose {args} -p {prompt}",
        "resumeOneshotArgv": "--output-format stream-json --verbose {args} --continue -p {prompt}",
        "resumeInteractiveArgv": "{args} --continue",
        "logFormat": "claude-stream-json",
    },
    "codex": {
        "bin": "codex",
        # --json precedes {args} for the same last-wins override reason.
        # Verified against codex-cli 0.145.0: --json is an option of the
        # `codex exec resume` subcommand, so it is valid after `resume`.
        "oneshotArgv": "exec --json {args} {prompt}",
        "resumeOneshotArgv": "exec --json resume --last {args} {prompt}",
        "resumeInteractiveArgv": "resume --last {args}",
        "logFormat": "codex-jsonl",
    },
    "agy": {
        "bin": "agy",
        # Antigravity CLI, the successor to gemini-cli (see module docstring
        # for why no "gemini" preset exists). Flag shape happens to mirror
        # claude's: --print for the one-shot, --continue for "resume the most
        # recent conversation in this cwd" — the same resume model claude's
        # --continue and codex's `resume --last` use, so no session id is
        # threaded here either.
        #
        # No --output-format is requested, unlike claude/codex: agy's
        # stream-json is its own schema ({"event":"init"|"step_update"|
        # "result"}), NOT claude's, and agentlog has no decoder for it.
        # Asking for structured output nothing can decode would only make the
        # task log unreadable, so plain text passes through instead and
        # logFormat stays "".
        #
        # Verified against agy 1.1.12: `--print` one-shot exits 0 with the
        # response on stdout; `--continue --print` carries context across
        # separate invocations; `--continue` starts the TUI under a PTY
        # (without one it exits with a "could not open TTY" error, which is
        # the interactive path working as intended).
        "oneshotArgv": "{args} --print {prompt}",
        "resumeOneshotArgv": "{args} --continue --print {prompt}",
        "resumeInteractiveArgv": "{args} --continue",
        "logFormat": "",
    },
    # Shell-sandbox preset, not a conversational agent — included because
    # it's a trivial copy of the runner-up.md "bash" preset row. --agents
    # only emits the bin/argv triplet; the accompanying --no-worktree and
    # --roots that a real bash runner needs are deliberately NOT injected
    # here (out of scope: --agents is about agent profiles, not
    # shell-sandbox concerns).
    "bash": {
        "bin": "bash",
        "oneshotArgv": "{args} -c {prompt}",
        "resumeOneshotArgv": "{args} -c {prompt}",
        "resumeInteractiveArgv": "{args}",
        "logFormat": "",
    },
}

# The podman sandbox runs the SAME agent binaries through a wrapper that is a
# pure pass-through (scripts/sandbox/README.md), so each sandbox-* preset is
# DERIVED from its base entry rather than copied: ONLY `bin` changes, to that
# agent's <agent>-in-podman.sh symlink. Spawned without this derivation, the
# first sandbox slot fell back to the runner's raw defaults and one-shot
# progress was invisible — a whole task arrived as one final blob instead of
# streamed events.
#
# The agent is carried by the BIN, not by a flag in the argv templates. A fresh
# interactive launch uses NO argv template (runner/agent_command.go
# buildInteractiveArgs returns the extra args unchanged when
# resumeConversation is false), so a template-borne selector vanishes on that
# path: `session new --agent sandbox-bash` opened Claude Code while the task
# row still read agent=sandbox-bash. The bin is the one thing every launch path
# carries.
#
# `bin` is an absolute path, which presets fully support: nothing constrains it
# to a bare command name (runner/agent_profile.go ResolveBinPaths LookPath+Abs's
# every profile bin — a path is in fact the better-behaved form there — and
# runner/connect.go reports agentBinBase(bin) for display).
#
# --agent-args is NOT part of a preset: the sandbox slot's
# --dangerously-skip-permissions is the caller's choice and --agent-args does
# not collide with --agents (see _CONFLICTING_FLAGS).
_SANDBOX_DIR = Path(__file__).resolve().parent / "sandbox"

for _base in ("claude", "codex", "agy", "bash"):
    _entry = dict(KNOWN_AGENT_PRESETS[_base])
    _entry["bin"] = str(_SANDBOX_DIR / f"{_base}-in-podman.sh")
    KNOWN_AGENT_PRESETS[f"sandbox-{_base}"] = _entry

# Back-compat: `sandbox` is what the claude-only kit was called, and existing
# runner slots / docs still name it.
KNOWN_AGENT_PRESETS["sandbox"] = dict(KNOWN_AGENT_PRESETS["sandbox-claude"])

# Flags --agents would itself set. If the caller already passed one of
# these explicitly alongside --agents, expand_agents_preset() refuses
# rather than guessing whether to override or merge (see its docstring for
# the conflict policy rationale).
_CONFLICTING_FLAGS = (
    "--agent-bin",
    "--claude-bin",
    "--agent-oneshot-argv",
    "--agent-resume-oneshot-argv",
    "--agent-resume-interactive-argv",
    "--agent-log-format",
    "--agent-profiles",
)


class AgentsPresetError(ValueError):
    """Raised by expand_agents_preset for an unknown agent name or a flag conflict."""


def _has_flag(args: list[str], name: str) -> bool:
    eq_prefix = f"{name}="
    return any(a == name or a.startswith(eq_prefix) for a in args)


def expand_agents_preset(agents_csv: str, existing_args: list[str]) -> list[str]:
    """Expand ``--agents claude,codex`` into concrete agent-runner flags.

    The FIRST name in *agents_csv* becomes the default profile: emitted as
    ``--agent-bin``, ``--agent-oneshot-argv``, ``--agent-resume-oneshot-argv``,
    ``--agent-resume-interactive-argv`` and ``--agent-log-format``. All five
    are always emitted together for the default profile — agent-runner's
    startup validation requires --agent-resume-oneshot-argv whenever
    --agent-oneshot-argv is customized (cmd/agent-runner/main.go validate()),
    and a Claude-shaped resume default would silently misfire on a
    non-Claude default bin.

    Any REMAINING names are serialized into a single ``--agent-profiles``
    JSON array flag, matching the wire shape
    runner.ParseAgentProfilesJSON expects: objects with
    name/bin/oneshotArgv/resumeOneshotArgv/resumeInteractiveArgv/logFormat,
    argv fields as JSON string arrays (runner/agent_profile.go).

    Conflict policy: if *existing_args* already contains any of
    --agent-bin/--claude-bin/--agent-oneshot-argv/
    --agent-resume-oneshot-argv/--agent-resume-interactive-argv/
    --agent-log-format/--agent-profiles, this raises AgentsPresetError
    instead of silently overriding or merging. --agents is an all-or-nothing
    shortcut for the known presets; use the explicit per-flag form instead
    of mixing it with --agents in the same invocation.

    Raises AgentsPresetError for any name not in KNOWN_AGENT_PRESETS (e.g.
    "gemini" — no authoritative built-in argv exists in this repo for it;
    pass --agent-profiles JSON directly).
    """
    names = [n.strip() for n in agents_csv.split(",") if n.strip()]
    if not names:
        raise AgentsPresetError("--agents requires at least one agent name")

    unknown = [n for n in names if n not in KNOWN_AGENT_PRESETS]
    if unknown:
        raise AgentsPresetError(
            f"no built-in preset for {unknown!r}; use --agent-profiles JSON "
            f"directly (known presets: {sorted(KNOWN_AGENT_PRESETS)})"
        )

    conflicting = [f for f in _CONFLICTING_FLAGS if _has_flag(existing_args, f)]
    if conflicting:
        raise AgentsPresetError(
            f"--agents conflicts with explicit {conflicting}; pass one or "
            "the other, not both, in a single runner.py up invocation"
        )

    default = KNOWN_AGENT_PRESETS[names[0]]
    out: list[str] = [
        "--agent-bin", default["bin"],
        "--agent-oneshot-argv", default["oneshotArgv"],
        "--agent-resume-oneshot-argv", default["resumeOneshotArgv"],
        "--agent-resume-interactive-argv", default["resumeInteractiveArgv"],
        "--agent-log-format", default["logFormat"],
    ]

    extra_names = names[1:]
    if extra_names:
        profiles = []
        for n in extra_names:
            p = KNOWN_AGENT_PRESETS[n]
            profiles.append(
                {
                    "name": n,
                    "bin": p["bin"],
                    "oneshotArgv": p["oneshotArgv"].split(),
                    "resumeOneshotArgv": p["resumeOneshotArgv"].split(),
                    "resumeInteractiveArgv": p["resumeInteractiveArgv"].split(),
                    "logFormat": p["logFormat"],
                }
            )
        out += ["--agent-profiles", json.dumps(profiles)]

    return out
