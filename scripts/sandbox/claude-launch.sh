#!/bin/bash
# Token-auth launcher. In token auth the home (/home/node) is ephemeral, so
# interactive claude re-runs first-run onboarding (theme wizard) AND the
# trust-this-folder dialog every run. The theme/onboarding state is pre-seeded in
# the image; the trust dialog is per-CWD (the task worktree, which is dynamic), so
# seed it here for $PWD before launching. Gated by SANDBOX_SEED_CONFIG=1 — set
# only in token mode, so mount auth never has its (host-mounted) ~/.claude.json
# rewritten.
set -euo pipefail

if [ "${SANDBOX_SEED_CONFIG:-0}" = "1" ]; then
  python3 - "$HOME" "$PWD" <<'PY' || true
import json, os, sys
home, proj = sys.argv[1], sys.argv[2]
# ~/.claude.json — onboarding + theme + trust-this-folder (for $PWD).
cfg_path = os.path.join(home, ".claude.json")
try:
    cfg = json.load(open(cfg_path))
except Exception:
    cfg = {}
cfg.setdefault("hasCompletedOnboarding", True)
cfg.setdefault("theme", "dark")
# NOTE: this clears the plain trust dialog, but NOT the variant claude 2.1.x
# shows when the folder's .claude/settings.json carries permissions.allow — the
# harness injects one for `Bash(harness-cli *)`, so every interactive sandbox
# session opens on "Quick safety check … this folder pre-approves 1 tool
# permission" and waits. Measured, not guessed: a container inspected before any
# input already had hasTrustDialogAccepted true, and answering the dialog
# changed NOTHING on disk (same container, before/after diff of ~/.claude.json
# and the whole ~/.claude tree). The acceptance is not persisted, so no seedable
# key exists to pre-set — seeding hasCompletedProjectOnboarding was tried and
# did not clear it. Removing the trigger (the injected permissions.allow, which
# --dangerously-skip-permissions makes redundant anyway) is the only fix that
# would work; oneshot is unaffected because it cannot prompt and just logs
# "Ignoring 1 permissions.allow entry".
cfg.setdefault("projects", {}).setdefault(proj, {})["hasTrustDialogAccepted"] = True
json.dump(cfg, open(cfg_path, "w"))
# ~/.claude/settings.json — suppress the one-time "Bypass Permissions mode"
# acceptance prompt (skip-permissions runs unattended in the container).
os.makedirs(os.path.join(home, ".claude"), exist_ok=True)
s_path = os.path.join(home, ".claude", "settings.json")
try:
    st = json.load(open(s_path))
except Exception:
    st = {}
st["skipDangerousModePermissionPrompt"] = True
json.dump(st, open(s_path, "w"))
PY
fi

exec claude "$@"
