"""Built-in ``--agents`` preset expansion for ``scripts/runner.py``.

Pure stdlib, no third-party deps — deliberately importable without
``scripts/.venv`` (see ``scripts/test_agent_presets.py``, which exercises
this module directly rather than going through ``runner.py``'s
``bootstrap.ensure_venv()`` / ``psutil`` import chain).

The per-agent argv templates below are copied verbatim from the
authoritative table in ``.claude/commands/runner-up.md`` ("Codex preset
details" / shell-sandbox presets table). That file remains the source of
truth for the argv shapes; this module only encodes them so ``runner.py
up --agents claude,codex`` can expand to concrete agent-runner flags
without the caller hand-typing the JSON.

No preset exists here for "gemini" — there is no authoritative argv for it
anywhere in this repo, and inventing one would silently ship an unverified
CLI invocation. ``expand_agents_preset`` raises ``AgentsPresetError`` for
any name not in ``KNOWN_AGENT_PRESETS``, telling the caller to pass
``--agent-profiles`` JSON directly instead.
"""

from __future__ import annotations

import json

# name -> {bin, oneshotArgv, resumeOneshotArgv, resumeInteractiveArgv}.
# Values are the flag-STRING template form ({args}/{prompt} tokens,
# shlex-split by agent-runner itself for the default profile — see
# cmd/agent-runner/main.go parseAgentArgsFlag). For extra profiles (carried
# via --agent-profiles JSON) expand_agents_preset() below splits these on
# whitespace into the argv-array form runner.ParseAgentProfilesJSON expects
# (runner/agent_profile.go); none of these templates need shell quoting, so
# a plain .split() matches what shlex.Split would produce.
# SINGLE SOURCE OF TRUTH for the built-in agent presets' bin + argv templates
# (claude / codex / bash). `.claude/commands/runner-up.md` references this table
# via `runner.sh up --agents <names>` and MUST NOT restate the literal argv
# strings — keep the values here only, so the doc and the code cannot diverge.
KNOWN_AGENT_PRESETS: dict[str, dict[str, str]] = {
    "claude": {
        "bin": "claude",
        "oneshotArgv": "{args} -p {prompt}",
        "resumeOneshotArgv": "{args} --continue -p {prompt}",
        "resumeInteractiveArgv": "{args} --continue",
    },
    "codex": {
        "bin": "codex",
        "oneshotArgv": "exec {args} {prompt}",
        "resumeOneshotArgv": "exec resume --last {args} {prompt}",
        "resumeInteractiveArgv": "resume --last {args}",
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
    },
}

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
    ``--agent-bin``, ``--agent-oneshot-argv``, ``--agent-resume-oneshot-argv``
    and ``--agent-resume-interactive-argv``. All four are always emitted
    together for the default profile — agent-runner's startup validation
    requires --agent-resume-oneshot-argv whenever --agent-oneshot-argv is
    customized (cmd/agent-runner/main.go validate()), and a Claude-shaped
    resume default would silently misfire on a non-Claude default bin.

    Any REMAINING names are serialized into a single ``--agent-profiles``
    JSON array flag, matching the wire shape
    runner.ParseAgentProfilesJSON expects: objects with
    name/bin/oneshotArgv/resumeOneshotArgv/resumeInteractiveArgv, argv
    fields as JSON string arrays (runner/agent_profile.go).

    Conflict policy: if *existing_args* already contains any of
    --agent-bin/--claude-bin/--agent-oneshot-argv/
    --agent-resume-oneshot-argv/--agent-resume-interactive-argv/
    --agent-profiles, this raises AgentsPresetError instead of silently
    overriding or merging. --agents is an all-or-nothing shortcut for the
    known presets; use the explicit per-flag form instead of mixing it
    with --agents in the same invocation.

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
                }
            )
        out += ["--agent-profiles", json.dumps(profiles)]

    return out
