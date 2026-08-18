# Generic agent sandbox — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `scripts/sandbox/` from a claude-only kit into one podman wrapper that confines any of claude / codex / agy / bash, driven by a single agent table.

**Architecture:** `claude-in-podman.sh` is renamed to `agent-in-podman.sh` and gains a `--sandbox-agent NAME` control flag, consumed from argv by the same filter loop that already eats `--firewall` / `--mount-auth`. Every per-agent difference (host binary, config mounts, auth mode, extra env, egress domains, container HOME) lives in ONE `case` block — the agent table — and nothing else in the script branches on the agent name. The launcher and the image follow the same rename; the firewall allowlists split into a shared dev base plus the table's per-agent domains.

**Tech Stack:** bash, rootless podman, Go (`cmd/sandbox-connect-proxy`), Python 3 stdlib (`scripts/agent_presets.py`).

**Spec:** `docs/superpowers/specs/2026-08-18-generic-agent-sandbox-design.md`

## Global Constraints

- Wrapper stays a **pure pass-through** for agent arguments: it consumes only its own control flags and forwards the rest untouched. Agent policy flags (`--dangerously-skip-permissions`, codex's approval flags) are the caller's business via `--agent-args` / `submit --claude-arg`.
- **No agent is installed into the image.** Host binaries are bridged in; only claude keeps the image's npm copy as a fallback (measured: codex is static-pie musl, agy is glibc-dynamic and runs on the image's glibc 2.36).
- An unknown `--sandbox-agent NAME` is a **hard error**, never a silent fallback to claude.
- Firewall behaviour is unchanged in kind: fail-closed, harness-server carve-out on its one `ip:proto:port`, gateway /32, IPv6 blocked.
- Egress domains for a new agent are **measured** from the CONNECT proxy's refusal log, never transcribed from provider docs.
- codex and agy are **mount-auth only** (no known revocable-token mode); agy mounts the whole `~/.gemini`.
- Existing sandbox runner slots record their `--agent-bin` argv and `scripts/build_and_restart_all.py` replays the RUNNING argv, so the rename requires `systemctl --user restart` (or a fresh `runner.sh up`) of those slots — it is not picked up by a normal restart-all.

---

### Task 1: Agent table, wrapper rename, launcher and image

**Files:**
- Rename: `scripts/sandbox/claude-in-podman.sh` → `scripts/sandbox/agent-in-podman.sh`
- Rename: `scripts/sandbox/claude-launch.sh` → `scripts/sandbox/agent-launch.sh`
- Modify: `scripts/sandbox/Containerfile` (COPY names, image-agent fallback comment)
- Modify: `scripts/sandbox/build.sh` (default image name)
- Test: real `podman` runs through the wrapper, one per agent

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `--sandbox-agent NAME` on the wrapper; env contract to the container — `SANDBOX_AGENT` (name), `SANDBOX_AGENT_BIN` (absolute path the launcher execs), `SANDBOX_SEED_CONFIG` (unchanged), `SANDBOX_AGENT_DOMAINS` (comma-separated, consumed in Task 2). Default image name becomes `harness-agent-sandbox:latest`.

- [ ] **Step 1: Rename both scripts with git mv so history follows**

```bash
cd scripts/sandbox
git mv claude-in-podman.sh agent-in-podman.sh
git mv claude-launch.sh agent-launch.sh
```

- [ ] **Step 2: Add the agent table to the wrapper**

Insert after the control-flag filter loop (the `for a in "$@"` block), before the mount computation. `AGENT` is set by the new flag in Step 3.

```bash
# --- agent table -------------------------------------------------------------
# The ONE place per-agent differences live. Everything below this block is
# agent-independent; if you find yourself adding `case "$AGENT"` anywhere else,
# the field belongs here instead.
#
#   A_HOST_BIN       host path bridged in (readlink -f'd later); "" = image only
#   A_CONTAINER_BIN  where the agent is exposed inside the container
#   A_IMAGE_FALLBACK 1 = the image ships a usable copy if the host bin is absent
#   A_CONFIG_MOUNTS  host paths bind-mounted at IDENTICAL paths in mount auth
#   A_TOKEN_FILE     revocable-token file; "" = this agent has no token auth
#   A_TOKEN_ENV      env var the token is handed to podman as
#   A_SEED_CONFIG    1 = run agent-launch.sh's first-run seeding
#   A_ALWAYS_ENV     NAME=VALUE passed on every run
#   A_FW_ENV         NAME=VALUE passed only when a firewall mode is on
#   A_DOMAINS        comma-separated egress domains this agent needs
#   A_HOME           HOME inside the container when NOT using token auth
#   A_EPHEMERAL_HOME HOME under token auth (the image's own writable home)
declare -a A_CONFIG_MOUNTS=() A_ALWAYS_ENV=() A_FW_ENV=()
case "$AGENT" in
  claude)
    A_HOST_BIN="$HOME_DIR/.local/bin/claude"
    A_CONTAINER_BIN=/usr/local/bin/claude
    A_IMAGE_FALLBACK=1
    A_CONFIG_MOUNTS=( "$HOME_DIR/.claude" "$HOME_DIR/.claude.json" )
    A_TOKEN_FILE="${HARNESS_SANDBOX_CLAUDE_TOKEN_FILE:-$HOME_DIR/.config/harness/sandbox-claude-token}"
    A_TOKEN_ENV=CLAUDE_CODE_OAUTH_TOKEN
    A_SEED_CONFIG=1
    A_ALWAYS_ENV=( DISABLE_AUTOUPDATER=1 )
    # Only under a firewall: disabling telemetry is what keeps a fail-closed
    # allowlist from having to include datadog/statsig (verified A/B).
    A_FW_ENV=( CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 )
    A_DOMAINS="api.anthropic.com,platform.claude.com,console.anthropic.com,statsig.anthropic.com,statsig.com,sentry.io"
    A_HOME="$HOME_DIR"
    A_EPHEMERAL_HOME=/home/node
    ;;
  codex)
    # The host launcher symlinks into ~/.codex, so the config mount already
    # covers the binary; bridge the resolved path anyway so the container path
    # does not depend on the host's package layout.
    A_HOST_BIN="$HOME_DIR/.local/bin/codex"
    A_CONTAINER_BIN=/usr/local/bin/codex
    A_IMAGE_FALLBACK=0
    A_CONFIG_MOUNTS=( "$HOME_DIR/.codex" )
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    A_SEED_CONFIG=0
    A_DOMAINS=""   # measured in Task 2
    A_HOME="$HOME_DIR"
    A_EPHEMERAL_HOME=""   # no token auth, so this is never read
    ;;
  agy)
    A_HOST_BIN="$HOME_DIR/.local/bin/agy"
    A_CONTAINER_BIN=/usr/local/bin/agy
    A_IMAGE_FALLBACK=0
    # The whole ~/.gemini, not just antigravity-cli/: the narrow mount works for
    # one-shots but needs a writable parent in the image and is unverified for
    # interactive sessions. See the spec's "agy's config directory" section.
    A_CONFIG_MOUNTS=( "$HOME_DIR/.gemini" )
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    A_SEED_CONFIG=0
    A_DOMAINS=""   # measured in Task 2
    A_HOME="$HOME_DIR"
    A_EPHEMERAL_HOME=""
    ;;
  bash)
    # A shell sandbox, not a conversational agent: nothing to bridge, nothing
    # to authenticate. HOME is the image's own so a stray write has somewhere
    # to go that is not a missing host path.
    A_HOST_BIN=""
    A_CONTAINER_BIN=/bin/bash
    A_IMAGE_FALLBACK=1
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    A_SEED_CONFIG=0
    A_DOMAINS=""
    A_HOME=/home/node
    A_EPHEMERAL_HOME=""
    ;;
  *)
    echo "[agent-in-podman] unknown --sandbox-agent '$AGENT' (want: claude|codex|agy|bash)" >&2
    exit 2
    ;;
esac
```

- [ ] **Step 3: Add the `--sandbox-agent` / `--image-agent` flags to the filter loop**

Replace the flag loop's `image_claude` handling and add the agent selector. `--image-claude` stays as a deprecated alias because it can appear in a live runner's recorded `--agent-args`.

```bash
AGENT=claude          # default: the only agent this wrapper used to run
image_agent=0
declare -a ARGS=()
want_agent=0
for a in "$@"; do
  if [ "$want_agent" = 1 ]; then AGENT="$a"; want_agent=0; continue; fi
  case "$a" in
    --sandbox-agent)    want_agent=1 ;;
    --sandbox-agent=*)  AGENT="${a#*=}" ;;
    --omit-harness-cli) bridge_cli=0 ;;
    --firewall)         firewall=1 ;;
    --firewall-proxy)   firewall_proxy=1 ;;
    --mount-auth)       force_mount=1 ;;
    --image-agent|--image-claude) image_agent=1 ;;
    *)                  ARGS+=( "$a" ) ;;
  esac
done
[ "$want_agent" = 1 ] && { echo "[agent-in-podman] --sandbox-agent needs a value" >&2; exit 2; }
```

- [ ] **Step 4: Make auth, mounts, bridge and HOME read the table**

Replace the claude-specific auth block, the `HOSTCLAUDE` block and the `-v "$HOME_DIR/.claude..."` lines with table-driven equivalents.

```bash
TOKEN_FILE="$A_TOKEN_FILE"
CLAUDE_HOME="$A_HOME"
declare -a AUTH=()
auth_mode="mount"
if [ -n "$TOKEN_FILE" ] && [ -s "$TOKEN_FILE" ] && [ "$force_mount" != 1 ]; then
  auth_mode="token"
  CLAUDE_HOME="$A_EPHEMERAL_HOME"     # /home/node; set for claude only
  AUTH=(
    --env "$A_TOKEN_ENV=$(cat "$TOKEN_FILE")"
    --env SANDBOX_SEED_CONFIG=1
    --env SANDBOX_SEED_PROJECTS="$(printf '%s\n' "${MOUNT_PATHS[@]}")"
  )
else
  for p in "${A_CONFIG_MOUNTS[@]}"; do
    [ -e "$p" ] && AUTH+=( -v "$p:$p" )
  done
fi

# Host binary bridge (default ON; --image-agent opts out where a fallback
# exists). An agent with no image fallback and no host binary is a hard error:
# falling through would exec a path that does not exist inside the container.
declare -a HOSTBIN=()
agent_src="image"
if [ -n "$A_HOST_BIN" ] && [ "$image_agent" != 1 ]; then
  hbin="$(readlink -f "$A_HOST_BIN" 2>/dev/null || true)"
  if [ -n "$hbin" ] && [ -x "$hbin" ]; then
    HOSTBIN+=( -v "$hbin:$A_CONTAINER_BIN:ro" )
    agent_src="host:$(basename "$hbin")"
  elif [ "$A_IMAGE_FALLBACK" != 1 ]; then
    echo "[agent-in-podman] $AGENT: no host binary at $A_HOST_BIN and no image fallback" >&2
    exit 2
  fi
elif [ -z "$A_HOST_BIN" ]; then
  agent_src="image"
fi
for e in "${A_ALWAYS_ENV[@]}"; do HOSTBIN+=( --env "$e" ); done
```

`A_EPHEMERAL_HOME` is set in the table (Step 2) and is only ever read on the token-auth branch, which only claude has. Add to the `FW` block, in place of the hard-coded `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`:

```bash
  for e in "${A_FW_ENV[@]}"; do FW+=( --env "$e" ); done
  FW+=( --env SANDBOX_AGENT_DOMAINS="$A_DOMAINS" )
```

- [ ] **Step 5: Pass the agent identity into the container and exec the new launcher**

Update the summary line and the final `podman run`:

```bash
echo "[agent-in-podman] agent=$AGENT auth=$auth_mode firewall=$fw_mode harness-cli=$([ "$bridge_cli" = 1 ] && echo on || echo off) bin=$agent_src image=$IMAGE" >&2
```

```bash
  --env SANDBOX_AGENT="$AGENT" \
  --env SANDBOX_AGENT_BIN="$A_CONTAINER_BIN" \
  ...
  "$IMAGE" \
  /usr/local/bin/sandbox-agent-launch.sh "$@"
```

Also change the image default at the top of the file:

```bash
IMAGE="${HARNESS_SANDBOX_IMAGE:-harness-agent-sandbox:latest}"
```

- [ ] **Step 6: Generalize the launcher**

In `agent-launch.sh`, the seeding stays behind `SANDBOX_SEED_CONFIG` (set only for claude) and the exec target comes from the env:

```bash
exec "${SANDBOX_AGENT_BIN:-/usr/local/bin/claude}" "$@"
```

- [ ] **Step 7: Update the Containerfile and build.sh for the new names**

```dockerfile
COPY agent-launch.sh /usr/local/bin/sandbox-agent-launch.sh
```

and in the `chmod +x` list replace `sandbox-claude-launch.sh` with `sandbox-agent-launch.sh`. In `build.sh`:

```bash
IMAGE="${HARNESS_SANDBOX_IMAGE:-harness-agent-sandbox:latest}"
```

- [ ] **Step 8: Rebuild the image**

Run: `scripts/sandbox/build.sh`
Expected: builds `harness-agent-sandbox:latest` with no error.

- [ ] **Step 9: Verify all four agents through the wrapper**

Run each from a repo directory (the wrapper mounts `$PWD`'s repo root):

```bash
cd /home/kforfk/workspace/remote-agent-harness
scripts/sandbox/agent-in-podman.sh --sandbox-agent bash   -c 'id -un; pwd'
scripts/sandbox/agent-in-podman.sh --sandbox-agent codex  --version
scripts/sandbox/agent-in-podman.sh --sandbox-agent agy    --version
scripts/sandbox/agent-in-podman.sh                        --version   # default = claude
scripts/sandbox/agent-in-podman.sh --sandbox-agent nope   --version   # must exit 2
```

Expected: the first four print the agent's own output and the summary line names the right agent; the last prints the unknown-agent error and exits 2.

- [ ] **Step 10: Verify a real prompt round-trips for codex and agy**

```bash
scripts/sandbox/agent-in-podman.sh --sandbox-agent codex exec --skip-git-repo-check 'reply with exactly: WRAPPER_CODEX_OK'
scripts/sandbox/agent-in-podman.sh --sandbox-agent agy --print 'reply with exactly: WRAPPER_AGY_OK'
```

Expected: both echo their sentinel, proving the table's config mounts carry auth through the wrapper (not just through a hand-written `podman run`).

- [ ] **Step 11: Commit**

```bash
git add scripts/sandbox/
git commit -m "feat(sandbox): one wrapper for claude/codex/agy/bash via an agent table"
```

---

### Task 2: Per-agent egress domains in both firewall modes

**Files:**
- Modify: `scripts/sandbox/init-firewall.sh` (the fixed domain loop, lines ~60-79)
- Modify: `cmd/sandbox-connect-proxy/main.go` (`defaultAllow`, `loadAllow`)
- Modify: `scripts/sandbox/agent-in-podman.sh` (already passes `SANDBOX_AGENT_DOMAINS` from Task 1)
- Test: `go test ./cmd/sandbox-connect-proxy/...` plus a measured proxy run per agent

**Interfaces:**
- Consumes: `SANDBOX_AGENT_DOMAINS` (comma-separated) from Task 1.
- Produces: a base allowlist shared by every agent (github / npm / pypi) and the per-agent additions; `SANDBOX_PROXY_ALLOW` keeps its existing meaning (per-task WebFetch targets).

- [ ] **Step 1: Split the ip-allowlist's fixed domain list**

In `init-firewall.sh`, replace the hard-coded `for domain in …` list with a base plus the env:

```bash
# Shared dev hosts every agent needs; the agent's own endpoints arrive in
# SANDBOX_AGENT_DOMAINS (set from the wrapper's agent table), so this list
# never has to know which agent is running.
base_domains="registry.npmjs.org pypi.org files.pythonhosted.org"
agent_domains="$(echo "${SANDBOX_AGENT_DOMAINS:-}" | tr ',' ' ')"
for domain in $base_domains $agent_domains; do
```

(the loop body is unchanged).

- [ ] **Step 2: Split the proxy's allowlist the same way**

In `cmd/sandbox-connect-proxy/main.go`, reduce `defaultAllow` to the shared base and read the agent's domains from the new env:

```go
// Shared dev hosts. Agent-specific endpoints (api.anthropic.com and friends)
// arrive in SANDBOX_AGENT_DOMAINS from the wrapper's agent table, so this
// binary does not have to know which agent it is fronting.
var defaultAllow = []string{
	"github.com",            // + api. / codeload. via suffix
	"githubusercontent.com", // raw. / objects.
	"npmjs.org",             // registry.
	"pypi.org",
	"pythonhosted.org", // files.
}
```

and in the config construction:

```go
allow: loadAllow(os.Getenv("SANDBOX_AGENT_DOMAINS") + "," + os.Getenv("SANDBOX_PROXY_ALLOW")),
```

- [ ] **Step 3: Add a test for the two-source merge**

In `cmd/sandbox-connect-proxy/`'s test file:

```go
func TestLoadAllowMergesAgentAndTaskDomains(t *testing.T) {
	got := loadAllow("api.anthropic.com," + "example.org")
	want := map[string]bool{"api.anthropic.com": true, "example.org": true, "github.com": true}
	for w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("loadAllow missing %q; got %v", w, got)
		}
	}
}
```

- [ ] **Step 4: Run the test**

Run: `go test ./cmd/sandbox-connect-proxy/... -run TestLoadAllow -v`
Expected: PASS.

- [ ] **Step 5: Measure codex's and agy's domains**

Rebuild the image (it ships the proxy binary), then run each agent under the proxy with an EMPTY agent domain list and read the refusals off stderr:

```bash
scripts/sandbox/build.sh
cd /home/kforfk/workspace/remote-agent-harness
scripts/sandbox/agent-in-podman.sh --sandbox-agent codex --firewall-proxy \
  exec --skip-git-repo-check 'reply with exactly: X' 2>&1 | grep -i 'refus\|deny\|not allowed'
scripts/sandbox/agent-in-podman.sh --sandbox-agent agy --firewall-proxy \
  --print 'reply with exactly: X' 2>&1 | grep -i 'refus\|deny\|not allowed'
```

Expected: `[sandbox-proxy]` lines naming the hosts each agent tried to reach. Record them.

- [ ] **Step 6: Put the measured domains in the table and re-run**

Fill `A_DOMAINS` for codex and agy in `agent-in-podman.sh` with what Step 5 printed, then repeat the two commands from Step 5.
Expected: the sentinel comes back and no refusal lines remain.

- [ ] **Step 7: Re-verify claude did not regress**

```bash
scripts/sandbox/agent-in-podman.sh --firewall-proxy -p 'reply with exactly: CLAUDE_PROXY_OK'
scripts/sandbox/agent-in-podman.sh --firewall     -p 'reply with exactly: CLAUDE_IP_OK'
```

Expected: both print the sentinel — the anthropic domains moved from the hard-coded lists into claude's table entry without loss.

- [ ] **Step 8: Commit**

```bash
git add scripts/sandbox/ cmd/sandbox-connect-proxy/
git commit -m "feat(sandbox): per-agent egress domains, measured from the proxy's refusals"
```

---

### Task 3: Presets for the four sandboxed agents

**Files:**
- Modify: `scripts/agent_presets.py:120-127` (the `sandbox` derivation)
- Modify: `scripts/test_agent_presets.py:153-177` (the sandbox preset test)

**Interfaces:**
- Consumes: `--sandbox-agent NAME` from Task 1.
- Produces: preset names `sandbox-claude`, `sandbox-codex`, `sandbox-agy`, `sandbox-bash`, and `sandbox` as an alias of `sandbox-claude`.

- [ ] **Step 1: Write the failing test**

Replace `test_sandbox_preset_matches_claude_except_bin` with:

```python
    def test_sandbox_presets_derive_their_base_agent(self) -> None:
        # Each sandbox-* preset runs its BASE agent through a pass-through
        # wrapper, so its argv must be the base's with only the wrapper's own
        # --sandbox-agent selector prefixed. Anything else means a sandboxed
        # slot silently behaves unlike its unsandboxed twin — which is how the
        # first sandbox preset lost stream-json progress.
        for base in ("claude", "codex", "agy", "bash"):
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
                        f"--sandbox-agent {base} " + plain[plain.index(flag) + 1],
                        f"{flag} drifted from the {base} preset",
                    )
                self.assertEqual(
                    sb[sb.index("--agent-log-format") + 1],
                    plain[plain.index("--agent-log-format") + 1],
                )
                bin_path = Path(sb[sb.index("--agent-bin") + 1])
                self.assertTrue(bin_path.is_absolute())
                self.assertEqual(bin_path.name, "agent-in-podman.sh")
                self.assertTrue(bin_path.exists(), f"{bin_path} does not exist")

    def test_sandbox_is_an_alias_of_sandbox_claude(self) -> None:
        self.assertEqual(
            expand_agents_preset("sandbox", []),
            expand_agents_preset("sandbox-claude", []),
        )
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `python3 scripts/test_agent_presets.py`
Expected: FAIL — `sandbox-claude` is not a known preset.

- [ ] **Step 3: Generate the sandbox presets from the base entries**

Replace the single `KNOWN_AGENT_PRESETS["sandbox"] = {...}` block with:

```python
# The podman sandbox runs the SAME agent binaries through a wrapper that is a
# pure pass-through (scripts/sandbox/README.md), so each sandbox-* preset is
# DERIVED from its base entry rather than copied: only `bin` changes, and the
# wrapper's own agent selector is prefixed onto the argv templates (the wrapper
# filters its control flags out of the stream before the agent sees them).
# Spawned without this derivation, the first sandbox slot fell back to the
# runner's raw defaults and a whole one-shot arrived as one final blob instead
# of streamed events.
#
# `bin` is an absolute path, which presets fully support: runner/agent_profile.go
# ResolveBinPaths LookPath+Abs's every profile bin, and runner/connect.go reports
# agentBinBase(bin) for display.
_SANDBOX_WRAPPER = str(Path(__file__).resolve().parent / "sandbox" / "agent-in-podman.sh")
_ARGV_KEYS = ("oneshotArgv", "resumeOneshotArgv", "resumeInteractiveArgv")

for _base in ("claude", "codex", "agy", "bash"):
    _entry = dict(KNOWN_AGENT_PRESETS[_base])
    _entry["bin"] = _SANDBOX_WRAPPER
    for _k in _ARGV_KEYS:
        _entry[_k] = f"--sandbox-agent {_base} " + _entry[_k]
    KNOWN_AGENT_PRESETS[f"sandbox-{_base}"] = _entry

# Back-compat: `sandbox` is what the claude-only kit was called.
KNOWN_AGENT_PRESETS["sandbox"] = dict(KNOWN_AGENT_PRESETS["sandbox-claude"])
```

- [ ] **Step 4: Run the tests**

Run: `python3 scripts/test_agent_presets.py`
Expected: PASS (all tests).

- [ ] **Step 5: Check the expansion by hand once**

Run: `python3 -c "import sys; sys.path.insert(0,'scripts'); from agent_presets import expand_agents_preset as e; print(e('sandbox-codex',[]))"`
Expected: `--agent-bin` is the absolute `agent-in-podman.sh` path and the argv values start with `--sandbox-agent codex exec --json`.

- [ ] **Step 6: Commit**

```bash
git add scripts/agent_presets.py scripts/test_agent_presets.py
git commit -m "feat(runner): sandbox-{claude,codex,agy,bash} presets derived from their base"
```

---

### Task 4: probe.sh reports which agent it measured

**Files:**
- Modify: `scripts/sandbox/probe.sh` (the config header, the auth measurement, the mount-evidence line)

**Interfaces:**
- Consumes: `SANDBOX_AGENT` from Task 1.
- Produces: a `config_agent` line and an agent-independent auth measurement.

- [ ] **Step 1: Add the config_agent line**

After the `config_firewall` / `config_auth` computation, and for the same reason those exist:

```bash
add "config_agent" "${SANDBOX_AGENT:-claude (unset — pre-generic wrapper?)}"
```

- [ ] **Step 2: Make the auth measurement agent-independent**

Replace the `~/.claude`-only mountinfo regex with one covering every agent's config path, keeping the measurement evidence-based rather than reading the env:

```bash
# MEASURED, not declared: only a bind mount proves a host config directory is
# in here. Covers every agent the wrapper supports — a claude-only regex read
# "token-or-ephemeral" for a codex container that had ~/.codex mounted rw.
cfg_re=' /[^ ]*/(\.claude(\.json)?|\.codex|\.gemini)( |$)'
if grep -qE "$cfg_re" /proc/self/mountinfo 2>/dev/null; then
	auth="mount (host agent config bind-mounted rw; persists past the container)"
else
	auth="token-or-ephemeral (no host agent config mount)"
fi
```

and the evidence line:

```bash
add "agent_config_mount_lines" "$(grep -oE "$cfg_re" /proc/self/mountinfo 2>/dev/null | tr -d ' ' | tr '\n' ',')"
```

- [ ] **Step 3: Update the host-process leak check's pattern**

The wrapper's name changed, so the leak check must look for the new one:

```bash
add "host_proc_leak" "$(cat /proc/[0-9]*/comm 2>/dev/null | grep -Ec 'agent-runner|agent-in-pod') matches (expect 0)"
```

- [ ] **Step 4: Run the probe inside a container, per agent**

```bash
cd /home/kforfk/workspace/remote-agent-harness
scripts/sandbox/agent-in-podman.sh --sandbox-agent bash -c 'bash scripts/sandbox/probe.sh' | head -20
scripts/sandbox/agent-in-podman.sh --sandbox-agent bash --firewall-proxy -c 'bash scripts/sandbox/probe.sh' | head -20
```

Expected: `config_agent: bash` in both, `config_firewall` naming the right mode, `server_other_port` NOT reading `open`.

- [ ] **Step 5: Commit**

```bash
git add scripts/sandbox/probe.sh
git commit -m "feat(sandbox): probe reports the agent it measured; auth check covers all agents"
```

---

### Task 5: Documentation and the slot-migration note

**Files:**
- Modify: `scripts/sandbox/README.md` (title, wrapper name, agent table, auth per agent, image name)
- Modify: `README.md` (the Sandboxing section)

**Interfaces:**
- Consumes: everything above.
- Produces: no code interface.

- [ ] **Step 1: Rewrite scripts/sandbox/README.md's opening and usage**

Retitle to "Agent sandbox kit (rootless podman)", change every `claude-in-podman.sh` to `agent-in-podman.sh`, every `harness-claude-sandbox:latest` to `harness-agent-sandbox:latest`, and replace the "Use it from a runner" snippet with the preset form:

```sh
scripts/sandbox/build.sh
scripts/runner.sh up --as sandboxed --agents sandbox-claude,sandbox-codex,sandbox-agy,sandbox-bash \
  --roots "$HOME/workspace/<repo>"
```

- [ ] **Step 2: Add the supported-agents table**

```markdown
| agent | host binary bridged | config mounted (mount auth) | token auth | image fallback |
|---|---|---|---|---|
| claude | `~/.local/bin/claude` | `~/.claude` + `~/.claude.json` | yes (`setup-token`) | yes (npm copy) |
| codex | `~/.local/bin/codex` | `~/.codex` | **no** | no |
| agy | `~/.local/bin/agy` | `~/.gemini` | **no** | no |
| bash | — (in the image) | — | — | yes |
```

- [ ] **Step 3: State the codex/agy auth exposure plainly**

```markdown
**codex and agy are mount-auth only.** Neither has a known revocable-token mode
of the kind claude's `setup-token` provides, so running them in the sandbox puts
that provider's OAuth credentials inside the container with no narrower option:
`~/.codex/auth.json` for codex, `~/.gemini` for agy — the latter shared with
gemini-cli, so it carries that product's credentials too (tier-ineligible here,
but present). `--firewall-proxy` is the mitigation that matters for them, since
it removes raw-socket egress entirely.

**codex brings its own sandbox.** It warns `could not find bubblewrap on PATH …
will use the bundled bubblewrap` and then works — nested inside podman. The
wrapper does not disable it; approval/sandbox flags are the caller's business
via `--agent-args`, exactly as `--dangerously-skip-permissions` is for claude.
```

- [ ] **Step 4: Add the slot-migration note**

```markdown
### Upgrading an existing sandbox slot

The wrapper was renamed (`claude-in-podman.sh` → `agent-in-podman.sh`) and the
image with it (`harness-claude-sandbox` → `harness-agent-sandbox`). A running
runner slot records its `--agent-bin` argv and `scripts/build_and_restart_all.py`
REPLAYS the running argv, so restart-all will not migrate it — it will re-exec
the old path. Stop the slot and bring it back up with the new presets
(`systemctl --user restart <unit>` for a registered slot, or `runner.sh down`
then `runner.sh up --agents sandbox-…`).
```

- [ ] **Step 5: Update the root README's Sandboxing section**

Change the opening sentence from "runs a runner's spawned `claude`" to "runs a runner's spawned agent (claude / codex / agy / bash)", update the command block to the `--agents sandbox-*` form and the image name, and add one sentence: "codex and agy are mount-auth only — see the kit README."

- [ ] **Step 6: Verify no stale names remain**

Run: `grep -rn "claude-in-podman\|harness-claude-sandbox\|claude-launch" --include='*.md' --include='*.sh' --include='*.py' . | grep -v '^./docs/superpowers/'`
Expected: no output (spec/plan docs keep the historical names on purpose).

- [ ] **Step 7: Commit**

```bash
git add README.md scripts/sandbox/README.md
git commit -m "docs(sandbox): generic agent kit — supported agents, auth exposure, slot migration"
```

---

### Task 6: End-to-end through a real runner slot (the spec's testing items 1 and 2)

Tasks 1-5 verify the wrapper by invoking it directly. That is not the shipping
path: the runner spawns it with a PTY, a worktree cwd and the `HARNESS_*` env,
and the preset's argv template — none of which a hand-run reproduces. This task
is the one that proves the presets and the wrapper compose.

**Files:**
- No source changes expected. Any defect found here is fixed in the task that owns the file, and this task re-run.

**Interfaces:**
- Consumes: the presets from Task 3 and the wrapper from Task 1.
- Produces: nothing; it is the acceptance gate.

- [ ] **Step 1: Bring up a sandbox runner slot**

A distinct `--hostname` keeps `Any` selectors from hitting `AmbiguousRunner`
against the existing slots on this host (same host × same roots is ambiguous).

```bash
scripts/runner.sh up --as sandboxed --hostname sandboxprobe \
  --agents sandbox-claude,sandbox-codex,sandbox-agy,sandbox-bash \
  --roots "$HOME/workspace/remote-agent-harness"
harness-cli ls | grep sandboxprobe
```

Expected: an `Idle` runner row listing all four sandbox-* profiles.

- [ ] **Step 2: One-shot per agent, asserting host-uid ownership of the write**

```bash
REPO="$HOME/workspace/remote-agent-harness"
for a in bash codex agy claude; do
  ID=$(harness-cli submit --repo "$REPO" --host sandboxprobe --agent "sandbox-$a" \
        --task "printf ok > SANDBOX_OWNER_PROBE_$a; id -un")
  echo "$a -> $ID"
done
harness-cli ls | grep -E "SANDBOX_OWNER_PROBE|sandbox-"
```

then, once each has succeeded, from the HOST:

```bash
for a in bash codex agy claude; do
  find "$REPO/.harness-worktrees" -name "SANDBOX_OWNER_PROBE_$a" -exec stat -c '%U %n' {} \;
done
```

Expected: every file exists and is owned by the host user (`kforfk`), not by
root — that is what `--userns=keep-id` buys and the only reason podman was
chosen over docker.

- [ ] **Step 3: Interactive session per agent, asserting INPUT works**

Rendering is not proof that input reaches the agent, so each session gets a real
keystroke and is checked for the answer:

```bash
SID=$(harness-cli session new --repo "$REPO" --host sandboxprobe --agent sandbox-bash -d)
harness-cli session send -enter "$SID" 'echo SANDBOX_INTERACTIVE_OK'
harness-cli session snapshot "$SID" | tail -5
harness-cli session kill "$SID"
```

Repeat with `--agent sandbox-claude`, `--agent sandbox-codex`, `--agent sandbox-agy`,
sending each agent an instruction it will answer (for the conversational ones,
`reply with exactly: SANDBOX_INTERACTIVE_OK`).

Expected: the sentinel appears in each snapshot. A snapshot that renders a
prompt but never the sentinel means input is not reaching the agent — that is a
Task 1 defect (TTY / arg passing), not a flaky test.

- [ ] **Step 4: probe.sh through the slot, both firewall modes**

```bash
harness-cli submit --repo "$REPO" --host sandboxprobe --agent sandbox-bash \
  --task 'bash scripts/sandbox/probe.sh'
```

Expected in the task log: `config_agent: bash`, `in_container: yes`,
`host_proc_leak: 0 matches`, and under the firewall variants
`server_other_port` NOT reading `open`.

- [ ] **Step 5: Tear the slot down and prune the probe tasks**

```bash
scripts/runner.sh down --as sandboxed
harness-cli prune --older-than 0s   # or prune the specific ids submitted above
git status --short scripts/ README.md   # the OWNER_PROBE files live in task worktrees, not here
```

Expected: the slot is gone and no probe tasks are left cluttering the board.

- [ ] **Step 6: Commit anything Steps 1-5 forced**

```bash
git add -A scripts/sandbox/ scripts/agent_presets.py
git commit -m "fix(sandbox): <what the real-runner E2E turned up>"
```

(If nothing was forced, skip this step — an empty commit is noise.)
