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
- claude. Measured to work (§8), not wired.

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
| 3 | Implement pi now; measure claude in the same change, do not wire it | operator, 2026-10-29 |
| 4 | Plain HTTP on loopback + the agent's base URL, not a TLS MITM of the CONNECT tunnel | author |
| 5 | The injector lives in the CONNECT proxy binary, under its uid: no new process, no new image file | author |
| 6 | The key reaches the proxy through the environment, dropped by the entrypoint before the agent; not `podman secret` (§7) | author |
| 7 | Injection reuses the per-run config root, binding the **whole** sessions store unless `--history-window` narrows it, so the operator's cross-cwd resume keeps working | author |
| 8 | Only the proxy mode can inject: `--firewall` / `--no-firewall` with injection exit 2 and name `--mount-auth`; no fallback | author |
| 9 | Only an `api_key` credential; a base URL from the host's `models.json` or a built-in table holding the one measured provider (openrouter) | author |
| 10 | `Authorization: Bearer` is the only injected header form. Both measured cases (openrouter, claude's OAuth token) use it | author |

## 5. Interface

- `--inject-auth`: pi only (`A_INJECT` in the agent table). Exits 2 for any
  other agent, together with `--mount-auth` or `--token-auth`, and outside the
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
since an access token expires within hours.

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
