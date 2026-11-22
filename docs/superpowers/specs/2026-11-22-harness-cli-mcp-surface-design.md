# harness-cli mcp — the harness control plane as an MCP server — Design

Date: 2026-11-22

A new verb, `harness-cli mcp`, that serves the harness control plane to a
coding agent over the Model Context Protocol (MCP) on stdio. It is a **fourth
consumer** of the declaration in `cli/verb/table.go`, beside the CLI, the TUI
and the WebUI — the extension `2026-09-03-cli-verb-ssot-design.md` says the
table was shaped to make cheap ("MCP or any fourth consumer. This design makes
a fourth consumer cheap; it does not add one."). That document also records MCP
as a v2 item (`2026-04-28-agent-comms-design.md:21`). This is that consumer.

Read `2026-09-03-cli-verb-ssot-design.md` first, and the constants it names:
`cli/verb/verb.go` (the declaration), `cli/verb/gen/main.go` (the generator),
`cli/verb/actions_gen.go` (its output), `cmd/harness-cli/main.go` (the surface's
dispatch) and `cmd/harness-cli/dispatch.go` (`cliVerbs`).

---

## 1. Problem

Today every harness operation an agent can perform is reached by **shelling out
to `harness-cli`** from a general-purpose Bash tool. That is what the sandbox
bridges in (`scripts/sandbox/agent-in-podman.sh`: the host `harness-cli` is
bind-mounted read-only at `/usr/local/bin/harness-cli`, and every `HARNESS_*`
name is forwarded). An MCP client can do none of it natively, so:

- the model must own the CLI's grammar, its stdout shapes and its identity
  resolution, instead of receiving typed tools;
- there is no declaration of *which* operations are safe to hand to a model as
  tools, and no place to attach per-tool policy later;
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
- **Per-verb authority.** The `cli/verb` layer holds no capability map, by
  design (`2026-09-03-cli-verb-ssot-design.md`: request-kind → capability lives
  in `server/capabilities.go`). The MCP surface does not add one; caps stay the
  enforcement point.
- **Guarding credentials in results.** `cli/ticket_guard.go` refuses to
  *publish* a body containing this task's ticket. MCP results are not covered by
  it here (§9), and this spec does not claim to fix the model channel.

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
and the same handlers, in the same process. The MCP server is a transport and a
declaration filter, never a second implementation; the only thing that differs
from the CLI surface is where a verb's result is written (§7).

## 4. Decisions taken

`Decided-by` is provenance. Rows marked `author` are proposed by whoever wrote
this spec and are open to the operator; a later reader may overturn them on
evidence.

| # | Decision | Decided-by |
|---|---|---|
| 1 | The surface is a verb, `harness-cli mcp`, not a new binary | author |
| 2 | It speaks MCP tools over stdio only; no resources/prompts/HTTP | author |
| 3 | Which verbs become tools is DECLARED on the `VerbSpec`, not listed in the MCP code | author |
| 4 | A tool call runs IN-PROCESS: synthesize argv → `VerbSpec.Parse` (the only entry that validates) → `BuildFunc` → the same `cliVerbs` body the CLI runs | author |
| 5 | `cliVerbs` gains an injected output writer, so the MCP consumer captures a result without re-exec and without a global `os.Stdout` swap (§7) | author |
| 6 | Tool identity is unchanged: the `mcp` process inherits `HARNESS_*` and dials as the task; caps are the only boundary | author |
| 7 | Tool names are the verb path joined with `_` (`agent_inbox`). MCP's tool-name grammar is `[A-Za-z0-9_-]`, so a dot is not a legal name; the `_` join is the derived name and `MCPTool.Name` overrides it | author |
| 8 | MCP annotations (`readOnlyHint`, `destructiveHint`) are DERIVED from the table's existing destructive/breadth markers | author |
| 9 | The `mcp` verb itself is never a tool | author |
| 10 | Exclusions are by omission: a verb is a tool only if its declaration says so | author |
| 11 | stdout is the JSON-RPC channel: the `mcp` verb writes nothing else to it | author |
| 12 | Default exposure is the non-interactive, JSON-emitting subset; mutating verbs are declared too, with `destructiveHint`, and a `--read-only` filter can drop them | author (needs operator) |

## 5. Interface

### 5.1 The verb

```
mcp [--stdio] [--read-only]
    serve the control plane as MCP tools on stdio (one JSON-RPC message per line)
    --stdio      the transport (the only one; accepted for symmetry with future ones)
    --read-only  expose only tools whose declaration is not destructive
```

Declared in `cli/verb/table.go` as a normal path, exactly as `version` is
(`Path: []string{"mcp"}, CmdlineSurfaces: CLI`), with `NoModalSurface` set for
the same reason `version` sets it (surveyed: no TUI action, no WebUI element).
It carries `CmdlineSurfaces: CLI` and **no MCP declaration** (D9). The generated
`cliVerbs.MCP` method blocks in the server loop; `ssh-gateway` is the existing
precedent for a blocking foreground verb.

### 5.2 The declaration

`VerbSpec` gains one field, beside `CmdlineSurfaces` and `ModalSurfaces`:

```go
// MCP, when non-nil, exposes this verb as an MCP tool. Presence is the whole
// statement: a verb the model may call with no further policy is declared once,
// here, and every MCP-aware consumer reads the same row. It is a pointer so
// "surveyed and deliberately not exposed" can be told from "no opinion", the
// same distinction NoModalSurface draws for modal surfaces.
MCP *MCPTool

type MCPTool struct {
    // Name overrides the derived tool name (path joined with "."). Empty means
    // derive. Set only where the path is not a usable tool name.
    Name string
    // SideEffect is "read" or "write". It fills readOnlyHint/destructiveHint
    // and is what --read-only filters on. It is DECLARED because the table
    // already knows: a verb with a WidensIfUnset positional or a destructive
    // Note is exactly the one a model must not run unattended.
    SideEffect MCPEffect
}
```

The generator (`cli/verb/gen`) is unaffected structurally — `MCP` is data it
carries into no generated code, because the parse it already generates is what
`tools/call` reuses. This keeps `TestGeneratedFileIsCurrent` meaningful.

### 5.3 Tools

`tools/list` walks `table.go` in table order and emits, for each spec with
`MCP != nil`:

```json
{
  "name": "agent.send",
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

`Flag.Aliases` are accepted on input as aliases of the canonical name and are
never advertised. `Flag.Default` becomes the schema default where it is not the
zero value.

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
CLI line is. The result is whatever the body wrote to the injected sink (§7);
stdout stays clean because the JSON-RPC stream and a verb's sink are two
writers, not two processes.

## 6. Refusals and exclusions

- **The `mcp` verb is not a tool** (D9); `tools/list` never contains it.
- **Interactive and streaming verbs are not tools** — `session attach`,
  `session exec`, `session stream *`, `session send`, `grid`, `ssh-gateway`,
  `exec tap`, `forward` (open/tap), `board wait`, `agent wait`. They are
  excluded by not carrying `MCP`, not by a denylist in the server (D10). A test
  asserts none of them carries `MCP`.
- **`tools/call` on an unknown name** returns an MCP error, not a shell error.
- **A `--read-only` server** refuses a `write` tool with an MCP error naming the
  flag, rather than running it (D12).
- **No `--stdio`/`--http` ambiguity:** only stdio exists; an unknown transport
  argument exits 2 like any other unknown flag.

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
`os.Stdout`). So `cliVerbs` gains one field, `out io.Writer`, set to `os.Stdout`
where the CLI surface constructs it, and every body writes there instead. That is
73 sites in `cmd/harness-cli/dispatch.go`, plus `isTTY` widened from `*os.File`
to `io.Writer` (it type-asserts and answers false for a buffer) — and it is the
shape the SSOT design wants: the CLI becomes one consumer that *chooses* a sink,
not the definition of where output goes. `DispatchCLIAction(cliVerbs{out: buf},
act)` then captures a result with no subprocess and no second implementation.

The field is read through a nil-safe accessor (`stdout()`), so a `cliVerbs` built
without one — a test — behaves as the CLI does rather than panicking.

**Residual.** A few bodies write with `fmt.Println`/`fmt.Printf` instead of the
sink (`CapsSetParent`, the runner and skill bodies). They are not declared MCP
tools, so no tool call reaches them; a verb declared later MUST write through the
sink or its output will leak into the JSON-RPC stream. The golden and the
stdout-purity test in V1 are what would catch that.

Rejected alternative: swapping the `os.Stdout` package variable around the call.
It reaches the var-reading bodies, but it is process-global — racy if two MCP
requests overlap — and it silently misses anything that writes fd 1 directly,
which is exactly the streaming verbs §6 already excludes. A field is explicit
and has no such edge.

A truly argv-free form — build `Bound` from the tool arguments and validate it
without `Parse` — is NOT taken. `Parse` is the only entry that runs the declared
checks and `Validate` (`check`, `cli/verb/build.go:202`); the `BuildFunc`
repackaging takes an already-validated `Bound` and does not. There is no
exported `Bound`-validation entry, and adding a second `Bound` constructor (the
WebUI bridge has one, in JS) is the copy the SSOT exists to prevent. Synthesizing
argv reuses the single parse path, and gate V3 pins it to the hand-typed argv.

The generated dispatch stays as it is: `DispatchCLIAction` already routes a
parsed `Action` to the CLI handler, so the MCP server needs no new generated
interface — only the field on the handler.

## 8. Verification gates

Each is measured before the dependent part ships and recorded with the command
that produced it.

| # | question | blocks |
|---|---|---|
| V1 | Does a real MCP client connect to `harness-cli mcp`, complete `initialize`, and list tools with no stray output on stdout? | the whole surface |
| V2 | Does a `tools/call` return the same bytes as the equivalent `harness-cli <verb>` run directly, for one read verb and one write verb? | D4 |
| V3 | Does schema → argv reconstruction reproduce the Action a hand-typed argv produces, including a `Variadic`, an `Optional`, a `Modes` group and a `Pathspec` verb? | §5.3 |
| V4 | Is `mcp` absent from `tools/list`, and is every streaming/interactive verb absent by declaration? | §6 |
| V5 | Under `--read-only`, is a `write` tool refused without running? | D12 |
| V6 | Does the same identity apply — is a tool call denied by the server exactly where the direct CLI call is denied (caps), with no bypass? | D6 |

### 8.1 Measured (2026-11-22, `main` at 768b1fce)

| # | result |
|---|---|
| V1 | **pass**: a real client connected. `pi mcp list` (no session, no model call) reported `harness: connected, 8 tools` and named all eight, with no stray output. The pipe probe also answered `initialize`, `tools/list`, `tools/call` and `ping` in 4 JSON-RPC lines with empty stderr. |
| V2 | **pass**, read verb: `tools/call version --json` returned bytes identical to `harness-cli version --json`. A write verb was not exercised end-to-end (it needs a live server); its Action is covered by V3. |
| V3 | **pass**: `TestMCPArgvRoundTrip` rebuilds argv from arguments for a flag-only, a positional and a write verb and compares the Action with the hand-typed argv. A `Variadic`/`Modes`/`Pathspec` tool is not declared yet, so those rows are untested. |
| V4 | **pass**: `TestMCPNeverExposesItself` and `TestMCPExcludesInteractiveVerbs`; the golden lists 8 tools. |
| V5 | **pass**: `TestMCPReadOnlyRefusesWriteTool` returns the refusal text without running the body. |
| V6 | **pass**: `record_ls` and `transcript_ls` were denied by the server with the same text as the typed command (`permission denied: RecordList requires capability exec_view` / `… TranscriptList requires capability transcript_read`), byte-for-byte, and came back as `isError`. The refusal is the server's; the tool layer neither widens nor hides it. |

## 9. Security notes

- **No new privilege and no new reach.** The MCP server is a child of the
  agent's MCP client, inside the same sandbox: same uid, same seccomp, same
  caps, no listener (stdio). It inherits the task's `HARNESS_*` and dials as the
  task, so the authority of a tool call is the authority of the agent that made
  it — the server's caps, unchanged (D6).
- **A typed surface is not a boundary.** Because Bash coexists, the same
  operations remain reachable by other means; the surface adds no confinement
  in v1. Its value is declaration (D3) and a place to attach per-tool policy
  later.
- **Result content is on the model channel.** A tool result is returned to the
  client and shown to the model, so anything a verb prints is published to the
  provider. `cli/ticket_guard.go` covers the board-publish path only; an MCP
  result containing the task's own ticket is not covered. Accepted residual: the
  ticket's authority equals the agent's, so nothing escalates (§ the sandbox
  audit), but a per-tool redaction hook at this layer is the natural place to
  close it and is left to the broker spec.
- **`--read-only` is a filter, not a capability.** It removes tools from
  `tools/list` and refuses them in `tools/call`; it does not change what the
  server authorizes. Authority remains the caps.

## 10. Testing

1. **Completeness (build property + test).** Every verb marked "should be a
   tool" carries `MCP`; `mcp` and every interactive/streaming verb does not; and
   every `MCP` tool name is unique after sanitization. In the style of
   `cmd/harness-cli/usage_completeness_test.go` and
   `cli/verb/invariant_test.go`.
2. **Schema golden.** `tools/list` for the whole table, diffed against a
   committed golden, so a table change that moves the tool surface is visible.
3. **Round-trip.** For each `MCP` verb, generate an `arguments` object from its
   schema, reconstruct argv, and assert the produced Action equals the Action
   from the canonical argv (`cli/verb`'s differential tests are the model).
4. **Stdio session.** Drive `harness-cli mcp` with an in-process JSON-RPC client
   on pipes (no network): `initialize`, `tools/list`, a `tools/call`, and a
   malformed frame; assert stdout is JSON-RPC only.
5. **Real client.** One call from a real agent (pi) with `mcp.json` pointing at
   `harness-cli mcp`, verifying the tool appears and one read verb answers.
6. **`make scripts-test`** stays green (the sandbox wrapper and presets are
   untouched by this change).

Implemented in `cmd/harness-cli/mcp_test.go`: 1
(`TestMCPToolNamesAreUnique`, `TestMCPExcludesInteractiveVerbs`), 2
(`TestMCPToolsGolden`, `testdata/mcp_tools_golden.json`, regenerate with
`-update-mcp`), 3 (`TestMCPArgvRoundTrip`), 4 (`TestMCPStdioSession`), and the
`--read-only` refusal (`TestMCPReadOnlyRefusesWriteTool`). Item 5 was not run:
the sandbox mounts no `mcp.json`, so a real client is a host-side step (§11).

## 11. Enablement (not this spec's to implement, recorded for the reader)

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

## 12. Open questions

- **D12** — should mutating verbs (`submit`, `cancel`, `file push/delete`,
  `agent send`) be tools by default, or only under an explicit `--read-only`
  inverse? The authority is the agent's either way; the question is accident
  surface, and it is the operator's.
- Should `tools/list` be filtered by the connection's caps (hide tools the
  server would deny)? Attractive for prompt economy and honesty; but it moves
  authority knowledge into the surface, which D9 and the SSOT deliberately keep
  out. Left open.
- Whether the `description` should be `UsageLines()` or the `Notes` prose; the
  former is stable and short, the latter is what an operator reads.
