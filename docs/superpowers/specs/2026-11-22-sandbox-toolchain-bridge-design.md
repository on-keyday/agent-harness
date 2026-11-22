# Pluggable sandbox toolchains — Design

Date: 2026-11-22. Line references are to `main` at `c9263b9c`.

A config file declares named toolchains (mounts, env, a host probe). The
sandbox wrapper reads it, resolves the named toolchain, and maps it into the
container. The consumer asks for a toolchain **by name**:

    --agent-arg --toolchain=go

Go is the first shipped entry, not a special case. The mechanism is generic:
any toolchain whose binaries and caches can be expressed as host mounts plus
env can be added as a config entry, without touching the wrapper.

## 1. Problem

The sandbox has no compiler: `make`, `go`, `gcc` are absent, so an agent can
edit but cannot build or test. That makes it a suggestion engine, not a coding
agent. The fix must not be Go-specific — the same problem exists for Rust,
Java, C, etc. — and it must not weaken the network containment.

## 2. What was measured (2026-11-22, host `gmkhost`)

| Fact | Measurement |
|---|---|
| Host Go | `/usr/bin/go` is **static** ("not a dynamic executable") → runs on the image's glibc |
| Image glibc | 2.36 (Debian bookworm); host glibc 2.43 → a *dynamic* host binary would not run |
| GOROOT | under GOMODCACHE (`…/pkg/mod/golang.org/toolchain@go1.27.3…`) |
| cgo | repo has **0** `import "C"` → gcc not needed for this repo |
| Bridge + seccomp | `podman run` with GOROOT/GOMODCACHE ro, GOCACHE rw, offline → `go build ./...` **OK** |
| Cold-cache `go test` | **OOM-killed** — host had 15 GB total / ~0 available; the compiler was killed |
| Warm-cache `go test` | `go test ./cli/verb` **OK** (one package) |
| `make` | absent from the image; host `make` is dynamic (libguile/gc) → cannot bridge |
| `make` is not redundant | `make test` = `js-test` + `scripts-test` + `go test ./...`; `make scripts-test` runs the python/bash suites. In-sandbox `go test ./...` replaces the first part; the script suites need `make` |

Not measured: `go test ./...` in the sandbox. The integration/socket/subprocess
tests it includes may fail under seccomp or memory pressure, so §5 V2 asserts
only what was measured.

Consequences: bridge the toolchain (ro), keep the build cache writable and
**persistent** (cold caches OOM the host), and leave the egress allowlist as it
is. The sandbox is not offline: the default proxy allowlist already carries
github/npm/pypi (`scripts/sandbox/README.md:459-470`). The property to preserve
is **default-deny plus an allowlist**, not "no egress".

## 3. Design

### 3.1 The config file

A JSON file, `scripts/sandbox/toolchains.json`, read by a python helper. An
operator may override it with `HARNESS_SANDBOX_TOOLCHAINS=/path/to/file.json`
(their own environment), matching `HARNESS_SANDBOX_MEMORY_ROOTS`. The config is
**trusted input** — same trust as the wrapper itself (§4).

Schema: a top-level object keyed by toolchain name.

```json
{
  "go": {
    "description": "Go toolchain bridged from the host",
    "probe": { "command": "go", "args": ["env", "-json", "GOROOT", "GOMODCACHE"] },
    "mounts": [
      { "path": "{GOROOT}", "mode": "ro" },
      { "path": "{GOMODCACHE}", "mode": "ro" },
      { "path": "{HOME}/.cache/harness-sandbox/go-build", "mode": "rw", "create": true }
    ],
    "env": {
      "GOROOT": "{GOROOT}",
      "GOMODCACHE": "{GOMODCACHE}",
      "GOCACHE": "{HOME}/.cache/harness-sandbox/go-build",
      "GOTOOLCHAIN": "local",
      "GOFLAGS": "-mod=readonly"
    },
    "path_prepend": ["{GOROOT}/bin"],
    "require": ["GOROOT", "GOMODCACHE"]
  }
}
```

Fields:

| Field | Meaning |
|---|---|
| `description` | one line, shown in refusal messages |
| `probe` | argv run **on the host** (no shell); its stdout is parsed as JSON and its keys become placeholders. Optional when the entry uses no placeholders |
| `mounts[].path` | host path expression; mounted at the **same path** in the container |
| `mounts[].mode` | `ro` or `rw` |
| `mounts[].create` | if true, a missing host dir is created as the host user **before** the run (rw only) |
| `env` | name → value expression, passed as `--env NAME=VALUE` |
| `path_prepend` | expressions prepended to the container PATH |
| `require` | placeholders that must be non-empty after resolution |
| `allow_domains` | optional host names this toolchain needs on the egress allowlist (§3.6) |

Placeholders are `{NAME}` where NAME is a key of the probe's JSON, plus
`{HOME}` (the host home). Unknown placeholders are a refusal, never an empty
string.

`create: true` creates the full host path as the host user **before** `podman
run`, so the mount source exists and is host-user-owned. That is what it fixes:
podman creating a missing source directory itself (root-owned, unusable by the
agent). It does **not** fix the separate container-side issue — podman still
synthesises a missing mountpoint parent in the container's overlay as container
root (`agent-in-podman.sh:754-758`, measured for opencode 2026-08-19), so a tool
that `mkdir`s a sibling of the mountpoint under `{HOME}/.cache` still gets
`EACCES`. That residual stays (§4); `create: true` with `mode: "ro"` is a
refusal.

`HARNESS_SANDBOX_TOOLCHAINS` is a host path with no meaning inside the
container: the config is read on the host by the wrapper, and the container
never sees the file. It is excluded from the `HARNESS_*` env forwarding beside
`HARNESS_MEMORY_DIR` (`agent-in-podman.sh:997-1000`).

The forwarding block's rule is "a host path is not a container mount", but only
`HARNESS_MEMORY_DIR` is excluded today. `HARNESS_SANDBOX_MEMORY_ROOTS` is the
same class of value and **is** forwarded, so a slot that sets it hands the
container a host path that does not exist there. This design adds the exclusion
for `HARNESS_SANDBOX_TOOLCHAINS` and corrects the memory-roots case in the same
change, so the rule holds for every host-path variable.

### 3.2 The resolver helper

`scripts/sandbox/toolchain.py` — same shape as `seccomp_profile.py` and
`history_window.py`: a python helper the bash wrapper calls.

    python3 toolchain.py --config FILE --name go[,NAME...] [--domains] [--container-path PATH]

It runs each probe, resolves placeholders, validates, and prints **podman argv
tokens, NUL-separated**, to stdout. `--domains` prints the merged, validated
`allow_domains` as a comma-separated line and runs **no probes** (domains need
no placeholder values); it exists so the wrapper can log and merge the list
without parsing argv. The helper owns argv construction; the wrapper never
parses JSON. Errors go to stderr with a named reason and exit 2.

The wrapper must not read stdout through process substitution: in bash,
`mapfile -d '' -t A < <(cmd)` exits 0 even when `cmd` exits 2 (measured, bash
5.3, under `set -euo pipefail`), so a refusal would be swallowed and the
container would start without the toolchain. Capture to a temp file and check
the status:

    tmp=$(mktemp) || exit 2
    python3 toolchain.py … >"$tmp" || { rm -f "$tmp"; exit 2; }
    mapfile -d '' -t TC_ARGS <"$tmp"
    rm -f "$tmp"

Refusals:

- unknown toolchain name; empty name (`--toolchain=`, `--toolchain=go,,rust`)
- probe command missing or non-zero exit; probe output not JSON
- a `require` placeholder empty or unresolved; unknown placeholder
- a mount path empty, relative, or (without `create`) missing
- a mount path containing `:` or a newline (breaks `podman -v src:dst:mode`;
  same rule as `--memory-seed`, `agent-in-podman.sh:199`)
- `create: true` with `mode: "ro"`
- mount overlap: same `realpath` and same mode → dedupe to one mount; same
  `realpath` and different mode → refuse; parent/child overlap with the same
  mode → allow and emit both (`go` needs this: `GOROOT` sits inside
  `GOMODCACHE`, both `ro`); parent/child overlap with different modes → refuse
- `env` key defined by two named toolchains with different values
- `allow_domains` value that is not a host name
- missing, unreadable, or malformed config file (including the
  `HARNESS_SANDBOX_TOOLCHAINS` override)

Paths are `realpath`'d before comparison, but the mount path is **not** rewritten
after resolution: `GOROOT` sits inside `GOMODCACHE` and `env.GOROOT` must keep
matching the mounted path. Dedupe therefore only collapses identical
source+mode pairs; it never rewrites a path.

### 3.3 The wrapper flag

`--toolchain=NAME[,NAME...]`, repeatable and comma-separated like
`--allow-domain`. Absent the flag, the container is **byte-identical** to
today: same argv, same mounts, same env. The flag is the only branch.

When present the wrapper calls the resolver and splices its tokens into the
`podman run` argv next to `"${MOUNTS[@]}"`. It logs `toolchain=go` in the
existing one-line start summary. The flag is orthogonal to auth, firewall and
history: it works under every combination. On its own it changes no domain; only
an entry that declares `allow_domains` widens the allowlist (§3.6).

### 3.4 PATH

`path_prepend` entries from **all** named toolchains are merged, in CLI order and
deduped, into a **single** `--env PATH=<tc1>:<tc2>:<base>` token. Emitting one
`--env PATH=` per toolchain would let the second overwrite the first and drop
`go`'s `GOROOT/bin`. `<base>` is the image's own PATH, read from
`podman image inspect` once and passed to the resolver as `--container-path`;
the resolver merges it with the toolchains' `path_prepend` lists and emits the
single token. If the inspect call fails (unpulled image, no `Config.Env`), the
wrapper falls back to the Debian default
`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` and continues
rather than aborting under `set -e`. `agent-launch.sh` prepends the agent bin
afterwards, so ordering is preserved.

### 3.5 Network defaults to unchanged

Absent `allow_domains`, no domain is added and no firewall mode changes: the
default proxy allowlist and the agent's own domains are exactly as today.
`GOTOOLCHAIN=local` and `GOFLAGS=-mod=readonly` are the toolchain's business,
not the wrapper's; a toolchain that needs the network declares it in its own
`env` and `allow_domains`, and it still cannot reach anything the allowlist
refuses.

### 3.6 Toolchain egress (`allow_domains`, opt-in per entry)

A toolchain may declare `allow_domains`. They are **additive**, exactly like
`--allow-domain`: the wrapper appends them to `allow_domains` **before** the
host-name loop that already exists (`agent-in-podman.sh:245-252`), so they flow
through the same path as a flag-given domain — the host-name check, the
`--no-firewall` clear, `allow_extra`, the start summary and the `A_DOMAINS`
merge. The allowlist mechanism is unchanged — the firewall and the proxy still
default-deny; a toolchain merely adds another declarer to the same list.

Rules, mirroring `--allow-domain`:

- Each value is validated as a host name; a bad value is a refusal, not a
  silent skip. The resolver validates in **both** modes, so a run without
  `--domains` still refuses a bad name.
- Under `--no-firewall` they are ignored with the existing warning (nothing to
  add).
- Under injection the agent's base `A_DOMAINS` is empty; the toolchain's
  domains are then added to the shared defaults. That is an explicit widening of
  the "injection = no agent egress" property, so it is **logged in the start
  summary** (`allow+=…`) and named in the README.
- The shipped `go` entry declares **no** domains. `GOMODCACHE` is `ro` and
  `GOFLAGS=-mod=readonly`, so `go mod download` fails with `EROFS` whatever the
  allowlist says; widening egress for an operation that cannot succeed is a
  change with no upside. The field stays in the schema so an operator can turn
  dependency installation on later, as a config change: mount a **dedicated**
  writable `GOMODCACHE` (`{HOME}/.cache/harness-sandbox/go-mod`, `rw`,
  `create: true`), never the host's own `{GOMODCACHE}`, add the domains, and
  drop `-mod=readonly` in that entry. Making the host's real module cache `rw`
  would break §4's "caches stay off the host's own": a container could
  trojanise a module the host's own `go build` later compiles.

Mechanism: the resolver's `--domains` mode prints the validated list and runs
no probes, so the wrapper can call it early and append the result to
`allow_domains` before the host-name loop:

    tc_domains=$(python3 toolchain.py --config … --name … --domains) || exit 2

Everything downstream then applies without a second seam: the `--no-firewall`
clear (`:249-252`), `allow_extra` (`:1014-1018`), the start summary's `allow+=…`
(`:1149`) and `FW+=( --env SANDBOX_AGENT_DOMAINS=… )` (`:1058`). No new env var,
no change to `init-firewall.sh`, `init-firewall-proxy.sh`,
`sandbox-connect-proxy`, or the image: both firewall modes read the same
`A_DOMAINS`, so one merge covers both.

The later argv call (NUL-separated, temp file) is separate and still runs
probes; the `--domains` call reads JSON and validates only. Reading its output
with `$(...)` is safe because it prints one line with no NULs, so exit 2
propagates under `set -euo pipefail`.

## 4. Security

- **No new escape.** The toolchain runs inside the container with the same
  seccomp, caps and namespaces. Giving an agent a compiler lets it build and run
  code *in the sandbox* — a bounded effect the model already allows.
- **The config is trusted input.** A `probe` is an argv run on the host as the
  host user; a hostile config is equivalent to a hostile wrapper. The shipped
  file is versioned; the env override is the operator's own. Document this in
  the README next to the flag.
- **`--toolchain` is not operator-only.** It arrives as an `--agent-arg`, so a
  spawning parent agent can pass it, the same surface as `--allow-domain`. It
  cannot introduce a mount or domain the config does not already name, and the
  config is trusted, so the reachable effect is "run this host probe as the host
  user" — which `--agent-arg` already permits.
- **Caches stay off the host's own.** `GOCACHE` is a dedicated directory
  (`~/.cache/harness-sandbox/go-build`), never the host's `~/.cache/go-build`,
  so a poisoned cache cannot reach a host build. `GOMODCACHE` is **ro** so the
  container cannot write the host's modules.
- **Residual: cross-task `GOCACHE` poisoning.** D3 shares one writable
  `GOCACHE` across every sandboxed task of the host user. Go's build cache
  reuses compiled `.a` archives and test binaries keyed by action ID without
  re-verifying artifact contents, so a compromised task can plant an artifact
  that runs in another task's build, use the cache as a covert channel between
  tasks of different trust, or exhaust host disk (it persists past `--rm`).
  Accepted for v1 because the tasks are the same owner's and the threat model is
  accidental blast radius; per-task or per-slot scoping is the fix if that
  changes.
- **Residual: `GOMODCACHE` read exposure.** Mounting the host's `GOMODCACHE` ro
  shows the container every cached host module, including private repositories'
  modules. Read-only, but it is a confidentiality surface the sandbox did not
  have.
- **Residual: container-side mountpoint parents.** Podman synthesises a missing
  mountpoint parent in the container's overlay as container root, so a tool that
  `mkdir`s a sibling of a mountpoint under `{HOME}/.cache` gets `EACCES`
  (`agent-in-podman.sh:754-758`, measured for opencode 2026-08-19). Host-side
  `create: true` fixes only the host source directory; this stays.
- **Residual: host memory.** A cold cache under memory pressure OOM-kills the
  compiler (measured). The persistent cache is the mitigation; the host's
  memory is the real limit.
- **Allowlist semantics differ by mode.** In proxy mode `permitted()`
  (`cmd/sandbox-connect-proxy/main.go:108`) does label-boundary suffix matching;
  in `--firewall` mode the ipset allows every host sharing a resolved A record,
  so a CDN IP admits its neighbours. A toolchain domain is no different from
  `--allow-domain` here, but the difference should be stated once.

## 5. Gates

| Gate | Expectation |
|---|---|
| V1 | no flag → argv/mounts/env identical to today |
| V2 | `--toolchain=go` → `go build ./...` and `go test ./cli/verb` pass without adding egress domains, under seccomp (the measured set; `go test ./...` is not asserted until measured) |
| V3 | GOMODCACHE ro: a write attempt from inside fails |
| V4 | unknown name, failed probe, empty `require`, missing mount, `create`+`ro`, malformed config → **`agent-in-podman.sh` exits 2 and no container starts** (not just the helper called directly) |
| V5 | `--toolchain=go,rust` resolves both; same-path same-mode dedupes, same-path different-mode refuses, parent/child same-mode is allowed; both `path_prepend` entries appear in the container's single `PATH` |
| V6 | the flag alone adds no egress; an entry with `allow_domains` adds exactly those and no others |
| V7 | a synthetic toolchain entry (e.g. a fake `foo` with a `cat` probe) resolves without wrapper changes |
| V8 | `allow_domains` from a synthetic entry appear in the allowlist and the start summary; a bad host name is refused; under `--inject-auth` the domains are added and logged (D5); under `--no-firewall` they are ignored with the warning |
| V9 | with `HARNESS_SANDBOX_TOOLCHAINS` and `HARNESS_SANDBOX_MEMORY_ROOTS` set in the runner env, neither appears in the container's environment; every other `HARNESS_*` still does |

V7 is the pluggability gate: it proves the mechanism is generic.

## 6. Decisions

**operator** = chosen by the human in conversation. **this spec** = chosen by
the author while writing. Rows marked "this spec" are the ones worth a second
look.

| # | Decision | Decided by |
|---|---|---|
| D1 | The config ships in-repo at `scripts/sandbox/toolchains.json`, with `HARNESS_SANDBOX_TOOLCHAINS` as the operator's own override. No `--toolchain-config` flag in v1: the env override already covers the operator's file, and a flag is another surface to validate | this spec |
| D2 | v1 ships `go` only. `make` is not baked into the image; `go build`/`go test` directly closes the compile loop. `make` is wanted (`make test`, `make scripts-test`) but its host binary cannot run in the image, so it moves to the deferred image-per-toolchain path (§9), where an image that carries it is selected by name | operator, 2026-11-22 |
| D3 | `GOCACHE` is a persistent per-host-user directory (`~/.cache/harness-sandbox/go-build`), not per-task. A cold cache under host memory pressure OOM-killed the compiler in measurement; the warm cache is the mitigation. The trade-off is cross-task cache poisoning (§4); per-task scoping is the alternative if the threat model widens | this spec |
| D4 | v1 ships only `go`. `rust`/`c` entries are examples the mechanism admits, not shipped entries: each needs its own measurement of what bridges and what does not | this spec |
| D5 | Toolchain `allow_domains` apply under injection too. The widening is explicit and logged; refusing them under injection would make the same toolchain work in one mode and not another for no stated reason | operator, 2026-11-22 |
| D6 | The resolver emits domains in a separate `--domains` mode and the wrapper appends them to `allow_domains` before the host-name loop, so the existing validation, `--no-firewall` clear, `allow_extra` summary and `A_DOMAINS` merge all apply. No new env var and no image change: both firewall modes already read `A_DOMAINS` | this spec |
| D7 | The shipped `go` entry declares no `allow_domains`. Dependency installation inside the sandbox is deferred; when wanted it is a config change (a dedicated writable `GOMODCACHE` plus domains), never the host's own cache | operator, 2026-11-22 |
| D8 | Host-path variables are not forwarded into the container. `HARNESS_SANDBOX_TOOLCHAINS` is excluded, and `HARNESS_SANDBOX_MEMORY_ROOTS` is excluded in the same change because it is the same class of value and leaks today; nothing inside the container reads either (`git grep` shows only the host-side wrapper and its tests) | this spec |

## 7. Left to the builder

- The `GOCACHE` directory name under `~/.cache/harness-sandbox/`.
- The README wording for the flag and the domain widening.

## 8. Non-goals

- A toolchain store/registry; the config is a file, not a service.
- Bridging `gcc`: not needed for this repo (0 cgo).
- Bridging `make` in v1: the host binary is dynamic and cannot run in the image
  (§2), so `make test` and `make scripts-test` are unavailable in-sandbox. `go
  test ./...` — what `make test` runs — is available directly; the python/bash
  tests behind `make scripts-test` are not. `make` is deferred to §9, not dropped
  as unnecessary.
- Replacing the host's land/build path; landing stays a host action.
- Installing dependencies inside the sandbox in v1 (D7).

## 9. Deferred: an image per toolchain

An entry may later name the image it needs (for example
`"image": "harness-agent-sandbox:with-make"`), so that choosing a toolchain
also chooses the image. Not in v1; recorded because the constraints are already
visible and should not be rediscovered.

- **One image per container.** Two named toolchains that name different images
  cannot both be satisfied; the resolver must refuse rather than pick one.
- **Precedence against the slot.** `HARNESS_SANDBOX_IMAGE` is read before flags
  (`agent-in-podman.sh:30`). If a toolchain names an image, the explicit slot
  value must win or the toolchain's must — the spec has to pick one and say so;
  silently preferring either is the failure mode.
- **The image carries more than the toolchain.** It also carries the agent
  binaries' fallback and the firewall kit, so an image chosen for a compiler is
  a decision about the whole container, not a mount-level convenience.
- **The current switch is slot-level.** `HARNESS_SANDBOX_IMAGE` selects the
  image per runner slot; there is no `--image` flag and no per-task switch. A
  toolchain-driven image would be the first per-task image selection and needs
  its own gate.

## 10. Anti-goals

Outcomes that count as failure even if the code works.

- **A toolchain that only works in one firewall mode.** The mount/env bridge and
  the domain merge must behave identically under `--firewall-proxy` and
  `--firewall`; a mode-specific second mechanism is the failure being avoided.
- **A silent fallback.** A resolver refusal must stop the run, not start a
  container without the toolchain. An unknown name must not degrade to "no
  toolchain".
- **A widened allowlist by default.** Naming a toolchain must not add a domain
  unless that entry declares one, and the widening must be visible in the start
  summary.
- **A host probe that runs without being asked.** The probe runs only when the
  flag names the toolchain; the absent flag must not execute anything.
- **`--toolchain` becoming a mount or domain injection primitive.** It selects
  among entries the trusted config already defines; it cannot name a path or a
  host the config does not.

## 11. Approaches considered

- **JSON config + python resolver (chosen).** Reuses the house pattern
  (`seccomp_profile.py`, `history_window.py`), keeps the wrapper a consumer of
  argv, and makes a new toolchain a data change. Cost: a new helper and a
  schema to validate.
- **Shell-only resolver in the wrapper.** No new file, but JSON parsing in bash
  and per-toolchain branching in the wrapper — the same reason the two existing
  helpers are python.
- **Bake toolchains into the image.** No mounts, no host coupling, but a rebuild
  per toolchain and a much larger image; also hides what the toolchain touches.
  Kept as the deferred path for `make` (§9), where a dynamic host binary cannot
  bridge.
- **A separate broker binary for build/test verbs.** Stronger mediation, but a
  second process, a protocol and a lifecycle to design; the mount bridge closes
  the compile loop without them.
