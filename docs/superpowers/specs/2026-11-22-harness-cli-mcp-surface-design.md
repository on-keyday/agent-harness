# harness-cli mcp — the harness control plane as an MCP server — Design

Date: 2026-11-22

A verb, `harness-cli mcp`, that serves the harness control plane to a coding
agent over the Model Context Protocol (MCP) on stdio. It is a **fourth consumer**
of the declaration in `cli/verb/table.go`, beside the CLI, the TUI and the
WebUI — the extension `2026-09-03-cli-verb-ssot-design.md` says the table was
shaped to make such a consumer cheap ("MCP or any fourth consumer. This design
makes a fourth consumer cheap; it does not add one."). That document also
records MCP as a v2 item (`2026-04-28-agent-comms-design.md:21`). This is that
consumer.

Read `2026-09-03-cli-verb-ssot-design.md` first, and the constants it names:
`cli/verb/verb.go` (the declaration), `cli/verb/gen/main.go` (the generator),
`cli/verb/actions_gen.go` (its output), `cmd/harness-cli/main.go` (the surface's
dispatch) and `cmd/harness-cli/dispatch.go` (`cliVerbs`).

> **Revision note (2026-11-22, second pass).** The first draft exposed a
> hand-picked eight verbs and called that "the non-interactive, JSON-emitting
> subset". Nine verbs in that list's neighbourhood were left out for no written
> reason, so the subset was a policy in a place that said it was a mechanism.
> This revision inverts the default (D10), states the one structural exclusion
> (§6), and adds the capability filter (`--caps`, §8). It also removes two
> surfaces that were declared but not implemented: the `--stdio` flag and the
> `MCPTool.Name` override.
>
> **Revision note (2026-11-23, third pass).** D13's declared `VerbSpec.Cap` was
> the server's knowledge written a second time, and a copy of it drifts in the
> direction that fails silently (a usable tool hidden). The server's knowledge
> now lives in one place (`authority.RequestNeed`,
> `2026-11-22-request-cap-need-design.md`), so this pass replaces `Cap` with the
> one edge the table can own: which request kinds a verb SENDS (D13, §8). It
> also writes down what a survey of the bodies found that the second pass did
> not: bodies that `os.Exit`, read `os.Stdin` or take the terminal (§6, §7), and
> corrections to §6's exclusion list.
>
> **Revision note (2026-11-23, fourth pass).** The third pass served tool calls
> one after another, so a call that waits held up every call behind it, and §6
> excluded every verb that waits for that reason. MCP does not require that
> order: requests may overlap, and a client may cancel one
> (`notifications/cancelled`). This pass serves calls concurrently and cancels
> them on request (D18), which removes the reason the waiting verbs were
> excluded; they become tools (D19). The `Sends` guard, which was process-wide
> on the assumption of one verb at a time, moves onto the call's context (§8.2).

---

## 1. Problem

Today every harness operation an agent can perform is reached by **shelling out
to `harness-cli`** from a general-purpose Bash tool. That is what the sandbox
bridges in (`scripts/sandbox/agent-in-podman.sh`: the host `harness-cli` is
bind-mounted read-only at `/usr/local/bin/harness-cli`, and every `HARNESS_*`
name is forwarded). An MCP client can do none of it natively, so:

- the model must own the CLI's grammar, its stdout shapes and its identity
  resolution, instead of receiving typed tools;
- there is no declaration of *which* operations exist as tools, and no place to
  attach per-tool policy later;
- nothing separates "the harness API" from "a shell", which is exactly the axis
  a future tool-boundary control (a mediated, non-Bash surface) would need.

The grammar already exists once. This spec exposes it, it does not restate it.

## 2. Non-goals

- **Removing or confining Bash.** This adds a typed surface; it does not take
  the shell away, and with Bash present the model can still call `harness-cli`
  directly. What it buys is a place a *later* design can stand on. Stated here
  so a reader does not mistake a tool surface for a boundary.
- **The sandbox execution broker.** A separate item (a `run_tests` / `build` /
  `git` server, no general shell) lives with `scripts/sandbox/`, not here.
- **A new binary.** `harness-cli mcp` is a verb of the existing binary. A
  separate `cmd/harness-mcp` would duplicate the table, the generator output and
  the dispatch, and would need its own build and its own sandbox bind mount; the
  sandbox already bridges `harness-cli`.
- **MCP resources, prompts, sampling, HTTP/SSE transport.** Tools over stdio
  only. The legacy SSE transport is not offered by every client and is not
  needed here.
- **A second transport flag for a transport that does not exist.** There is no
  `--stdio` flag. stdio is the only transport, and a flag that selects nothing
  is a surface a reader has to investigate and no code ever reads.
- **Per-verb authority.** The `cli/verb` layer is not the enforcement point
  (`2026-09-03-cli-verb-ssot-design.md`: request-kind → capability lives in
  `server/capabilities.go`). The MCP surface does not change that; caps stay the
  enforcement (§10, §11).
- **Guarding credentials in results.** `cli/ticket_guard.go` refuses to
  *publish* a body containing this task's ticket. MCP results are not covered by
  it here (§10), and this spec does not claim to fix the model channel.

## 3. The mechanism

```
model
  │  tools/call { name, arguments }
  ▼
MCP client (pi / claude / codex), spawned by the agent
  │  stdio JSON-RPC 2.0
  ▼
harness-cli mcp                      (the verb; blocks, owns stdout)
  │  argv from arguments → spec.For(CLI).Parse (validate) → BuildFunc → Action
  │  DispatchCLIAction(cliVerbs{out: buf}, act)
  ▼
cliVerbs body                        (the SAME body the CLI surface runs,
  │                                   writing to its injected sink)
  ▼
server  ── caps enforced here (server/capabilities.go)
```

`tools/list` is generated from the same `VerbSpec` table the CLI is. A tool
call is the CLI call it names, run through the same parser, the same validation
and the same handler.

## 4. Decisions taken

`Decided-by` is provenance. Rows marked `author` are proposed by whoever wrote
this spec and are open to the operator; a later reader may overturn them on
evidence.

| # | Decision | Decided-by |
|---|---|---|
| 1 | The surface is a verb, `harness-cli mcp`, not a new binary | author |
| 2 | It speaks MCP tools over stdio only; no resources/prompts/HTTP. There is no `--stdio` flag; the transport is a fact of the process, not a choice | operator |
| 3 | Which verbs become tools is DECLARED on the `VerbSpec`, not listed in the MCP code | author |
| 4 | A tool call runs IN-PROCESS: synthesize argv → `VerbSpec.Parse` (the only entry that validates) → `BuildFunc` → the same `cliVerbs` body the CLI runs | author |
| 5 | `cliVerbs` gains an injected output writer, so the MCP consumer captures a result without re-exec and without a global `os.Stdout` swap. Every tool's body MUST write through it (§7) | author |
| 6 | Tool identity is unchanged: the `mcp` process inherits `HARNESS_*` and dials as the task; caps are the only boundary | author |
| 7 | Tool names are the verb path joined with `_` (`agent_inbox`). MCP's tool-name grammar is `[A-Za-z0-9_-]`, so a dot is not a legal name. There is no per-tool override; a path that cannot be a name is a declaration error to fix in the table, not a field here | operator |
| 8 | MCP annotations (`readOnlyHint`, `destructiveHint`) are DERIVED from the table's existing destructive/breadth markers | author |
| 9 | The `mcp` verb itself is never a tool | author |
| 10 | **Inclusion by default.** Every verb with a CLI surface is a tool. A verb is withheld ONLY by carrying an explicit `MCPNotATool` reason, and the reason says why it cannot be a single request/response (§6). There is no hand-picked list | **operator** |
| 11 | stdout is the JSON-RPC channel: the `mcp` verb writes nothing else to it | author |
| 12 | Side effects are declared per tool (`read`/`write`). `--read-only` drops `write` tools. The default surface is the whole CLI table | **operator** |
| 13 | ~~Each verb DECLARES the capability its call needs (`VerbSpec.Cap`)~~ — **replaced 2026-11-23**: each tool verb declares the request kinds it SENDS (`VerbSpec.Sends`); the capability a tool may need is DERIVED from those kinds through `authority` (§8). The server remains the only enforcement | **operator** (2026-11-22; replacement 2026-11-23) |
| 14 | A body that sends a kind its verb does not declare is refused at the client, on the CLI surface as on MCP: the declaration cannot drift silently from the body (§8.2) | **operator** (2026-11-23) |
| 15 | A `tools/call` naming an unknown tool, or one the server's own filters (`--read-only`, `--caps`, the caller's caps) withheld, is a JSON-RPC error (`-32602`), not a tool result: from the client's view that tool does not exist | author |
| 16 | A tool body neither ends the process nor reads its stdin: `os.Exit` in a body becomes a returned exit code, and the body's stdin is injected like its stdout (§7) | author |
| 17 | A verb that writes only when a flag is given declares that flag in `MCPTool.WriteFlags` and stays `read`: a `--read-only` server lists it with the flag omitted (and refuses it as unknown), any other server lists it whole with `readOnlyHint: false`. `record get -o`, `transcript get -o`, `skill --extract`/`--force` | **operator** (2026-11-23) |
| 18 | `tools/call` requests run concurrently, each under its own context; `notifications/cancelled` cancels that context and the cancelled request gets no response. `initialize`, `ping` and `tools/list` are answered in arrival order by the reading loop, so a waiting call never delays them. No bound on concurrent calls: a bound would queue a fast call behind waiting ones, and the client is the agent's own child with the agent's own authority | **operator** (2026-11-23) |
| 19 | A verb that waits for something else and then answers once is a tool: `session exec`, `session await-idle`, `await-progress`, `agent wait`, `agent dispatch`. A waiting call occupies only itself (D18), and the client's own timeout ends it through cancellation. Streaming and terminal-owning verbs stay excluded | **operator** (2026-11-23) |

## 5. Interface

### 5.1 The verb

```
mcp [--read-only] [--caps <list>]
    serve the control plane as MCP tools on stdio (one JSON-RPC message per line)
    --read-only  expose only tools whose declaration is not destructive
    --caps LIST  expose only tools some call of which LIST can satisfy
                 (comma-separated grantable cap names; a tool that needs no
                 capability is always exposed, as on a session's --caps)
```

Declared in `cli/verb/table.go` as a normal path, exactly as `version` is
(`Path: []string{"mcp"}, CmdlineSurfaces: CLI`), with `NoModalSurface` set for
the same reason `version` sets it (surveyed: no TUI action, no WebUI element).
It carries `CmdlineSurfaces: CLI` and **no MCP declaration** (D9). The generated
`cliVerbs.Mcp` method blocks in the server loop; `ssh-gateway` is the existing
precedent for a blocking foreground verb.

### 5.2 The declaration

`VerbSpec` gains two fields, beside `CmdlineSurfaces` and `ModalSurfaces`:

```go
// MCP, when non-nil, is this verb's tool declaration. Every verb with a CLI
// surface must carry either MCP or MCPNotATool, and a test refuses a verb with
// neither. It is a pointer so "declared" can be told from "forgotten".
MCP *MCPTool

// MCPNotATool is the other answer, and exists for the same reason
// NoModalSurface does: "we looked, and it cannot be one" is a result worth
// keeping. Non-empty means the verb is NOT a tool and says why -- the only
// admissible reasons are structural (it streams, blocks, or owns a terminal),
// not policy.
MCPNotATool string

// Sends lists the TaskControl kinds this verb's body may send, whatever its
// flags. A tool must declare it (empty for a verb that sends none: version,
// caps, skill, workspace). It is the ONE edge the table owns; which bits each
// kind needs is authority's (§8). A body that sends a kind not listed is
// refused at the client (D14).
Sends []protocol.TaskControlKind

type MCPTool struct {
    // SideEffect is "read" or "write". It fills readOnlyHint/destructiveHint
    // and is what --read-only filters on. A verb that always can write -- the
    // server's state, or a file on this machine (file pull) -- is "write".
    SideEffect MCPEffect

    // WriteFlags are the flags whose presence makes a "read" verb write
    // (record get -o writes a local file). A --read-only server omits them
    // from the schema and refuses them; any other server lists the verb with
    // readOnlyHint false (D17).
    WriteFlags []string

    // Omit lists flags the tool's schema leaves out, and Fixed the tokens every
    // tool call passes ahead of the caller's arguments. Both exist for ONE
    // reason, which Why states: a verb that is a single request/response with
    // some flag set (or unset) would otherwise stream, block or take the
    // terminal (§6). A call naming an omitted flag is refused as an unknown
    // argument.
    Omit  []string
    Fixed []string
    Why   string
}
```

There is no `MCPTool.Name`: the derived `_`-joined name was always the name
(D7), and an override field with no setter is a surface a reader has to check.

The generator (`cli/verb/gen`) is unaffected structurally — `MCP`/`Sends` are
data it carries into no generated code, because the parse it already generates
is what `tools/call` reuses.

### 5.3 Tools

`tools/list` walks `table.go` in table order and emits, for each spec that is a
tool and survives the `--read-only`/`--caps` filters:

```json
{
  "name": "agent_inbox",
  "description": "<UsageLines()[0] + the verb's Notes, generated>",
  "inputSchema": { "type": "object", "properties": { ... }, "required": [ ... ] },
  "annotations": { "readOnlyHint": false, "destructiveHint": true }
}
```

`inputSchema` is a pure function of the declaration:

| declaration | JSON Schema |
|---|---|
| `Args[i]` (required) | property named by `Arg.Name`, `required` |
| `Args[i].Optional` | same property, omitted from `required` |
| `ArgType` (`ArgString`/`ArgTaskID`/`ArgTopic`/`ArgUint`) | `string` / `string` with a `^[0-9a-f]{32}$` pattern / `string` / `integer` minimum 0 |
| `Args[i].Variadic` | `array` of the element type |
| `Flag{Type: FlagBool}` | `boolean` |
| `Flag{Type: FlagString}` | `string` |
| `Flag{Type: FlagUint/Uint64}` | `integer` minimum 0 |
| `Flag{Type: FlagDuration}` | `string` (Go duration) |
| `Modes` | an enum of the mode flag names |
| `Pathspec` | a `pathspec` array → emitted after `--` |
| `Trailing` | a property named by `Trailing.Name`: a `string` (the joined text) for a `Field` tail, an `array` of strings for a `FieldArgs` tail; `required` when `Trailing.Required` and no `UnlessFlag` |
| `MCPTool.Omit` | the named flags are left out |

`Flag.Aliases` are accepted on input as aliases of the canonical name and are
never advertised. `Flag.Default` becomes the schema default where it is not the
zero value.

A positional or trailing value that begins with `-` is passed so the parser
cannot read it as a flag: `--` before it where the verb's grammar allows one,
and a refusal where it does not (a `Pathspec` verb, whose `--` is taken).

`tools/call` reconstructs argv from `arguments` against the same declaration
(positionals in `Args` order, the `Modes` group, `pathspec` after `--`), refuses
an unknown key, then runs the shared path — `spec.For(CLI).Parse` →
`BuildFunc` → `DispatchCLIAction` — and returns:

- handler success → `content: [{ "type": "text", "text": <captured output> }]`
- handler error → the same shape with the error text, and `"isError": true`

Running the CLI-narrowed spec (`For(CLI)`) keeps positional indices identical to
the surface an operator types, and `Parse` is the only entry that runs the
declared checks and `Validate` (`check`, `cli/verb/build.go`); the `BuildFunc`
repackaging does not. So a tool call is accepted and refused exactly where the
CLI line is. The result is whatever the body wrote to the injected sink (§7).

## 6. Refusals and exclusions

- **The `mcp` verb is not a tool** (D9); `tools/list` never contains it.
- **A verb that is not a single request/response is not a tool.** This is the
  whole exclusion set, and it is structural, not policy:
  - it streams until interrupted — `watch`, `notify-watch`, `exec tap`,
    `forward tap`, `session stream attach`;
  - it serves until interrupted — `forward`, `ssh-gateway` (each holds a
    listener open; there is no single answer to return);
  - it owns this process's terminal, editor or stdin — `session attach`,
    `file edit`, `file new` (both run `$EDITOR` on the process's terminal),
    `exec` (splices stdin/stdout to the command and ends the process with its
    exit code), `lifecycle` (a hook: it reads its event JSON from stdin, which
    inside the MCP server is the JSON-RPC stream);
  - it is the server — `mcp`.

  Corrections to the second pass's list, from reading the bodies: `logs` no
  longer exists (retired in A3; `main.go`'s `retiredVerbs`); `session stream
  turn` does not stream — it writes one user turn and returns after
  `--flush-ms` (`cli/stream_write.go` `StreamTurn`), so it is a tool; `file new`,
  `exec` and `lifecycle` were missing.

  Not excluded since the fourth pass (D19): a verb that WAITS and then answers
  once — `session exec` (bounded by `--timeout`, 30s by default), `agent wait`
  and `agent dispatch` (`--timeout`, 5m), `session await-idle` and
  `await-progress` (unbounded long poll; the client's timeout cancels it). A
  cancelled long poll closes its connection, and the server drops a reply-sink
  watcher whose connection is gone (`DropIdleWatchersForConn`), so cancelling
  leaves nothing armed. A cancelled `session exec` cannot take back what it
  typed: the command keeps running in the session's shell. `session
  await-idle` and `await-progress` are `read` with `notify`/`topic` as
  `WriteFlags` (D17): the long poll's watcher lives only as long as the call,
  while a notify or board watcher outlives it.
- A verb with a **flag** that makes it stream, block or take the terminal is a
  tool whose schema omits that flag (`MCPTool.Omit`/`Fixed`, with `Why`):
  - `conns`: `--follow` streams connection events, `--watch` repeats the
    `--trsf` table on an interval — both omitted;
  - `session new`: without `-d` the new session takes over this process's
    terminal — `-d` is Fixed, and `detach`, `x11`, `x11-display` are omitted.
- **`tools/call` on an unknown name** — or on a tool this server withheld by
  `--read-only`, `--caps` or the caller's caps — is a JSON-RPC error (`-32602`)
  naming the reason, not a tool result (D15).

## 7. Why in-process, and the one change it needs

An earlier draft ran each tool call by re-executing the binary
(`os.Executable()`) and capturing its stdout. That is rejected. It forks a
process to reach handlers that live in the same binary and the same package
(`cliVerbs`, `cmd/harness-cli/dispatch.go`); it depends on `os.Executable()`
resolving to a runnable path; and it re-synthesizes argv for a parse the process
already holds. It buys nothing the in-process path does not, and because the CLI
bodies dial a fresh connection per verb anyway (`withClient`), it does not even
save a connection.

What in-process needs is a **sink**. Every `cliVerbs` body writes its result to
`os.Stdout` directly (`FileLs` passes `os.Stdout`; `session.go` encodes to
`os.Stdout`). So `cliVerbs` gained one field, `out io.Writer`, set to
`os.Stdout` where the CLI surface constructs it, and every body writes there
instead. That is 73 sites in `cmd/harness-cli/dispatch.go`, plus `isTTY` widened
from `*os.File` to `io.Writer` (it type-asserts and answers false for a buffer)
— and it is the shape the SSOT design wants: the CLI becomes one consumer that
*chooses* a sink, not the definition of where output goes.
`DispatchCLIAction(cliVerbs{out: buf}, act)` then captures a result with no
subprocess and no second implementation.

The field is read through a nil-safe accessor (`stdout()`), so a `cliVerbs` built
without one — a test — behaves as the CLI does rather than panicking.

**Residual — and this is now load-bearing, not incidental.** The first pass
exposed only eight verbs, so a body that wrote with `fmt.Println` instead of the
sink could not be reached by a tool call. With the whole table exposed (D10),
**every tool body MUST write through the sink** or its output leaks into the
JSON-RPC stream. `dispatch.go` and `session.go` still hold direct `fmt.Print*`
calls; they are moved to `h.stdout()` before the verb that owns each is exposed.
The golden and the stdout-purity test in V1 are what catch a regression: a body
that prints to fd 1 fails the "stdout is JSON-RPC only" assertion.

**Two more things a long-lived server cannot share with a one-shot CLI** (D16):

- **`os.Exit` in a body.** `server dial-runner`, `runner enroll / unenroll`,
  `runner dial add / rm` and `runner kick` print a result and then `os.Exit(1)`
  on a refused status; `session new` exits 3 on an ambiguous runner
  (`exitOnAmbiguous`); `session resize` exits 3 when the size is refused. In the
  MCP server that ends the server. These return an exit-code error instead: the
  CLI's `die()` exits with that code and prints the error's text (empty for the
  ones that already printed), so the command line behaves exactly as before;
  `tools/call` returns the captured output with `isError`, followed by the
  error's text or, when it has none, `exit status N` — the code is the only
  place `session exec` reports its command's status without `--json`.
- **`os.Stdin` in a body.** `board send`, `agent send` read the body from stdin
  when neither text nor `--data` gives one. In the MCP server stdin is the
  JSON-RPC stream, so `cliVerbs` gains `in io.Reader` beside `out`, read through
  a nil-safe `stdin()`; the MCP consumer sets a reader that fails with "a tool
  call has no stdin: give the body as text or --data".

A body's stderr lines (`session send`'s summary, `session mode`'s confirmation)
stay on the MCP server's stderr, which is the client's log, not the tool
result. The result is what the body wrote to its sink (§5.3).

**A third, since calls overlap (D18): the body's context.** Each call runs
under its own context, carried in `cliVerbs.ctx`, and a body MUST dial and
wait with that context, not `context.Background()`. It is what cancellation
reaches, and what carries the call's `Sends` (§8.2); a body that makes its own
context escapes both. The go/ast guard (§11 2b) refuses `context.Background`
and `context.TODO` in tool bodies for that reason.

Rejected alternative: swapping the `os.Stdout` package variable around the call.
It reaches the var-reading bodies, but it is process-global — racy now that MCP
requests overlap — and it silently misses anything that writes fd 1 directly.
A field is explicit and has no such edge.

A truly argv-free form — build `Bound` from the tool arguments and validate it
without `Parse` — is NOT taken. `Parse` is the only entry that runs the declared
checks and `Validate` (`check`, `cli/verb/build.go:202`); the `BuildFunc`
repackaging takes an already-validated `Bound` and does not. Synthesizing argv
reuses the single parse path, and gate V3 pins it to the hand-typed argv.

## 8. Capability filter

The whole CLI table is ~132 verbs. Sending every tool schema to the model is a
real cost, and an operator may want the surface to reflect the authority they
meant to grant. Two filters answer that:

- `--read-only`: drop `write` tools.
- `--caps LIST`: expose only tools some call of which LIST can satisfy.

### 8.1 Where the answer comes from

Which bits a request needs is the server's knowledge, and it now has one home:
`authority` (`2026-11-22-request-cap-need-design.md`), which the server's own
gates read. What it cannot know is which requests a VERB sends — that edge
lived only inside the `cli/` bodies. The table declares it (`VerbSpec.Sends`)
and nothing else; no bit is written in the table.

A tool is satisfiable by a set of bits `held` when ANY kind it sends has SOME
request that `held` satisfies: every clause of `authority.RequestNeed` for that
request has a bit in `held` (or its implication), with a `Later` requirement
assumed satisfiable — the lookup it waits on may answer either way.
`authority.MaySatisfy(kind, held)` answers that for one kind over the same
variants `authority.Possible` enumerates. A tool sending no kind (`version`,
`caps`, `skill`, `workspace`) is always satisfiable.

The rule is deliberately generous. A verb sending a cheap kind and a costly one
(`credential revoke` reads the task list, then cancels) is shown to a caller
who can only do the first; the server refuses the second when it is called. The
opposite error — hiding a tool the caller could use — fails silently, because a
model does not call a tool it was not shown, so every imprecision here lands on
the shown side.

### 8.2 Keeping `Sends` honest

`Sends` is a declaration beside the body it describes, so it is enforced where
the body acts: the dispatch of a verb that declares `Sends` puts that set on
the context the body runs under (`cli.WithExpectedKinds`), every client the
body dials from that context records it, and a body that sends a kind outside
it gets an error naming the verb and the kind instead of a request on the wire
(D14). That is so on the command line as well as in the MCP server — a
forgotten kind shows up the first time anyone runs the verb, not as a quietly
wrong tool list. The TUI and WebUI, which share `cli.Client` but not this
dispatch, put nothing on their contexts and are not checked.

The set travels on the context, not in a process-wide variable, because MCP
calls overlap (D18): two verbs run at once, each with its own declaration. A
body that dials from a context of its own making loses the set and is not
checked, which is one of the two reasons §7 forbids it.

### 8.3 Automatic and explicit

`tools/list` applies the rule with `held` = the connection's own caps (from
`whoami`; an operator holds every bit), intersected with `--caps` when given.
`--caps` therefore narrows, never widens past the caller. If `whoami` fails,
the list is served without the caller-caps half and the failure is logged to
stderr — the filter saves context, it never authorizes, so an unfiltered list
is the safe fallback.

The cost is that the tool list is a function of the connection, so two tasks
with different caps see different lists. That is the point; `tools/list` is not
a capability document.

## 9. Verification gates

Each is measured before the dependent part ships and recorded with the command
that produced it.

| # | question | blocks |
|---|---|---|
| V1 | Does a real MCP client connect to `harness-cli mcp`, complete `initialize`, and list tools with no stray output on stdout — including with the whole table exposed? | the whole surface |
| V2 | Does a `tools/call` return the same bytes as the equivalent `harness-cli <verb>` run directly, for a read verb and a write verb? | D4 |
| V3 | Does schema → argv reconstruction reproduce the Action a hand-typed argv produces, including a `Variadic`, an `Optional`, a `Modes` group and a `Pathspec` verb? | §5.3 |
| V4 | Is `mcp` absent from `tools/list`, and is every streaming/interactive verb absent by declaration? | §6 |
| V5 | Under `--read-only`, is a `write` tool refused without running? | D12 |
| V6 | Does the same identity apply — is a tool call denied by the server exactly where the direct CLI call is denied (caps), with no bypass? | D6 |
| V7 | Under `--caps`, is a tool no call of which the list can satisfy absent from `tools/list` and refused by `tools/call`? | §8 |
| V8 | With a caller that cannot satisfy a tool, is the tool absent from `tools/list` — and is a tool the caller CAN use under some request never absent (agent send without board_peer, file ls with only file_write)? | §8.1, §8.3 |
| V9 | Does a body that sends a kind outside its verb's `Sends` fail before the request leaves, on the CLI and in a tool call? | D14 |
| V10 | Does no tool body end the MCP server or read its stdin (`os.Exit`, `os.Stdin` in a tool's body)? | D16 |
| V11 | While one `tools/call` waits, is a second `tools/call` and a `ping` answered? Does `notifications/cancelled` end the waiting call with no response, and does a cancelled `session await-idle` leave no watcher armed? | D18 |
| V12 | With two calls of different verbs in flight, does each see only its own `Sends`? | §8.2 |

## 10. Security notes

- **No new privilege and no new reach.** The MCP server is a child of the
  agent's MCP client, inside the same sandbox: same uid, same seccomp, same
  caps, no listener (stdio). It inherits the task's `HARNESS_*` and dials as the
  task, so the authority of a tool call is the authority of the agent that made
  it — the server's caps, unchanged (D6).
- **A typed surface is not a boundary.** Because Bash coexists, the same
  operations remain reachable by other means; the surface adds no confinement.
  Its value is declaration (D3) and a place to attach per-tool policy later.
- **The advertised cap is not a gate.** `--caps` and the automatic filter change
  what is *listed*, never what is *allowed*. A tool omitted from the list is
  still refused by the server if called directly (V8).
- **Result content is on the model channel.** A tool result is returned to the
  client and shown to the model, so anything a verb prints is published to the
  provider. `cli/ticket_guard.go` covers the board-publish path only; an MCP
  result containing the task's own ticket is not covered. Accepted residual: the
  ticket's authority equals the agent's, so nothing escalates, but a per-tool
  redaction hook at this layer is the natural place to close it.

## 11. Testing

1. **Completeness (build property + test).** Every verb with a CLI surface
   carries exactly one of `MCP` / `MCPNotATool`; `mcp` and every interactive or
   streaming verb carries `MCPNotATool` with a structural reason; every tool's
   name is unique after sanitization. In the style of
   `cmd/harness-cli/usage_completeness_test.go` and `cli/verb/invariant_test.go`.
2. **The filter rule.** `authority.MaySatisfy` per kind (a no-bit request
   exists → always satisfiable; ListFiles with file_write alone; a reach-only
   kind like AgentSend never hidden), and the tool-level rule over `Sends`
   (ANY kind satisfiable; no kind → always). The V8 cases are table rows.
2a. **`Sends` is enforced.** A client carrying a verb's `Sends` refuses an
   undeclared kind before sending (V9); every tool declares `Sends` (a test
   refuses a tool with a nil `Sends`).
2b. **No body ends or reads the process, or leaves the call's context.** A
   go/ast scan of `cmd/harness-cli` refuses `os.Exit`, `os.Stdin`, `os.Stdout`,
   `fmt.Print*`, `context.Background` and `context.TODO` inside the bodies of
   tool verbs, with an allowlist naming the not-a-tool bodies (V10, §7).
2c. **Overlap and cancellation.** The serve loop with an injected call runner:
   a call that blocks until its context ends does not delay a second call or a
   `ping`; `notifications/cancelled` naming it ends it and no response is
   written for it; end of input cancels what is in flight. The `Sends` set of
   one context is not seen by a client dialed from another (V11, V12).
3. **Schema golden.** `tools/list` for the whole table, diffed against a
   committed golden, so a table change that moves the tool surface is visible.
4. **Round-trip.** For each tool, generate an `arguments` object from its
   schema, reconstruct argv, and assert the produced Action equals the Action
   from the canonical argv.
5. **Stdio session.** Drive `harness-cli mcp` with an in-process JSON-RPC client
   on pipes (no network): `initialize`, `tools/list`, a `tools/call`, a malformed
   frame, and a `--read-only`/`--caps` list; assert stdout is JSON-RPC only.
6. **Real client.** One call from a real agent (pi) with `mcp.json` pointing at
   `harness-cli mcp`, verifying the tool appears and one read verb answers.
7. **All tool bodies use the sink.** A test (or the stdout-purity assertion)
   fails if any exposed verb writes to fd 1 directly (§7).

## 12. Enablement (not this spec's to implement, recorded for the reader)

Outside the sandbox, one line:

```json
{ "mcpServers": { "harness": { "command": "harness-cli", "args": ["mcp"] } } }
```

Inside the sandbox, `harness-cli` is bridged
(`/usr/local/bin/harness-cli:ro`), but no `mcp.json` is mounted and the per-run
config root does not carry one, so a sandboxed agent sees no MCP servers.
Wiring that (`mcp.json` into the per-run root, exposing `harness-cli mcp`) is a
`scripts/sandbox` change with its own review; this spec only notes the seam.

Two properties of the sandbox decide how the seam closes, and neither is about
this verb in particular:

- `~/.pi` is a **per-run tmpfs** and `~/.pi/agent` is writable. An `mcp.json`
  written there works for the life of the container but is gone on the next
  start. Durable enablement means adding the file to the per-run config the
  wrapper builds, not just writing it once.
- The bridged binary is a **snapshot taken at container start**. A running
  container keeps the binary it was started with, so a newly added verb is not
  reachable until `make build` produces a new binary and the container restarts
  with it. Landing the code is not enough.

## 13. Open questions

- Whether the `description` should be `UsageLines()` or the `Notes` prose; the
  former is stable and short, the latter is what an operator reads.
- A side effect a POSITIONAL decides (`restore` lists with no ids and restores
  with them) is not expressible by `WriteFlags`; such a verb stays `write`.

Resolved by the third pass: the `Cap` link (the verb → kind edge is now the
table's `Sends`, the bits are `authority`'s), flag-dependent streaming (`logs`
is gone; `conns` and `session new` use `Omit`/`Fixed`, §6), and a per-call side
effect decided by a flag (`WriteFlags`, D17).
