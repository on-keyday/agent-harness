# Event-stream write verbs and the TUI chat screen — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an event-stream task drivable — send turns, answer approvals,
interrupt, finish — from the CLI and from a TUI chat screen entered with `r`.

**Architecture:** One place builds an adapter-protocol line (marshal + append
`\n`). Two writers use it: short-lived CLI verbs that open a cowrite attach per
call, and a long-lived `StreamSession` the TUI chat holds open for both
directions. The chat decodes the NDJSON itself rather than reading the task
log, because the log's rendering drops a request's `Input`.

**Tech Stack:** Go 1.x, bubbletea/bubbles (TUI), the existing
`runner/streamagent` adapter protocol, `objtrsf/exec` CommandExecutionStream.

**Spec:** `docs/superpowers/specs/2026-08-20-event-stream-agent-design.md` —
§3 (verbs, authority) and **Amendment 2026-08-21b** (this increment's scope).

## Global Constraints

- **Every write appends its own `\n`.** Measured: a line without it sits
  invisibly in the adapter's line buffer until the next newline flushes it.
  `session send` is the raw escape hatch and appends nothing.
- **One serializer per grammar** (surface-parity checklist 32). The CLI verbs
  and the TUI chat MUST both build their lines through `cli.EncodeStreamMsg`.
  A second hand-rolled `json.Marshal` of a `streamagent.Msg` is a defect.
- Writes are `exec_cowrite`; reads are `exec_view`. Cowrite is read AND write,
  so one attach serves the chat's both directions.
- **The verbs are 1:1 with the adapter's inbound kinds** — `turn`→`user`,
  `approve`→`response`, `interrupt`→`interrupt`, `finish`→`finish`. All four
  are already handled by `claudeAdapter.pumpNeutralIn`; nothing in the adapter
  changes except Task 1.
- **Do NOT raise `agentlog.maxFieldBytes` (200).** It is correct for the task
  log, whose payloads are unbounded. The chat is exempt by reading the stream.
- A deny `message` reaches the agent verbatim as a `tool_result` with
  `is_error` — operator-authored text entering a model's context.
- `--modify` (`Response.UpdatedInput`) is OUT of this increment.
- WebUI is the increment AFTER this one; do not add WebUI code here.
- Verify with `make check`, `make vet`, `make test` — never a bare
  `go build ./cmd/<x>` (it drops a binary in the worktree).

---

### Task 1: Adapter request-id nonce

The id is the staleness guard (§3): `approve <req-id>` must refuse an answer
aimed at a request that no longer exists. `claudeAdapter` mints `req-<n>` from
a per-process counter, so a resumed task restarts at `req-1` and a stale
`approve req-1` answers a DIFFERENT request. Fix inside the adapter — no wire
change.

**Files:**
- Modify: `runner/streamagent/claude.go:189-209` (struct), `:121-122`
  (construction), `:297-301` (mint)
- Test: `runner/streamagent/claude_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: request ids of the form `req-<8 hex>-<n>`. Task 2's `approve`
  helper passes them through opaquely; no consumer parses them.

- [ ] **Step 1: Write the failing test**

```go
func TestRequestIDsCarryAPerRunNonce(t *testing.T) {
	// Two adapters standing for a task and its resume. Ids must not collide,
	// or a stale `approve req-1` answers a different request (design §3).
	a := &claudeAdapter{nonce: newRunNonce()}
	b := &claudeAdapter{nonce: newRunNonce()}
	if a.nonce == b.nonce {
		t.Fatal("two runs minted the same nonce")
	}
	first := a.mintRequestID()
	second := a.mintRequestID()
	if first == second {
		t.Fatalf("ids repeat within one run: %q", first)
	}
	if !strings.HasPrefix(first, "req-"+a.nonce+"-") {
		t.Errorf("id %q does not carry the run nonce", first)
	}
	if b.mintRequestID() == first {
		t.Error("the second run's first id collides with the first run's")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/streamagent/ -run TestRequestIDsCarryAPerRunNonce`
Expected: FAIL — `undefined: newRunNonce`, `a.nonce undefined`.

- [ ] **Step 3: Write minimal implementation**

In `claude.go`, add to the `claudeAdapter` struct beside `seq`:

```go
	// nonce makes a request id unique across RUNS, not just within one. seq is
	// per-process, so a resumed task restarts at 1 and a stale `approve req-1`
	// would answer a different request — and the id is precisely the staleness
	// guard (design §3).
	nonce string
```

Add the helpers:

```go
// newRunNonce is the per-run half of a request id. Random rather than a
// timestamp: two adapters can start inside one clock tick.
func newRunNonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a reason to run without the guard; a
		// degenerate nonce still separates this run from a DIFFERENT process
		// only by luck, so make the failure visible in the id itself.
		return "norand"
	}
	return hex.EncodeToString(b[:])
}

// mintRequestID returns the next id for this run. Caller holds no lock; this
// takes a.mu itself.
func (a *claudeAdapter) mintRequestID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	return "req-" + a.nonce + "-" + strconv.Itoa(a.seq)
}
```

Add `"crypto/rand"` and `"encoding/hex"` to the imports.

At construction (`:121`), set the nonce:

```go
	a := &claudeAdapter{w: w, agentIn: stdin, agentInClose: closeAgentIn,
		nonce:   newRunNonce(),
		pending: map[string]string{}, interrupts: map[string]struct{}{}}
```

At the mint site (`:297-301`), replace the inline counter:

```go
	id := a.mintRequestID()
	a.mu.Lock()
	a.pending[id] = v.RequestID
	a.mu.Unlock()
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/streamagent/ -run TestRequestIDsCarryAPerRunNonce -v`
Expected: PASS.

- [ ] **Step 5: Run the package's whole suite**

Run: `go test ./runner/streamagent/ ./runner/`
Expected: PASS. The existing adapter tests assert behaviour, not literal ids;
if one asserts `req-1`, change it to assert the `req-` prefix and
`strings.Count(id, "-") == 2`, not to a fixed nonce.

- [ ] **Step 6: Commit**

```bash
git add runner/streamagent/claude.go runner/streamagent/claude_test.go
git commit -m "fix(streamagent): a request id must not repeat after a resume"
```

---

### Task 2: One line builder, and the four one-shot write helpers

**Files:**
- Create: `cli/stream_write.go` (untagged — `CommandExecutionStream` lives in
  objtrsf's untagged `exec_stream.go`, the same reason `cli/snapshot_raw.go`
  is untagged; the WebUI increment needs these under `GOOS=js`)
- Test: `cli/stream_write_test.go`

**Interfaces:**
- Consumes: `streamagent.Msg` and its payload types; `(*Client).AttachSession`.
- Produces, for Tasks 3–6:
  - `func EncodeStreamMsg(m streamagent.Msg) ([]byte, error)`
  - `func (c *Client) StreamTurn(ctx context.Context, taskIDHex, text string, flush time.Duration) error`
  - `func (c *Client) StreamApprove(ctx context.Context, taskIDHex string, r streamagent.Response, flush time.Duration) error`
  - `func (c *Client) StreamInterrupt(ctx context.Context, taskIDHex string, flush time.Duration) error`
  - `func (c *Client) StreamFinish(ctx context.Context, taskIDHex string, flush time.Duration) error`

- [ ] **Step 1: Write the failing test**

```go
func TestEncodeStreamMsgAppendsTheNewline(t *testing.T) {
	// The newline is not cosmetic: without it the line sits in the adapter's
	// line buffer, invisible, until some later write flushes it. Measured.
	got, err := EncodeStreamMsg(streamagent.Msg{
		V: streamagent.ProtocolVersion, Kind: streamagent.KindUser,
		User: &streamagent.UserTurn{Text: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, []byte("\n")) {
		t.Fatalf("no trailing newline: %q", got)
	}
	if bytes.Count(got, []byte("\n")) != 1 {
		t.Fatalf("want exactly one newline, got %q", got)
	}
	// It must round-trip through the adapter's own decoder, not merely be
	// JSON: the decoder is what the far side runs.
	back, err := streamagent.DecodeMsg(bytes.TrimSuffix(got, []byte("\n")))
	if err != nil {
		t.Fatalf("adapter cannot decode what we built: %v", err)
	}
	if back.Kind != streamagent.KindUser || back.User == nil || back.User.Text != "hello" {
		t.Fatalf("round trip changed the message: %+v", back)
	}
}

func TestEncodeStreamMsgRejectsEmbeddedNewline(t *testing.T) {
	// A turn holding a newline is ordinary (multi-line paste). JSON escapes it
	// as \n inside the string, so the framing survives — assert that rather
	// than assuming it.
	got, err := EncodeStreamMsg(streamagent.Msg{
		V: streamagent.ProtocolVersion, Kind: streamagent.KindUser,
		User: &streamagent.UserTurn{Text: "one\ntwo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(got, []byte("\n")) != 1 {
		t.Fatalf("a multi-line turn broke the framing: %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/ -run TestEncodeStreamMsg`
Expected: FAIL — `undefined: EncodeStreamMsg`.

- [ ] **Step 3: Write minimal implementation**

Create `cli/stream_write.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// EncodeStreamMsg marshals one adapter-protocol message and appends the
// newline that frames it.
//
// THE ONE builder for this grammar (surface-parity checklist 32): the CLI
// verbs and the TUI chat both go through it, so neither can drift into its own
// spelling of a message the adapter has to parse. The newline is the part that
// must not be re-derived — measured, a line without it sits in the adapter's
// line buffer until some later write flushes it, so the operator sees nothing
// happen and no error either.
func EncodeStreamMsg(m streamagent.Msg) ([]byte, error) {
	if m.V == 0 {
		m.V = streamagent.ProtocolVersion
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", m.Kind, err)
	}
	return append(b, '\n'), nil
}

// writeStreamMsg is the short-lived write: one cowrite attach, one line, drain,
// detach. It is what every `session stream <verb>` CLI call does. The TUI chat
// holds a StreamSession instead and writes on the attach it already has.
//
// The kind is checked here rather than in each verb: writing NDJSON at a PTY
// session would type JSON into somebody's shell.
func (c *Client) writeStreamMsg(ctx context.Context, taskIDHex string, m streamagent.Msg, flush time.Duration) error {
	line, err := EncodeStreamMsg(m)
	if err != nil {
		return err
	}
	stream, _, kind, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_Cowrite)
	if err != nil {
		return err
	}
	defer stream.Close()
	if kind != protocol.TaskKind_Stream {
		return fmt.Errorf("task %s is not an event-stream session (kind %s): `session stream %s` has no meaning there: %w",
			taskIDHex, kind, m.Kind, ErrAttachWrongKind)
	}
	if _, err := stream.Stdin().Write(line); err != nil {
		return err
	}
	// Same reason SessionSend drains: Close cancels the transport, so closing
	// immediately after the write can drop the in-flight frame.
	if flush > 0 {
		t := time.NewTimer(flush)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return nil
}

// StreamTurn appends one user turn.
func (c *Client) StreamTurn(ctx context.Context, taskIDHex, text string, flush time.Duration) error {
	return c.writeStreamMsg(ctx, taskIDHex, streamagent.Msg{
		Kind: streamagent.KindUser, User: &streamagent.UserTurn{Text: text},
	}, flush)
}

// StreamApprove answers a pending request. r.ID is the staleness guard: the
// adapter refuses an id it is not holding, so a stale answer is rejected rather
// than applied to whatever happens to be pending (design §3).
func (c *Client) StreamApprove(ctx context.Context, taskIDHex string, r streamagent.Response, flush time.Duration) error {
	if r.ID == "" {
		return fmt.Errorf("approve: a request id is required — it is what makes a stale answer a refusal rather than a misapplied one")
	}
	return c.writeStreamMsg(ctx, taskIDHex, streamagent.Msg{
		Kind: streamagent.KindResponse, Response: &r,
	}, flush)
}

// StreamInterrupt abandons the running turn. The agent survives and takes the
// next turn; this is not a kill.
func (c *Client) StreamInterrupt(ctx context.Context, taskIDHex string, flush time.Duration) error {
	return c.writeStreamMsg(ctx, taskIDHex, streamagent.Msg{
		Kind: streamagent.KindInterrupt, Interrupt: &streamagent.Interrupt{},
	}, flush)
}

// StreamFinish ends the session cleanly: the adapter closes the agent's stdin,
// so it completes the turn in flight and exits 0.
func (c *Client) StreamFinish(ctx context.Context, taskIDHex string, flush time.Duration) error {
	return c.writeStreamMsg(ctx, taskIDHex, streamagent.Msg{
		Kind: streamagent.KindFinish, Finish: &streamagent.Finish{},
	}, flush)
}
```

`streamagent.Interrupt` and `streamagent.Finish` each carry one optional
`Reason string` — "for the harness's own log; the agent is told nothing", per
their doc comments. `&streamagent.Interrupt{}` is therefore correct as written;
leave `Reason` empty rather than inventing one, since nothing reads it yet.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/ -run TestEncodeStreamMsg -v`
Expected: PASS (both).

- [ ] **Step 5: Add the wrong-kind test**

```go
func TestStreamApproveRequiresARequestID(t *testing.T) {
	var c Client
	err := c.StreamApprove(context.Background(), "deadbeef", streamagent.Response{
		Behavior: streamagent.BehaviorAllow,
	}, 0)
	if err == nil {
		t.Fatal("an approve with no id must be refused before it reaches the wire")
	}
	if !strings.Contains(err.Error(), "request id") {
		t.Errorf("error should say what is missing, got %q", err)
	}
}
```

Run: `go test ./cli/ -run TestStreamApprove -v`
Expected: PASS — it returns before touching the nil transport.

- [ ] **Step 6: Commit**

```bash
git add cli/stream_write.go cli/stream_write_test.go
git commit -m "feat(cli): one builder for adapter-protocol lines, and the four write helpers"
```

---

### Task 3: The `session stream` CLI verbs

**Files:**
- Modify: `cmd/harness-cli/session.go:109-135` (the `runSessionStream`
  dispatch and its usage line)

**Interfaces:**
- Consumes: Task 2's four `(*Client).Stream*` helpers.
- Produces: the CLI surface. Task 6's TUI cmdline mirrors these flag names.

- [ ] **Step 1: Replace the "not built yet" arms**

In `runSessionStream`, the `case "turn", "approve", ...` arm currently returns
"specified but not built yet". Replace with per-verb runners and leave
`requests` / `snapshot` on the not-built arm:

```go
	case "turn":
		return runSessionStreamTurn(cid, rest)
	case "approve":
		return runSessionStreamApprove(cid, rest)
	case "interrupt":
		return runSessionStreamSimple(cid, rest, "interrupt")
	case "finish":
		return runSessionStreamSimple(cid, rest, "finish")
	case "requests", "snapshot":
		return fmt.Errorf("session stream %s: specified (design §3) but not built yet; "+
			"`session snapshot --raw` reads the stream verbatim in the meantime", verb)
```

Update the usage line to list what exists:

```go
		fmt.Fprintln(os.Stderr, "usage: harness-cli session stream <attach|turn|approve|interrupt|finish> <id> ...  (requests/snapshot: specified, not built yet)")
```

- [ ] **Step 2: Write the verbs**

```go
// runSessionStreamTurn sends one user turn. The text is the joined positional
// args, matching `session send`'s shape so an operator does not have to quote
// differently between the two.
func runSessionStreamTurn(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session stream turn", flag.ExitOnError)
	flushMs := fs.Uint("flush-ms", 400, "ms to let the line drain to the runner before detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: session stream turn <id> <text...>")
	}
	taskIDHex := rest[0]
	text := strings.Join(rest[1:], " ")
	c, err := Dial(cid)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.StreamTurn(ctx, taskIDHex, text, time.Duration(*flushMs)*time.Millisecond)
}

// runSessionStreamApprove answers one pending request. --deny takes the reason
// as the remaining positional args: it reaches the AGENT verbatim as a failed
// tool result, so it is operator-authored text entering a model's context, not
// a private audit note.
func runSessionStreamApprove(cid objproto.ConnectionID, args []string) error {
	fs := flag.NewFlagSet("session stream approve", flag.ExitOnError)
	allow := fs.Bool("allow", false, "run the tool as requested")
	deny := fs.Bool("deny", false, "refuse it; the remaining args are the reason, which the agent reads")
	suggestion := fs.Int("suggestion", -1, "accept the request's Nth suggestion (0-based) instead of answering only this call")
	flushMs := fs.Uint("flush-ms", 400, "ms to let the line drain to the runner before detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: session stream approve <id> <request-id> --allow | --deny [reason...]")
	}
	if *allow == *deny {
		return fmt.Errorf("session stream approve: give exactly one of --allow or --deny")
	}
	resp := streamagent.Response{ID: rest[1]}
	if *allow {
		resp.Behavior = streamagent.BehaviorAllow
	} else {
		resp.Behavior = streamagent.BehaviorDeny
		resp.Message = strings.Join(rest[2:], " ")
	}
	if *suggestion >= 0 {
		s := *suggestion
		resp.AcceptSuggestion = &s
	}
	c, err := Dial(cid)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.StreamApprove(ctx, rest[0], resp, time.Duration(*flushMs)*time.Millisecond)
}

// runSessionStreamSimple serves the two verbs that carry no payload.
func runSessionStreamSimple(cid objproto.ConnectionID, args []string, verb string) error {
	fs := flag.NewFlagSet("session stream "+verb, flag.ExitOnError)
	flushMs := fs.Uint("flush-ms", 400, "ms to let the line drain to the runner before detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: session stream %s <id>", verb)
	}
	c, err := Dial(cid)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	flush := time.Duration(*flushMs) * time.Millisecond
	if verb == "interrupt" {
		return c.StreamInterrupt(ctx, rest[0], flush)
	}
	return c.StreamFinish(ctx, rest[0], flush)
}
```

**Two sibling patterns from `runSessionStreamAttach` (`session.go:139`) that
the sketch above gets wrong — copy these instead:**

1. **Dial**: it is `cli.Dial(ctx, cid, protocol.ClientKind_Cli)` with a
   `ctx := context.Background()`, not a bare `Dial(cid)`.
2. **Flag order**: this file parses with `pos, err := parsePermuted(fs, args)`,
   NOT `fs.Parse` + `fs.Args()`. `parsePermuted` tolerates flags appearing
   after positionals, which is deliberate on these verbs — plain `fs.Parse`
   stops at the first positional, so `session stream approve <id> <req>
   --allow` would silently drop `--allow`. Every verb below must use it.

So each verb's body is:

```go
	pos, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	// … arity check on pos …
	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.StreamTurn(ctx, pos[0], strings.Join(pos[1:], " "), flush)
```

with `pos` replacing `rest` throughout.

- [ ] **Step 3: Build and eyeball the usage text**

Run: `make check`
Expected: builds clean.

Run: `./bin/harness-cli session stream` — no, do NOT run a bare build binary
from the worktree. Instead: `go run ./cmd/harness-cli session stream 2>&1 | head -3`
Expected: the new usage line naming attach/turn/approve/interrupt/finish.

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-cli/session.go
git commit -m "feat(cli): session stream turn / approve / interrupt / finish"
```

---

### Task 4: `StreamSession` — the long-lived attach the chat holds

**Files:**
- Modify: `cli/stream_write.go` (append; keep it one file — these are the same
  responsibility, writing to a stream task)
- Test: `cli/stream_write_test.go`

**Interfaces:**
- Consumes: Task 2's `EncodeStreamMsg`; `(*Client).AttachSession`.
- Produces, for Task 5:
  - `type StreamLine struct { Msg streamagent.Msg; Raw []byte; Decoded bool }`
  - `func (c *Client) OpenStreamSession(ctx context.Context, taskIDHex string) (*StreamSession, error)`
  - `func (s *StreamSession) ReadLine() (StreamLine, error)`
  - `func (s *StreamSession) Send(m streamagent.Msg) error`
  - `func (s *StreamSession) Close() error`

- [ ] **Step 1: Write the failing test**

```go
func TestStreamLineMarksAnUndecodableLine(t *testing.T) {
	// `session send` can lawfully put a non-protocol line on this stream. A
	// follower that DROPS it cannot explain what the adapter does next, so the
	// line survives with Decoded=false rather than becoming an error.
	line, err := decodeStreamLine([]byte(`hello, not json`))
	if err != nil {
		t.Fatalf("a non-protocol line must not be an error: %v", err)
	}
	if line.Decoded {
		t.Error("Decoded should be false")
	}
	if string(line.Raw) != "hello, not json" {
		t.Errorf("Raw = %q, want the original bytes", line.Raw)
	}

	ok, err := decodeStreamLine([]byte(`{"v":1,"kind":"event","event":{"kind":"text","text":"hi"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ok.Decoded || ok.Msg.Kind != streamagent.KindEvent {
		t.Fatalf("a protocol line must decode: %+v", ok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/ -run TestStreamLineMarks`
Expected: FAIL — `undefined: decodeStreamLine`.

- [ ] **Step 3: Write minimal implementation**

Append to `cli/stream_write.go`:

```go
// StreamLine is one line read off an event-stream task. Decoded=false with Raw
// set is a line that is not the protocol — `session send` can put one there —
// and it is carried rather than dropped: a follower who cannot see what a
// cowriter injected cannot explain what the adapter does next.
type StreamLine struct {
	Msg     streamagent.Msg
	Raw     []byte
	Decoded bool
}

func decodeStreamLine(line []byte) (StreamLine, error) {
	out := StreamLine{Raw: append([]byte(nil), line...)}
	m, err := streamagent.DecodeMsg(line)
	if err != nil {
		return out, nil
	}
	out.Msg, out.Decoded = m, true
	return out, nil
}

// StreamSession is a live COWRITE attach to an event-stream task: it reads the
// adapter's messages and writes the client's, on one attach. Cowrite takes no
// writer seat, so several may coexist and the task stays Detached — which for
// this kind is the ordinary state.
//
// The short-lived Stream* helpers above open one of these per call. A surface
// that both follows and drives (the TUI chat) holds ONE instead: reattaching
// per keystroke would re-replay the ring every time.
type StreamSession struct {
	stream *agentexec.CommandExecutionStream
	rd     *bufio.Reader
	mu     sync.Mutex // serialises writes; ReadLine runs on its own goroutine
}

// OpenStreamSession attaches and refuses a non-stream task before the caller
// can put NDJSON into somebody's shell.
func (c *Client) OpenStreamSession(ctx context.Context, taskIDHex string) (*StreamSession, error) {
	stream, _, kind, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_Cowrite)
	if err != nil {
		return nil, err
	}
	if kind != protocol.TaskKind_Stream {
		_ = stream.Close()
		return nil, fmt.Errorf("task %s is not an event-stream session (kind %s): %w",
			taskIDHex, kind, ErrAttachWrongKind)
	}
	return &StreamSession{stream: stream, rd: bufio.NewReader(stream.Stdout())}, nil
}

// ReadLine returns the next line. It blocks; run it on its own goroutine.
func (s *StreamSession) ReadLine() (StreamLine, error) {
	line, err := s.rd.ReadBytes('\n')
	if len(line) > 0 {
		out, _ := decodeStreamLine(bytes.TrimRight(line, "\r\n"))
		return out, err
	}
	return StreamLine{}, err
}

// Send writes one message. Safe from any goroutine.
func (s *StreamSession) Send(m streamagent.Msg) error {
	b, err := EncodeStreamMsg(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.stream.Stdin().Write(b)
	return err
}

func (s *StreamSession) Close() error { return s.stream.Close() }
```

Add `bufio`, `bytes`, `sync` and the `agentexec` import
(`agentexec "github.com/on-keyday/objtrsf/exec"`) — match how
`cli/snapshot_raw.go` spells that import.

**Note on `AttachSession`'s return type:** `snapshot_raw.go` calls
`attachSessionRPC` and wraps it with `agentexec.NewCommandExecutionStream`
because the js and native builds declare `AttachSession` differently. If
`cli/stream_write.go` must stay untagged, follow `snapshot_raw.go`'s route
(`attachSessionRPC` + `NewCommandExecutionStream`) rather than
`AttachSession`. Read that file before writing this one.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/ -run TestStreamLine -v`
Expected: PASS.

- [ ] **Step 5: Verify the wasm build still compiles**

Run: `make wasm-check`
Expected: clean. If it fails on `AttachSession`, take the `attachSessionRPC`
route from the note above.

- [ ] **Step 6: Commit**

```bash
git add cli/stream_write.go cli/stream_write_test.go
git commit -m "feat(cli): StreamSession, one cowrite attach carrying both directions"
```

---

### Task 5: The TUI chat model

A full-screen overlay beside `GridModel`. Pure model + view here; Task 6 wires
it into the app.

**Files:**
- Create: `tui/chat.go`
- Test: `tui/chat_test.go`

**Interfaces:**
- Consumes: Task 4's `StreamSession` / `StreamLine`; `streamagent` types.
- Produces, for Task 6:
  - `type ChatModel struct{ … }`, `func NewChatModel() ChatModel`
  - `func (m ChatModel) IsOpen() bool`
  - `func (m *ChatModel) SetSize(w, h int)`
  - `func (m *ChatModel) Open(ctx context.Context, c *cli.Client, program *tea.Program, taskID string)`
  - `func (m *ChatModel) Close()`
  - `func (m ChatModel) Update(msg tea.Msg) (ChatModel, tea.Cmd)`
  - `func (m ChatModel) View() string`
  - `type ChatLineMsg struct{ TaskID string; Line cli.StreamLine }`
  - `type ChatEndedMsg struct{ TaskID string; Err error }`

- [ ] **Step 1: Write the failing test**

```go
func TestChatFoldsEventsIntoTheTranscript(t *testing.T) {
	m := NewChatModel()
	m.taskID = "abc"
	m.open = true

	m.applyLine(streamLineOf(t, `{"v":1,"kind":"event","event":{"kind":"tool_start","tool":"Bash","args":"ls"}}`))
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"event","event":{"kind":"text","text":"done"}}`))

	body := strings.Join(m.lines, "\n")
	if !strings.Contains(body, "Bash") {
		t.Errorf("tool line missing from transcript: %q", body)
	}
	if !strings.Contains(body, "done") {
		t.Errorf("answer missing from transcript: %q", body)
	}
}

func TestChatHoldsAPendingRequestWithItsInput(t *testing.T) {
	// The whole reason the chat reads the STREAM: the log's rendering drops
	// Input, and Input is what the operator decides on.
	m := NewChatModel()
	m.taskID = "abc"
	m.open = true
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Write","input":{"file_path":"/tmp/x","content":"hi"}}}`))

	if m.pending == nil {
		t.Fatal("a request must become pending state, not just a transcript line")
	}
	if m.pending.ID != "req-ab12-1" {
		t.Errorf("pending id = %q", m.pending.ID)
	}
	if !strings.Contains(string(m.pending.Input), "file_path") {
		t.Errorf("Input was not retained: %q", m.pending.Input)
	}
	if !strings.Contains(m.View(), "/tmp/x") {
		t.Error("the approval view must show what is being written")
	}
}

func TestChatAllowBuildsTheResponseAndClearsPending(t *testing.T) {
	m := NewChatModel()
	m.taskID = "abc"
	m.open = true
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Write"}}`))

	sent := m.buildApproval(streamagent.BehaviorAllow, "")
	if sent == nil || sent.Response == nil {
		t.Fatal("allow must build a response message")
	}
	if sent.Response.ID != "req-ab12-1" || sent.Response.Behavior != streamagent.BehaviorAllow {
		t.Fatalf("wrong response: %+v", sent.Response)
	}
	if m.pending != nil {
		t.Error("pending must clear once answered")
	}
}

func TestChatDenyCarriesTheReasonToTheAgent(t *testing.T) {
	m := NewChatModel()
	m.taskID = "abc"
	m.open = true
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Bash"}}`))

	sent := m.buildApproval(streamagent.BehaviorDeny, "use the Makefile instead")
	if sent.Response.Message != "use the Makefile instead" {
		t.Errorf("deny reason lost: %+v", sent.Response)
	}
}

func TestChatRestoresThePromptAfterTheDenyEditor(t *testing.T) {
	// kscale's recorded trap: a sub-mode that replaces the input and does not
	// restore it leaves the wrong prompt stuck forever.
	m := NewChatModel()
	m.taskID = "abc"
	m.open = true
	m.applyLine(streamLineOf(t, `{"v":1,"kind":"request","request":{"id":"req-ab12-1","tool":"Bash"}}`))
	m.enterDenyReason()
	if m.mode != chatModeDenyReason {
		t.Fatal("deny should switch modes")
	}
	m.cancelSubMode()
	if m.mode != chatModeNormal {
		t.Error("esc must return to the normal mode")
	}
	if !strings.Contains(m.input.Prompt, "you") {
		t.Errorf("prompt not restored: %q", m.input.Prompt)
	}
}

func streamLineOf(t *testing.T, s string) cli.StreamLine {
	t.Helper()
	m, err := streamagent.DecodeMsg([]byte(s))
	if err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	return cli.StreamLine{Msg: m, Raw: []byte(s), Decoded: true}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tui/ -run TestChat`
Expected: FAIL — `undefined: NewChatModel`.

- [ ] **Step 3: Write `tui/chat.go`**

Structure (write it out; the tests above pin the names):

```go
package tui

// ChatModel is the event-stream kind's driving surface: a conversation view
// over one task's stream.
//
// It reads the STREAM rather than the task log, and that is forced rather than
// stylistic. streamagent.RenderText renders a request as "⏸ approval needed:
// <tool> (<id>)" and drops Input entirely, and agentlog.Render truncates tool
// args and results at 200 bytes — correct for a progress feed, useless for
// deciding whether to allow a Write. Input rides the stream verbatim, so this
// holds its own cowrite attach (cli.StreamSession) and decodes NDJSON.
//
// Shape borrowed from kscale's cmd/katui/chat.go, which solved it first: a
// transcript ring, activity lines rendered muted against a primary answer
// line, an elapsed-seconds ticker so a minutes-long turn visibly advances, and
// an approval that freezes the transcript and offers its choices as keys.

const chatLineLimit = 400 // transcript ring; bounded memory, plenty of scrollback

type chatMode int

const (
	chatModeNormal chatMode = iota
	chatModeDenyReason
)

type ChatModel struct {
	open   bool
	taskID string
	width  int
	height int

	sess   *cli.StreamSession
	cancel context.CancelFunc

	input textinput.Model
	mode  chatMode

	lines   []string
	scroll  int
	pending *streamagent.Request
	busy    bool
	elapsed int
	status  string
}
```

Behaviour each method owns:

- `Open` — store the task id, build the prompt, then start a goroutine that
  calls `OpenStreamSession` and pumps `ReadLine()` into `program.Send(ChatLineMsg{…})`
  until it errors, then sends `ChatEndedMsg`. This is the TUI's established
  async pattern (`tui/events.go` `subscribeAndStream` uses `program.Send` from
  a goroutine, NOT a channel the Update loop re-arms — follow the sibling).
- `applyLine(cli.StreamLine)` — the fold. `!Decoded` → one `(not the protocol)`
  line. `KindEvent` → `streamagent.RenderText` (the SAME renderer as the log,
  so the two cannot drift; only the REQUEST case differs). `KindRequest` → set
  `m.pending` AND append a marker line. `KindExit` → an ended line, clear busy.
- `buildApproval(b streamagent.Behavior, message string) *streamagent.Msg` —
  builds from `m.pending`, clears it, returns nil when nothing is pending.
- `enterDenyReason` / `cancelSubMode` — mode switches that ALWAYS rebuild the
  input; every exit path goes through `restoreInput()`.
- `Update` — `tea.KeyMsg`: in normal mode with a pending request, `a` allows,
  `d` enters the deny reason, digits `1..9` accept that suggestion; with no
  pending request, enter sends a turn; `esc` interrupts a running turn; `ctrl+d`
  finishes; `q` closes the overlay WITHOUT ending the task. `ChatLineMsg` →
  `applyLine`. `ChatEndedMsg` → status. A tick advances `elapsed` while busy.
- `View` — title with the short task id, transcript, an approval block when
  `m.pending != nil` showing tool + pretty-printed Input + the key row, then
  the status line and the input.

- [ ] **Step 4: Run the tests**

Run: `go test ./tui/ -run TestChat -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add tui/chat.go tui/chat_test.go
git commit -m "feat(tui): a chat model for the event-stream kind"
```

---

### Task 6: Wire the chat into the app, and `r`

**Files:**
- Modify: `tui/app.go` (5 sites — the same set `a.grid` occupies: the two
  `Update` guards at `:352` and `:1160`, `SetSize` at `:1087`, the `View`
  branch at `:2286`, plus the `r` dispatch at `:1928`)
- Modify: `tui/taskaction.go` (a live stream task returns a new
  `actionChat` instead of the hint added in `eab8422`)
- Modify: `tui/taskaction_test.go` (that hint test becomes an `actionChat` test)
- Modify: `tui/keys.go` (the `r`/`R` binding's Long text)
- Modify: `tui/cmdline.go:755-772` (the stream sub-verbs stop answering
  "not built yet")

**Interfaces:**
- Consumes: Task 5's `ChatModel`, Task 2's `Stream*` helpers.
- Produces: nothing further.

- [ ] **Step 1: Turn the hint into an action**

In `tui/taskaction.go`, add `actionChat` to the `taskActionKind` const block,
and replace the live-stream case:

```go
	case t.Kind == protocol.TaskKind_Stream && taskSessionAlive(t.Status):
		// No terminal to take over, but it IS drivable: the chat screen reads
		// the stream and can send turns and answer approvals.
		return taskAction{Kind: actionChat}
```

Update `TestResumeReattachAction`'s stream case to assert
`got.Kind == actionChat` instead of the `logs pane` hint.

- [ ] **Step 2: Run it, watch it fail, then wire the dispatch**

Run: `go test ./tui/ -run TestResumeReattachAction`
Expected: FAIL until `actionChat` exists; then PASS.

In `app.go`'s `r` block, add the arm:

```go
			case actionChat:
				if unpinnedResume {
					a.cmdresult.Append(WarnStyle.Render("u/U: pick a finished task to resume without assigned runner"))
					return a, nil
				}
				a.chat.Open(a.appCtx, a.client, a.program, a.tasks.SelectedID())
				a.chat.SetSize(a.width, a.height)
				return a, chatTickCmd()
```

- [ ] **Step 3: Add the four overlay sites**

Mirror `a.grid` exactly — read each `a.grid` site in `app.go` and add the
`a.chat` equivalent beside it. Order matters in `View`: put the chat branch
next to the grid branch, both before the final `return view`.

- [ ] **Step 4: Wire the TUI cmdline verbs**

In `tui/cmdline.go`, the `case "turn", "approve", "interrupt", "finish", ...`
arm currently errors for all six. Split it so the four built verbs parse into
actions and only `requests`/`snapshot` keep the not-built error. Add the
matching `runAction` arms in `app.go` calling Task 2's helpers via the
existing `Do*` command pattern (grep how `DoSessionKill` is written and follow
it — do NOT dial a fresh client; thread `a.client`).

- [ ] **Step 5: Update the key help**

In `tui/keys.go`, the `r`/`R` binding's `Long` currently describes reattach and
resume only. Extend it to name the chat: `keys_test` enforces that every
dispatched key has a row, so the help cannot silently omit it.

- [ ] **Step 6: Verify**

Run: `make check && make vet && make test`
Expected: all clean.

- [ ] **Step 7: Commit**

```bash
git add tui/
git commit -m "feat(tui): r opens the chat screen on a live event-stream task"
```

---

### Task 7: Live verification, docs, and the parity record

**Files:**
- Modify: `README.md` (the 5b block — the verbs exist now)
- Modify: `docs/superpowers/plans/2026-08-20-event-stream-wiring-log.md`
  (append entry 23)
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md`

- [ ] **Step 1: Drive it live**

Spawn a stream session on a preset-launched runner and make it want a tool, so
the approval path is exercised rather than assumed:

```bash
harness-cli session new --stream -d --agent claude --repo <a runner root>
harness-cli session stream turn <id> 'create a file /tmp/probe.txt containing OK'
harness-cli session snapshot --raw <id>     # see the request and its Input
harness-cli session stream approve <id> <req-id> --allow
harness-cli logs <id>
harness-cli session kill <id>
```

Expected: the request carries `req-<nonce>-<n>`; the approve is accepted; the
file is written. Then repeat with `--deny "no, use /tmp/other.txt"` and confirm
the reason reaches the agent (it appears in the transcript as a failed tool
result the model reacts to).

- [ ] **Step 2: Drive the TUI**

Open the TUI, select the live stream task, press `r`, send a turn, answer an
approval with `a`, and `esc`-interrupt a long turn. Per
`feedback_verify_interactive_input_not_just_render`, feed real keystrokes —
rendering is not evidence that input works.

- [ ] **Step 3: Update README 5b**

Replace "The remaining `session stream` verbs (turn/approve/interrupt/finish/
requests/snapshot) are specified in the event-stream design spec and not built
yet" with what is now true: the four write verbs exist, `requests`/`snapshot`
do not, and the TUI drives one with `r`.

- [ ] **Step 4: Wiring-log entry 23**

Record what resisted, in that log's register: what the plan got wrong, what was
measured, and what the next increment inherits (`requests`, `snapshot`,
`--modify`, WebUI, `pending=N`).

- [ ] **Step 5: Firing-log entry**

Walk the surface-parity checklist for the new option surface (items 1–10 for
the verbs, 24/28a for the two write paths, 29–33 for the result wording, 35–37
for docs) and record `done`/`omitted` per number. WebUI is `omitted —
deferred to the next increment at the operator's direction`, which is NOT the
same verdict the `87c10a2` walk recorded.

- [ ] **Step 6: Land**

```bash
make check && make vet && make test
git push origin HEAD:main          # FF only; see landing-to-main
```

Then `make build` in the main checkout, and restart the fleet only if a runner
path changed (Task 1 changes `runner/streamagent`, so it does — the adapter is
re-read per task, but `bin/harness-stream-adapter` must be rebuilt).
