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
