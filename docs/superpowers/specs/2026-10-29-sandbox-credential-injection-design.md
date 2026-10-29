# Sandbox credential injection — the agent's API key stays with the proxy — Design

Date: 2026-10-29

A new auth mode for `scripts/sandbox/agent-in-podman.sh`, the default for pi.
It builds on the per-run config root of
`2026-10-26-sandbox-history-window-design.md` and on the `--firewall-proxy`
CONNECT proxy (`cmd/sandbox-connect-proxy`).

---

## 1. Problem

Under mount auth the container sees the agent's credential file. For pi that
is `~/.pi/agent/auth.json`, a static provider key (openrouter on this host).
The default proxy mode allowlists `github.com`, so a key read from that file
can leave the container, for use after it is gone. A red-team review of the
sandbox (2026-10-29, relayed to this task) found it readable from inside a live
`sandbox-pi` task.

The same mount is read-write, and pi loads every extension under
`~/.pi/agent/extensions/` (pi 1.4.0 `dist/core/extensions/loader.js:684`): a
file the container leaves there runs in the next host `pi`, with the host's
permissions.

## 2. Non-goals

- Stopping the agent from **using** the credential while it runs. It can still
  send any request the provider accepts.
- codex, agy, opencode. codex and agy hold OAuth credentials whose refresh the
  proxy would have to take over; opencode has no credential on this host.
- claude. Measured to work (§8), not wired. **Replaced by Amendment 1**, which
  wires it.

## 3. The mechanism

```
agent (uid 1000)                          proxy process (uid 1001)
 baseUrl http://127.0.0.1:18081/api/v1     inject listener 127.0.0.1:18081
 apiKey  sandbox-placeholder  ── HTTP ──▶   Authorization: Bearer <key>
                                            ── TLS ──▶ https://openrouter.ai
```

- `inject_auth.py pi` reads the host's `settings.json` (`defaultProvider`) and
  `auth.json` (that provider's `api_key` entry), and writes a `models.json`
  that gives the provider `baseUrl = http://127.0.0.1:18081<path of its base
  URL>` and `apiKey = sandbox-placeholder`. It prints the upstream origin and
  the key on stdout, which the wrapper reads into a variable.
- pi takes a credential from `auth.json` first and from the provider's
  `models.json` `apiKey` only when `auth.json` has none (pi-ai
  `dist/auth/resolve.js`), so the container gets no `auth.json`. A `baseUrl`
  on a built-in provider applies to its built-in models
  (`docs/models-configuration.md`).
- The wrapper passes `SANDBOX_INJECT_KEY` to podman **by name** (`--env
  SANDBOX_INJECT_KEY`), so the value is on no argv. `SANDBOX_INJECT_UPSTREAM`
  and `SANDBOX_INJECT_PORT` go by value.
- `entrypoint.sh` starts the proxy, which inherits its environment, waits for
  the inject port (absent = fatal), `unset`s the key, and only then applies
  the firewall and `exec`s the agent through `gosu`. It sets `no_proxy` and
  `NO_PROXY` to `127.0.0.1`: pi's undici `EnvHttpProxyAgent` runs with
  `proxyTunnel: true` (`dist/core/http-dispatcher.js:73-85`), so without it
  even `http://127.0.0.1:18081` is sent to the CONNECT proxy, which refuses
  loopback (403). undici reads `no_proxy` before `NO_PROXY`
  (`undici/lib/dispatcher/env-http-proxy-agent.js:171`), and the entrypoint
  already exported an empty `no_proxy`, so both are set.
- The proxy's inject listener (`inject.go`) is an `httputil.ReverseProxy` to
  one https origin. It overwrites `Authorization`, adds no `X-Forwarded-*`,
  and logs `INJECT <method> <host> <path> -> <status>`.

## 4. Decisions taken

`Decided-by` is provenance; `author` rows were proposed by this task.

| # | Decision | Decided-by |
|---|---|---|
| 1 | Keep the credential out of the container by having a proxy add it, rather than only narrowing the mount | red-team review (relayed), accepted by the operator |
| 2 | On by default for pi; `--mount-auth` is the way back | operator, 2026-10-29 |
| 3 | Implement pi now; measure claude in the same change, do not wire it. **Replaced by Amendment 1** (claude wired, operator, same day) | operator, 2026-10-29 |
| 4 | Plain HTTP on loopback + the agent's base URL, not a TLS MITM of the CONNECT tunnel | author |
| 5 | The injector lives in the CONNECT proxy binary, under its uid: no new process, no new image file | author |
| 6 | The key reaches the proxy through the environment, dropped by the entrypoint before the agent; not `podman secret` (§7) | author |
| 7 | Injection reuses the per-run config root, binding the **whole** sessions store unless `--history-window` narrows it, so the operator's cross-cwd resume keeps working | author |
| 8 | Only the proxy mode can inject: `--firewall` / `--no-firewall` with injection exit 2 and name `--mount-auth`; no fallback | author |
| 9 | Only an `api_key` credential; a base URL from the host's `models.json` or a built-in table holding the one measured provider (openrouter) | author |
| 10 | `Authorization: Bearer` is the only injected header form. Both measured cases (openrouter, claude's OAuth token) use it | author |

## 5. Interface

- `--inject-auth`: pi (and claude since Amendment 1; `A_INJECT` in the agent
  table). Exits 2 for any other agent, together with `--mount-auth` or `--token-auth`, and outside the
  proxy mode.
- Default: an agent whose table entry has `A_INJECT` injects unless
  `--mount-auth` or `--token-auth` is given.
- The runner log line reads `auth=inject`. `probe.sh` adds
  `config_credential` and reads `config_auth` as `per-run-root`.
- The image must be rebuilt (`build.sh`).

## 6. Refusals (exit 2, nothing created)

From `inject_auth.py`, each naming its reason and the wrapper adding "pass
--mount-auth": no `defaultProvider`; no `auth.json` or no entry for the
provider; an entry that is not `api_key`; no known base URL; a base URL that
is not https; a model of that provider with its own `baseUrl`, which would
send its requests past the injector.

## 7. Security notes

- **Where the key is.** In the proxy process's environment (uid 1001), in the
  environment of PID 1 (catatonit, root), and in podman's container config on
  the host. Measured from the agent uid (§8): `/proc/<pid>/environ` of PID 1
  and of the proxy refuse, the agent's own environment holds no key, and no
  readable file contains one. `podman exec` from the host would hand the key
  to the exec'd process; that is the host user acting, outside this model.
- **`podman secret`** (`uid=1001,mode=0400`) would keep the key out of the
  container config too, at the cost of creating and removing a named secret
  per run in the reaper. Not taken: the remaining holders are root or the host
  user.
- **`--token-auth` puts its token on podman's argv** (`--env NAME=$(cat …)`),
  which every user of the host can read in `ps`. Out of scope here; the inject
  path does not repeat it.
- **Spend.** The agent can use the key on any path of the one upstream. A
  provider key with a credit limit bounds that.

## 8. Verification (2026-10-29)

Through the wrapper (`scripts/sandbox/pi-in-podman.sh --omit-harness-cli
--approve --print -- …`, image rebuilt with this change, pi 1.4.0, openrouter),
from a linked worktree:

| # | Check | Result |
|---|---|---|
| V1 | one-shot answers | `PONG`; runner log `auth=inject`; proxy log one `INJECT POST openrouter.ai /api/v1/chat/completions -> 200` |
| V2 | `--continue` in a second container | recalled the previous answer |
| V3 | `probe.sh` run by pi's own bash tool | `config_auth: per-run-root`; `config_credential: inject=https://openrouter.ai key_in_my_env=no key_in_readable_environs=0 (unreadable: 2) files:.pi/agent/auth.json(2B)` — the two unreadable are PID 1 and the proxy; pi writes the `{}` itself |
| V4 | the host's own `pi --continue` in the same cwd | resumed the container's conversation |
| V5 | without the `no_proxy` change | pi `Connection error.`; proxy log `DENY 127.0.0.1:18081` ×4 |
| V6 | agent uid searching every readable `/proc/*/environ` and the filesystem for the key's prefix | PID 1 and the proxy refused; no hit |

V5 and V6 ran on a hand-assembled `podman run` with the same flags as the
wrapper and a prototype injector (a separate uid-1001 process), before the
wrapper change; V3 repeats V6's environ half on the shipped build.

claude (one-shot, `-p`): `ANTHROPIC_BASE_URL=http://127.0.0.1:18081` and
`CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-sandbox-placeholder` in the container, a
valid OAuth access token injected: `PONG`, one `POST /v1/messages -> 200`, no
CONNECT to any host. An interactive session was not measured; it may contact
endpoints the one-shot does not. Wiring it would inject the `setup-token`,
since an access token expires within hours. **Replaced by Amendment 1**: the
setup-token on this host had stopped working (401 with no injection at all),
and the operator chose the host's own refreshing login over issuing a
year-long token; the expiry is handled by the CLI's own refresh.

## 9. Testing

- `cmd/sandbox-connect-proxy/inject_test.go`: the agent's `Authorization` is
  replaced, `Host` is the upstream's, no `X-Forwarded-For`, a stream arrives
  event by event, an unreachable upstream is a 502, upstream parsing.
- `scripts/sandbox/test_inject_auth.py`: the generated config and every
  refusal in §6.
- `scripts/sandbox/test_history_window.py`: the podman argv under default
  injection (no `auth.json`, no key on the argv, the key in podman's
  environment), with `--history-window`, the refusals in §5, and that a
  refusal leaves no temp root.

## Amendment 1 (2026-10-29): claude, through a host relay

DECIDED (operator, 2026-10-29): claude injects by default too, with the
credential the host's claude already holds, not a `setup-token`.

### Problem

claude's credential under mount auth is `~/.claude/.credentials.json`: an
OAuth access token, the refresh token and a device token, all readable and
rewritable from the container. Two things rule out pi's mechanism:

- **The access token lives 8 hours** (`expiresAt` − the file's mtime = 8.00 h)
  and the host's claude refreshes it about 5 minutes before expiry. A copy
  handed to the proxy at start would die mid-session; the proxy (uid 1001)
  cannot read the host file (uid 1000, 0600) to follow it.
- **The refresh token rotates on every use** (third-party reports; consistent
  with the measurement below, where both tokens changed). A second refresher
  that does not write back logs the CLI out, so nothing but the CLI may refresh.

Hiding only the credential file under mount auth does not work either: a file
bind-mounted over `~/.claude/.credentials.json` inside the `~/.claude` mount is
detached the moment the host's claude renames a refreshed file over it, and
the container then sees the new real file (measured: a placeholder over-mount
read `REAL-2` after a host `mv`; the kernel lazily unmounts a mount whose
mountpoint is unlinked or renamed over in another namespace).

### The mechanism

```
agent (uid 1000)                 proxy (uid 1001)                      host: relay (wrapper's pid)
 ANTHROPIC_BASE_URL ── HTTP ──▶  inject listener ── unix socket ──▶   reads .credentials.json per request
 placeholder OAuth token          adds nothing                         Authorization: Bearer <access token>
                                                                       ── TLS ──▶ https://api.anthropic.com
```

- **The relay** is `sandbox-connect-proxy relay --kind claude` (same binary as
  the proxy, `relay_unix.go`, built into `bin/` by `make build`). The wrapper
  starts it before podman with `--parent-pid $$` and it exits when that pid
  does; the reaper removes its directory. It ignores SIGHUP/SIGINT/SIGQUIT,
  since it shares the agent's PTY session.
- **Refresh**: within 5 minutes of `expiresAt` the relay runs the host's
  `claude auth status --json` once, under a flock shared by every relay of the
  user, then re-reads the file. Measured on claude 2.1.312 with `expiresAt`
  set one minute past, and again with three minutes left: both tokens changed
  and `expiresAt` moved 480 minutes out. A failed refresh still sends the token
  read (the upstream's 401 is more useful to the agent than a 502).
- **The socket** is bind-mounted at `/run/harness-relay/sock`. Under keep-id
  its mode cannot tell the agent from the proxy, so `entrypoint.sh` gives the
  directory around it to the proxy uid, 0700, before the agent starts
  (`probe.sh`: `relay_socket=unreachable`). The host side is in a mktemp 0700
  directory.
- **The agent's environment**: `ANTHROPIC_BASE_URL=http://127.0.0.1:18081`,
  `CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-sandbox-placeholder`, and
  `CLAUDE_CODE_SUBSCRIPTION_TYPE` / `CLAUDE_CODE_RATE_LIMIT_TIER` copied from
  the host's login by `inject_auth.py claude`. Without the last two claude
  labels itself "Claude API" and defaults to claude-sonnet-5-5 where the host
  defaults to claude-opus-5-5; both are read by the claude 2.1.312 binary and
  undocumented. A placeholder credential FILE instead does not work: claude
  then checks the login at api.anthropic.com directly, past the base URL, and
  fails with "OAuth session expired".
- **The per-run root** (`~/.claude`): the host's whole `projects/` (rw);
  copies of `~/.claude.json` and `settings.json` (rw, discarded); `CLAUDE.md`,
  `skills/`, `agents/`, `commands/`, `hooks/`, `plugins/` read-only where the
  host has them; no `.credentials.json`. With `--history-window` the window
  rules apply (only this cwd's projects directory, no host settings).
- **Settings the agent saves as its default are carried back.** `/model`
  writes `model` and `/effort` writes `modelSettings` (measured: nothing else
  changed). On the host those take effect at once, so the reaper runs
  `config_sync.py` every 5 seconds and once at the end, writing to the host's
  `settings.json` only those keys, only when the container changed them. Hooks,
  permissions and env never leave the container.

### Decisions taken

| # | Decision | Decided-by |
|---|---|---|
| A1 | claude injects by default | operator, 2026-10-29 |
| A2 | The credential is the host's refreshing `/login`, not a `setup-token` | operator ("long-lived token" declined) |
| A3 | Only the CLI refreshes; the relay triggers it with `claude auth status --json` | author, on the measurement |
| A4 | The relay runs on the host, so neither token enters the container | author |
| A5 | The relay is a mode of `sandbox-connect-proxy`, chosen by `--kind`, with agent-neutral flags (`--credentials`, `--agent-bin`) | author; flag naming at the operator's prompting |
| A6 | Host config carried as copies + read-only mounts, not a rw mount, so the container cannot change what the host's claude runs | author |
| A7 | `model` and `modelSettings` are synced back every 5 s, matching the host's immediate effect | operator raised the difference; author on the measured keys |

### Verification (2026-10-29, claude 2.1.312)

| # | Check | Result |
|---|---|---|
| C1 | wrapper one-shot | `PONG`, user `CLAUDE.md` loaded; proxy `INJECT POST sandbox-relay /v1/messages -> 200`, relay `INJECT POST api.anthropic.com /v1/messages -> 200`; afterwards no relay process and no temp directory left |
| C2 | `--continue` in a second container | recalled the previous answer |
| C3 | `probe.sh` run by claude | `config_credential: inject=unix:/run/harness-relay/sock key_in_my_env=no key_in_readable_environs=0 (unreadable: 2) relay_socket=unreachable files:none` |
| C4 | the host's own `claude --continue` in the same cwd | resumed the container's conversation |
| C5 | interactive, through a dummy harness's runner | banner "Opus 5.5 with high effort · Claude Team"; a typed prompt answered; only `POST /v1/messages` reached the relay |
| C6 | `--model sonnet`, `--model haiku`, interactive `/model sonnet` | each answered on that model |
| C7 | new wrapper on the previous image | entrypoint `FATAL: credential injector not listening`, agent not started |
| C8 | relay that fails to start | wrapper exits 2 with the relay's own reason and "pass --mount-auth"; nothing left behind (offline test) |

### Testing

- `relay_unix_test.go`: a fresh token used as is; the CLI run within 5 minutes
  of expiry and after it; one run under 20 concurrent requests; the file
  re-read per request; a failed refresh still sends the token; a file without
  an OAuth login refused.
- `inject_test.go`: the container side forwards to the relay socket without
  the agent's `Authorization` / `X-Api-Key`; a failing key source is a 502 and
  sends nothing.
- `test_history_window.py`: claude's default argv (no credential anywhere, the
  copies, the read-only mounts, the environment, the relay's flags), with
  `--history-window`, relay failure, and the reaper carrying `model` back but
  not `hooks` (negative control: without the final sync the test fails).
- `test_config_sync.py`, `test_inject_auth.py` (`claude` mode).
