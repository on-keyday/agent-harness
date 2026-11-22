# Pluggable sandbox toolchains — Design

Date: 2026-11-22

A config file declares named toolchains (mounts, env, a host probe). The
sandbox wrapper reads it, resolves the named toolchain, and maps it into the
container. The consumer asks for a toolchain **by name**:

    --agent-arg --toolchain=go

Go is the first shipped entry, not a special case. The mechanism is generic:
any toolchain whose binaries and caches can be expressed as host mounts plus
env can be added as a config entry, without touching the wrapper.

## 1. Problem

The sandbox has no compiler: `make`, `go`, `gcc` are absent and egress is
closed, so an agent can edit but cannot build or test. That makes it a
suggestion engine, not a coding agent. The fix must not be Go-specific — the
same problem exists for Rust, Java, C, etc. — and it must not weaken the
network containment.

## 2. What was measured (2026-11-22, host `gmkhost`)

| Fact | Measurement |
|---|---|
| Host Go | `/usr/bin/go` is **static** ("not a dynamic executable") → runs on the image's glibc |
| Image glibc | 2.36 (Debian bookworm); host glibc 2.43 → a *dynamic* host binary would not run |
| GOROOT | under GOMODCACHE (`…/pkg/mod/golang.org/toolchain@go1.27.3…`) |
| cgo | repo has **0** `import "C"` → gcc not needed for this repo |
| Bridge + seccomp | `podman run` with GOROOT/GOMODCACHE ro, GOCACHE rw, offline → `go build ./...` **OK** |
| Cold-cache `go test` | **OOM-killed** — host had 15 GB total / ~0 available; the compiler was killed |
| Warm-cache `go test` | `go test ./cli/verb` **OK** |
| `make` | absent from the image; host `make` is dynamic (libguile/gc) → cannot bridge |

Consequences: bridge the toolchain (ro), keep the build cache writable and
**persistent** (cold caches OOM the host), and leave egress closed.

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
    "require": ["GOROOT", "GOMODCACHE"],
    "allow_domains": ["proxy.golang.org", "sum.golang.org"]
  }
}
```

Fields:

| Field | Meaning |
|---|---|
| `description` | one line, shown in refusal messages |
| `probe` | argv run **on the host** (no shell); its stdout is parsed as JSON and its keys become placeholders |
| `mounts[].path` | host path expression; mounted at the **same path** in the container |
| `mounts[].mode` | `ro` or `rw` |
| `mounts[].create` | if true, a missing host dir is created (rw caches only) |
| `env` | name → value expression, passed as `--env NAME=VALUE` |
| `path_prepend` | expressions prepended to the container PATH |
| `require` | placeholders that must be non-empty after resolution |
| `allow_domains` | optional host names this toolchain needs on the egress allowlist (§3.6) |

Placeholders are `{NAME}` where NAME is a key of the probe's JSON, plus
`{HOME}` (the host home). Unknown placeholders are a refusal, never an empty
string.

### 3.2 The resolver helper

`scripts/sandbox/toolchain.py` — same shape as `seccomp_profile.py` and
`history_window.py`: a python helper the bash wrapper calls.

    python3 toolchain.py --config FILE --name go[,NAME...] [--container-path PATH]

It runs each probe, resolves placeholders, validates, and prints **podman argv
tokens, NUL-separated**, to stdout. The wrapper reads them with
`mapfile -d '' -t TC_ARGS < <(python3 …)`. The helper owns argv construction;
the wrapper never parses JSON. Errors go to stderr with a named reason and exit
2.

Refusals:

- unknown toolchain name
- probe command missing or non-zero exit
- probe output not JSON
- a `require` placeholder empty or unresolved
- a mount path empty, relative, or (without `create`) missing
- a duplicate mount with conflicting modes

### 3.3 The wrapper flag

`--toolchain=NAME[,NAME...]`, repeatable and comma-separated like
`--allow-domain`. Absent the flag, the container is **byte-identical** to
today: same argv, same mounts, same env. The flag is the only branch.

When present the wrapper calls the resolver and splices its tokens into the
`podman run` argv next to `"${MOUNTS[@]}"`. It logs `toolchain=go` in the
existing one-line start summary. The flag is orthogonal to auth, firewall and
history: it works under every combination and never touches the allowlist.

### 3.4 PATH

`path_prepend` is emitted as one `--env PATH=<prepend>:<base>` per toolchain.
`<base>` is the image's own PATH, read from `podman image inspect` once (falling
back to the Debian default `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`).
`agent-launch.sh` prepends the agent bin afterwards, so ordering is preserved.

### 3.5 Network defaults to unchanged

Absent `allow_domains`, no domain is added and no firewall mode changes.
`GOTOOLCHAIN=local` and `GOFLAGS=-mod=readonly` are the toolchain's business,
not the wrapper's; a toolchain that needs the network declares it in its own
`env`, and it still cannot reach anything the firewall refuses.

### 3.6 Toolchain egress (`allow_domains`, opt-in per entry)

A toolchain may declare `allow_domains`. They are **additive**, exactly like
`--allow-domain`: the wrapper appends them to the agent's own `A_DOMAINS`
before the firewall is built. The allowlist mechanism is unchanged — the
firewall still resolves A records and default-denies; a toolchain merely adds
another declarer to the same list.

Rules, mirroring `--allow-domain`:

- Each value is validated as a host name; a bad value is a refusal, not a
  silent skip.
- Under `--no-firewall` they are ignored with a warning (nothing to add).
- Under injection the agent's base `A_DOMAINS` is empty; the toolchain's
  domains are then the only agent egress. That is an explicit widening of the
  "injection = no agent egress" property, so it is **logged in the start
  summary** (`allow+=…`) and named in the README.
- The Go entry ships with `proxy.golang.org` and `sum.golang.org` so
  `go mod download` works; an operator who wants offline-only omits them or
  uses `--no-firewall`-style refusal.

Mechanism: the resolver emits the domains as `--env SANDBOX_TOOLCHAIN_DOMAINS=<csv>`
(a separate variable so it cannot clobber `SANDBOX_AGENT_DOMAINS`), and the
firewall scripts merge both sources. This keeps the resolver the single owner
of the run parameters and leaves the allowlist code additive.

## 4. Security

- **No new escape.** The toolchain runs inside the container with the same
  seccomp, caps and namespaces. Giving an agent a compiler lets it build and run
  code *in the sandbox* — a bounded effect the model already allows.
- **The config is trusted input.** A `probe` is an argv run on the host as the
  host user; a hostile config is equivalent to a hostile wrapper. The shipped
  file is versioned; the env override is the operator's own. Document this in
  the README next to the flag.
- **Caches stay off the host's own.** `GOCACHE` is a dedicated directory
  (`~/.cache/harness-sandbox/go-build`), never the host's `~/.cache/go-build`,
  so a poisoned cache cannot reach a host build. `GOMODCACHE` is **ro** so the
  container cannot write the host's modules.
- **Residual: egress widens when a toolchain declares domains.** `allow_domains`
  is the same allowlist mechanism as `--allow-domain`, so it is bounded and
  auditable — but naming a toolchain now also widens egress. It is logged in the
  start summary and ignored under `--no-firewall`, and an entry that declares no
  domains changes nothing. The operator chooses the toolchain, so the widening
  is a deliberate act, not a default.
- **Residual: host memory.** A cold cache under memory pressure OOM-kills the
  compiler (measured). The persistent cache is the mitigation; the host's
  memory is the real limit.

## 5. Gates

| Gate | Expectation |
|---|---|
| V1 | no flag → argv/mounts/env identical to today |
| V2 | `--toolchain=go` → `go build ./...` and `go test ./...` pass offline, under seccomp |
| V3 | GOMODCACHE ro: a write attempt from inside fails |
| V4 | unknown name, failed probe, empty `require`, missing mount → exit 2 with a named reason |
| V5 | `--toolchain=go,rust` resolves both; duplicate mounts dedupe |
| V6 | egress still refused with the flag on |
| V7 | a synthetic toolchain entry (e.g. a fake `foo` with a `cat` probe) resolves without wrapper changes |
| V8 | `allow_domains` from a toolchain appear in the allowlist and the start summary; a bad host name is refused |

V7 is the pluggability gate: it proves the mechanism is generic.

## 6. Decisions

**operator** = chosen by the human in conversation. **this spec** = chosen by
the author while writing. Rows marked "this spec" are the ones worth a second
look.

| # | Decision | Decided by |
|---|---|---|
| D1 | The config ships in-repo at `scripts/sandbox/toolchains.json`, with `HARNESS_SANDBOX_TOOLCHAINS` as the operator's own override. No `--toolchain-config` flag in v1: the env override already covers the operator's file, and a flag is another surface to validate | this spec |
| D2 | v1 ships `go` only. `make` is not baked into the image; `go build`/`go test` directly closes the compile loop. `make` moves to the deferred image-per-toolchain path (§9), where an image that carries it is selected by name | operator, 2026-11-22 |
| D3 | `GOCACHE` is a persistent per-host-user directory (`~/.cache/harness-sandbox/go-build`), not per-task. A cold cache under host memory pressure OOM-killed the compiler in measurement; the warm cache is the mitigation | this spec |
| D4 | v1 ships only `go`. `rust`/`c` entries are examples the mechanism admits, not shipped entries: each needs its own measurement of what bridges and what does not | this spec |
| D5 | Toolchain `allow_domains` apply under injection too. The widening is explicit and logged; refusing them under injection would make the same toolchain work in one mode and not another for no stated reason | operator, 2026-11-22 |
| D6 | Domains merge in the firewall scripts via a new `SANDBOX_TOOLCHAIN_DOMAINS` variable that the resolver emits as an `--env` token. The allowlist code stays additive and the wrapper stays a dumb consumer of argv | this spec |

## 7. Left to the builder

- The resolver's output protocol details (NUL-delimited argv tokens; the
  `--domains` accessor if the wrapper needs the list separately).
- The exact JSON field names in `toolchains.json`.
- The `GOCACHE` directory name under `~/.cache/harness-sandbox/`.
- The README wording for the flag and the domain widening.

## 8. Non-goals

- Toolchains that need the network to install (the point is offline).
- Bridging `make`/`gcc` for this repo (0 cgo; not needed).
- A toolchain store/registry; the config is a file, not a service.
- Replacing the host's land/build path; landing stays a host action.

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
