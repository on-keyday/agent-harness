# Agent Comms P3: `harness agent` CLI Subcommand Group Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** claude が Bash tool 経由で呼び出す `harness agent send/wait/inbox/subscribe/unsubscribe/dispatch` subcommand 群を実装する。env primary、flag override、`HARNESS_AUTH_TICKET` のみ env-only。cursor file (`~/.cache/harness/agent-cursor-<task_id>`) で `--since-last` を支える。

**Architecture:** 共通 helper `cli/cliopts/` で env/flag の resolve を集約し、新パッケージ `cli/agent/` に subcommand 実装を分離する。各 subcommand は `peer.Dial` → `AgentBridgeHello` → 個別 request → response → exit という短命 connection。output は JSON Lines で stdout。

**Tech Stack:** Go 1.25.7、`encoding/hex`、`encoding/json`、既存 `objproto`/`peer`/`transport`/`cli`、P1 で導入された `agentboard` パッケージ wire schema。Spec: `docs/superpowers/specs/2026-04-28-agent-comms-design.md` §9, §10.5。

---

## Reference for implementers

### 前提

- P1 完了 (`agentboard.bgn` schema、wire payload kind、server agentboard handler)
- P2 完了 (runner が env と settings.json を正しく注入)

### env / flag resolution rules

- env name と同名 flag (`--server-cid` 等) があれば flag が優先
- `HARNESS_AUTH_TICKET` のみ env-only、flag 不可
- どちらも未指定なら起動エラー (exit code 2)

### output convention

- 成功時: JSON Lines を stdout (1 行 = 1 message or 1 status)
- エラー: stderr に短い 1 行、exit code 1
- usage error: stderr usage、exit code 2

### cursor file

- path: `$XDG_CACHE_HOME/harness/agent-cursor-<task_id>` または fallback `$HOME/.cache/harness/...`
- 内容: 単一 u64 (decimal text) — 最後に inbox/wait で得た next_cursor
- `--since-last` で読み込み、subcommand 終了直前に新 cursor を書き出し

### deferred features (out of scope for v1)

- glob pattern (server 側も exact match のみ実装した)
- `Deliver` push の active path (Wait の long-poll で代用)
- `dispatch` subcommand の reply correlation 自動化 (v1 では topic suffix で人間が分ける)

---

## File structure

### Create

```
cli/cliopts/cliopts.go          # resolveServerCID/AuthTicket/AgentIdentity helpers
cli/cliopts/cliopts_test.go

cli/agent/conn.go               # Dial+Hello helper, ConnectAgent()
cli/agent/conn_test.go
cli/agent/cursor.go             # cursor file read/write
cli/agent/cursor_test.go
cli/agent/send.go               # Send subcommand
cli/agent/wait.go               # Wait subcommand
cli/agent/inbox.go              # Inbox subcommand
cli/agent/subscribe.go          # Subscribe / Unsubscribe subcommands
cli/agent/dispatch.go           # Dispatch (= Send + Wait sugar)
cli/agent/agent_test.go         # subcommand integration tests
```

### Modify

```
cmd/harness-cli/main.go         # register `agent` subcommand group, dispatch to cli/agent.*
```

---

## Tasks

### Task 1: `cliopts` common helpers

**Files:**
- Create: `cli/cliopts/cliopts.go`, `cli/cliopts/cliopts_test.go`

- [ ] **Step 1: Write failing test**

```go
package cliopts

import (
    "encoding/hex"
    "os"
    "strings"
    "testing"
)

func TestResolveServerCID_FlagWinsOverEnv(t *testing.T) {
    t.Setenv("HARNESS_SERVER_CID", "ws:127.0.0.1:1-1")
    cid, err := ResolveServerCID("ws:127.0.0.1:2-2")
    if err != nil {
        t.Fatal(err)
    }
    if !strings.HasSuffix(cid.String(), "-2") {
        t.Errorf("flag should win, got %s", cid.String())
    }
}

func TestResolveServerCID_FallsBackToEnv(t *testing.T) {
    t.Setenv("HARNESS_SERVER_CID", "ws:127.0.0.1:1-3")
    cid, err := ResolveServerCID("")
    if err != nil {
        t.Fatal(err)
    }
    if !strings.HasSuffix(cid.String(), "-3") {
        t.Errorf("env should be used when flag empty, got %s", cid.String())
    }
}

func TestResolveServerCID_ErrorWhenMissing(t *testing.T) {
    os.Unsetenv("HARNESS_SERVER_CID")
    if _, err := ResolveServerCID(""); err == nil {
        t.Error("expected error when both flag and env empty")
    }
}

func TestResolveAuthTicket_EnvOnly(t *testing.T) {
    var want [16]byte
    for i := range want {
        want[i] = byte(i)
    }
    t.Setenv("HARNESS_AUTH_TICKET", hex.EncodeToString(want[:]))
    got, err := ResolveAuthTicket()
    if err != nil {
        t.Fatal(err)
    }
    if got != want {
        t.Errorf("ticket = %x, want %x", got, want)
    }
}

func TestResolveAuthTicket_RejectsBadHex(t *testing.T) {
    t.Setenv("HARNESS_AUTH_TICKET", "not-hex")
    if _, err := ResolveAuthTicket(); err == nil {
        t.Error("expected error on invalid hex")
    }
}
```

- [ ] **Step 2: Run test (fails)**

```bash
go test ./cli/cliopts/ -v
```

Expected: build error.

- [ ] **Step 3: Implement `cli/cliopts/cliopts.go`**

```go
package cliopts

import (
    "encoding/hex"
    "errors"
    "fmt"
    "os"

    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/runner/protocol"
)

// ResolveServerCID returns the ConnectionID from the flag value or HARNESS_SERVER_CID.
// Flag wins over env. Returns error if both are empty.
func ResolveServerCID(flagVal string) (objproto.ConnectionID, error) {
    raw := flagVal
    if raw == "" {
        raw = os.Getenv("HARNESS_SERVER_CID")
    }
    if raw == "" {
        return objproto.ConnectionID{}, errors.New("--server-cid required (or set HARNESS_SERVER_CID)")
    }
    return objproto.ParseConnectionID(raw, objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
}

// ResolveAuthTicket reads HARNESS_AUTH_TICKET only (no flag fallback).
// Returns error if env is unset or not 32 hex chars.
func ResolveAuthTicket() ([16]byte, error) {
    var t [16]byte
    raw := os.Getenv("HARNESS_AUTH_TICKET")
    if raw == "" {
        return t, errors.New("HARNESS_AUTH_TICKET env required (no flag accepted)")
    }
    b, err := hex.DecodeString(raw)
    if err != nil {
        return t, fmt.Errorf("HARNESS_AUTH_TICKET: %w", err)
    }
    if len(b) != 16 {
        return t, fmt.Errorf("HARNESS_AUTH_TICKET: expected 16 bytes, got %d", len(b))
    }
    copy(t[:], b)
    return t, nil
}

// ResolveTaskID reads from flag or HARNESS_TASK_ID env (32 hex chars).
func ResolveTaskID(flagVal string) (protocol.TaskID, error) {
    var t protocol.TaskID
    raw := flagVal
    if raw == "" {
        raw = os.Getenv("HARNESS_TASK_ID")
    }
    if raw == "" {
        return t, errors.New("--task-id required (or set HARNESS_TASK_ID)")
    }
    b, err := hex.DecodeString(raw)
    if err != nil {
        return t, fmt.Errorf("task-id: %w", err)
    }
    if len(b) != 16 {
        return t, fmt.Errorf("task-id: expected 16 bytes, got %d", len(b))
    }
    copy(t.Id[:], b)
    return t, nil
}

// ResolveRunnerID reads from flag or HARNESS_RUNNER_ID env, parses as ConnectionID, then converts to RunnerID.
func ResolveRunnerID(flagVal string) (protocol.RunnerID, error) {
    var rid protocol.RunnerID
    raw := flagVal
    if raw == "" {
        raw = os.Getenv("HARNESS_RUNNER_ID")
    }
    if raw == "" {
        return rid, errors.New("--runner-id required (or set HARNESS_RUNNER_ID)")
    }
    cid, err := objproto.ParseConnectionID(raw, objproto.ParseOption_ResolveAddr)
    if err != nil {
        return rid, fmt.Errorf("runner-id: %w", err)
    }
    rid.Transport = []byte(cid.Transport)
    if cid.Addr.Addr().Is4() {
        ip4 := cid.Addr.Addr().As4()
        rid.IpAddr = ip4[:]
    } else {
        ip16 := cid.Addr.Addr().As16()
        rid.IpAddr = ip16[:]
    }
    rid.Port = cid.Addr.Port()
    rid.UniqueNumber = cid.ID
    return rid, nil
}

// ResolveString returns flag if non-empty, else env, else "".
func ResolveString(flagVal, envName string) string {
    if flagVal != "" {
        return flagVal
    }
    return os.Getenv(envName)
}
```

- [ ] **Step 4: Run test (passes)**

```bash
go test ./cli/cliopts/ -v
```

Expected: 5 tests pass.

- [ ] **Step 5: Commit**

```bash
git add cli/cliopts/cliopts.go cli/cliopts/cliopts_test.go
git commit -m "cli/cliopts: env-primary resolve helpers (server-cid, ticket, task-id, runner-id)"
```

---

### Task 2: cursor file r/w

**Files:**
- Create: `cli/agent/cursor.go`, `cli/agent/cursor_test.go`

- [ ] **Step 1: Write failing test**

```go
package agent

import (
    "testing"
)

func TestCursor_RoundTrip(t *testing.T) {
    dir := t.TempDir()
    t.Setenv("XDG_CACHE_HOME", dir)
    if err := SaveCursor("abc123", 42); err != nil {
        t.Fatal(err)
    }
    got, err := LoadCursor("abc123")
    if err != nil {
        t.Fatal(err)
    }
    if got != 42 {
        t.Errorf("loaded cursor = %d, want 42", got)
    }
}

func TestCursor_LoadMissingReturnsZero(t *testing.T) {
    dir := t.TempDir()
    t.Setenv("XDG_CACHE_HOME", dir)
    got, err := LoadCursor("nonexistent")
    if err != nil {
        t.Fatal(err)
    }
    if got != 0 {
        t.Errorf("missing cursor = %d, want 0", got)
    }
}
```

- [ ] **Step 2: Run test (fails)**

```bash
go test ./cli/agent/ -run TestCursor -v
```

Expected: build error.

- [ ] **Step 3: Implement `cli/agent/cursor.go`**

```go
package agent

import (
    "errors"
    "io/fs"
    "os"
    "path/filepath"
    "strconv"
    "strings"
)

func cursorPath(taskIDHex string) (string, error) {
    base := os.Getenv("XDG_CACHE_HOME")
    if base == "" {
        home, err := os.UserHomeDir()
        if err != nil {
            return "", err
        }
        base = filepath.Join(home, ".cache")
    }
    dir := filepath.Join(base, "harness")
    if err := os.MkdirAll(dir, 0o755); err != nil {
        return "", err
    }
    return filepath.Join(dir, "agent-cursor-"+taskIDHex), nil
}

// LoadCursor returns 0 when no cursor file yet.
func LoadCursor(taskIDHex string) (uint64, error) {
    p, err := cursorPath(taskIDHex)
    if err != nil {
        return 0, err
    }
    data, err := os.ReadFile(p)
    if err != nil {
        if errors.Is(err, fs.ErrNotExist) {
            return 0, nil
        }
        return 0, err
    }
    v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
    if err != nil {
        return 0, err
    }
    return v, nil
}

func SaveCursor(taskIDHex string, cursor uint64) error {
    p, err := cursorPath(taskIDHex)
    if err != nil {
        return err
    }
    return os.WriteFile(p, []byte(strconv.FormatUint(cursor, 10)), 0o644)
}
```

- [ ] **Step 4: Run test (passes)**

```bash
go test ./cli/agent/ -run TestCursor -v
```

Expected: 2 tests pass.

- [ ] **Step 5: Commit**

```bash
git add cli/agent/cursor.go cli/agent/cursor_test.go
git commit -m "cli/agent: cursor file in $XDG_CACHE_HOME/harness/agent-cursor-<task>"
```

---

### Task 3: ConnectAgent helper

**Files:**
- Create: `cli/agent/conn.go`

- [ ] **Step 1: Implement `cli/agent/conn.go`**

```go
package agent

import (
    "context"
    "errors"
    "fmt"
    "log/slog"
    "time"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/cli"
    "github.com/on-keyday/agent-harness/cli/cliopts"
    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/peer"
    "github.com/on-keyday/agent-harness/runner/protocol"
    "github.com/on-keyday/agent-harness/transport"
    "github.com/on-keyday/agent-harness/trsf/wire"
)

type Flags struct {
    ServerCID string
    TaskID    string
    RunnerID  string
    Hostname  string
    WSPath    string
    // Note: no AuthTicket flag — env-only
}

type Conn struct {
    pc       *peer.Conn
    taskID   protocol.TaskID
    runnerID protocol.RunnerID
}

func (c *Conn) Close() { _ = c.pc.Close() }
func (c *Conn) PC() *peer.Conn { return c.pc }
func (c *Conn) TaskID() protocol.TaskID { return c.taskID }
func (c *Conn) RunnerID() protocol.RunnerID { return c.runnerID }

func ConnectAgent(ctx context.Context, f Flags) (*Conn, error) {
    cid, err := cliopts.ResolveServerCID(f.ServerCID)
    if err != nil {
        return nil, err
    }
    tid, err := cliopts.ResolveTaskID(f.TaskID)
    if err != nil {
        return nil, err
    }
    rid, err := cliopts.ResolveRunnerID(f.RunnerID)
    if err != nil {
        return nil, err
    }
    ticket, err := cliopts.ResolveAuthTicket()
    if err != nil {
        return nil, err
    }
    wsPath := cliopts.ResolveString(f.WSPath, "HARNESS_WS_PATH")
    if wsPath == "" {
        wsPath = cli.WebSocketPath
    } else {
        cli.WebSocketPath = wsPath
    }

    ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
        Logger: slog.Default(),
        Path:   wsPath,
        Mode:   objproto.EndpointModeClient,
    })
    if err != nil {
        return nil, fmt.Errorf("ws endpoint: %w", err)
    }
    pc, err := peer.Dial(ctx, ep, cid, peer.DialConfig{
        Logger:       slog.Default(),
        PingInterval: 30 * time.Second,
    })
    if err != nil {
        return nil, err
    }

    helloRespCh := make(chan agentboard.HelloStatus, 1)
    pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
        if kind != wire.ApplicationPayloadKind_AgentMessage {
            return
        }
        msg := &agentboard.AgentMessage{}
        if _, err := msg.Decode(payload); err != nil {
            return
        }
        if msg.Kind == agentboard.AgentMessageKind_hello_response {
            helloRespCh <- msg.HelloResponse().Status
        }
    })
    pc.Start(ctx)

    hostname := cliopts.ResolveString(f.Hostname, "HARNESS_HOSTNAME")
    hello := agentboard.AgentBridgeHello{
        RunnerId:   rid,
        TaskId:     tid,
        AuthTicket: ticket,
    }
    if hostname != "" {
        hello.SetHostname([]byte(hostname))
    }
    msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_hello}
    msg.SetHello(hello)
    data := msg.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentMessage)})
    if _, _, err := pc.Connection().SendMessage(data); err != nil {
        _ = pc.Close()
        return nil, fmt.Errorf("send hello: %w", err)
    }

    select {
    case status := <-helloRespCh:
        if status != agentboard.HelloStatusOk {
            _ = pc.Close()
            return nil, fmt.Errorf("hello rejected: %v", status)
        }
    case <-ctx.Done():
        _ = pc.Close()
        return nil, ctx.Err()
    }
    return &Conn{pc: pc, taskID: tid, runnerID: rid}, nil
}

// SendRequest is a low-level helper for subcommands.
func (c *Conn) SendRaw(msg *agentboard.AgentMessage) error {
    data := msg.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentMessage)})
    _, _, err := c.pc.Connection().SendMessage(data)
    if err != nil {
        return errors.Join(errors.New("agent: send failed"), err)
    }
    return nil
}
```

- [ ] **Step 2: Verify build**

```bash
go build ./cli/agent/
```

Expected: クリーン。

- [ ] **Step 3: Commit**

```bash
git add cli/agent/conn.go
git commit -m "cli/agent: ConnectAgent — Dial + AgentBridgeHello + ticket auth"
```

---

### Task 4: Send subcommand

**Files:**
- Create: `cli/agent/send.go`

- [ ] **Step 1: Implement**

```go
package agent

import (
    "context"
    "encoding/json"
    "errors"
    "flag"
    "fmt"
    "io"
    "math/rand"
    "os"
    "strings"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/trsf/wire"
)

// Send is the entry for `harness agent send`.
func Send(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
    fs := flag.NewFlagSet("agent send", flag.ContinueOnError)
    var f Flags
    serverCID := fs.String("server-cid", "", "server ConnectionID (env: HARNESS_SERVER_CID)")
    taskID := fs.String("task-id", "", "(debug) task id hex (env: HARNESS_TASK_ID)")
    runnerID := fs.String("runner-id", "", "(debug) runner id (env: HARNESS_RUNNER_ID)")
    topic := fs.String("topic", "", "agentboard topic")
    data := fs.String("data", "-", `payload string, or "-" to read stdin`)
    if err := fs.Parse(args); err != nil {
        return err
    }
    if *topic == "" {
        return errors.New("--topic required")
    }
    f.ServerCID, f.TaskID, f.RunnerID = *serverCID, *taskID, *runnerID

    var payload []byte
    if *data == "-" {
        b, err := io.ReadAll(stdin)
        if err != nil {
            return err
        }
        payload = b
    } else {
        payload = []byte(*data)
    }

    conn, err := ConnectAgent(ctx, f)
    if err != nil {
        return err
    }
    defer conn.Close()

    reqID := rand.Uint32()
    respCh := make(chan agentboard.SendResponse, 1)
    conn.PC().SetOnControl(func(kind wire.ApplicationPayloadKind, p []byte) {
        if kind != wire.ApplicationPayloadKind_AgentMessage {
            return
        }
        msg := &agentboard.AgentMessage{}
        if _, err := msg.Decode(p); err != nil {
            return
        }
        if msg.Kind == agentboard.AgentMessageKind_send_response {
            r := msg.SendResponse()
            if r.RequestId == reqID {
                respCh <- *r
            }
        }
    })

    msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_send}
    sr := agentboard.SendRequest{RequestId: reqID, Topic: []byte(*topic), Payload: payload}
    msg.SetSend(sr)
    if err := conn.SendRaw(msg); err != nil {
        return err
    }

    select {
    case resp := <-respCh:
        if resp.Status != agentboard.SendStatus_ok {
            return fmt.Errorf("send rejected: %v", resp.Status)
        }
        out, _ := json.Marshal(map[string]any{"seq": resp.Seq, "status": "ok"})
        fmt.Fprintln(stdout, string(out))
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

var _ = strings.HasPrefix // silence unused import in some builds
var _ = os.Stderr
```

- [ ] **Step 2: Build**

```bash
go build ./cli/agent/
```

Expected: クリーン。

- [ ] **Step 3: Commit**

```bash
git add cli/agent/send.go
git commit -m "cli/agent: send subcommand"
```

---

### Task 5: Wait subcommand

**Files:**
- Create: `cli/agent/wait.go`

- [ ] **Step 1: Implement**

```go
package agent

import (
    "context"
    "encoding/json"
    "errors"
    "flag"
    "fmt"
    "io"
    "math/rand"
    "time"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/trsf/wire"
)

func Wait(ctx context.Context, args []string, stdout io.Writer) error {
    fs := flag.NewFlagSet("agent wait", flag.ContinueOnError)
    var f Flags
    serverCID := fs.String("server-cid", "", "")
    taskID := fs.String("task-id", "", "")
    runnerID := fs.String("runner-id", "", "")
    topic := fs.String("topic", "", "topic to wait on")
    sinceLast := fs.Bool("since-last", false, "use the persisted cursor")
    since := fs.Uint64("since", 0, "cursor to wait beyond (ignored if --since-last)")
    timeout := fs.Duration("timeout", 5*time.Minute, "max block duration")
    if err := fs.Parse(args); err != nil {
        return err
    }
    if *topic == "" {
        return errors.New("--topic required")
    }
    f.ServerCID, f.TaskID, f.RunnerID = *serverCID, *taskID, *runnerID

    conn, err := ConnectAgent(ctx, f)
    if err != nil {
        return err
    }
    defer conn.Close()

    cursor := *since
    if *sinceLast {
        c, err := LoadCursor(hexTaskID(conn.TaskID()))
        if err == nil {
            cursor = c
        }
    }

    reqID := rand.Uint32()
    respCh := make(chan agentboard.WaitResponse, 1)
    conn.PC().SetOnControl(func(kind wire.ApplicationPayloadKind, p []byte) {
        if kind != wire.ApplicationPayloadKind_AgentMessage {
            return
        }
        msg := &agentboard.AgentMessage{}
        if _, err := msg.Decode(p); err != nil {
            return
        }
        if msg.Kind == agentboard.AgentMessageKind_wait_response {
            r := msg.WaitResponse()
            if r.RequestId == reqID {
                respCh <- *r
            }
        }
    })

    waitMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_wait}
    waitMsg.SetWait(agentboard.WaitRequest{
        RequestId: reqID,
        Pattern:   []byte(*topic),
        Since:     cursor,
        TimeoutMs: uint32(timeout.Milliseconds()),
    })
    if err := conn.SendRaw(waitMsg); err != nil {
        return err
    }

    select {
    case r := <-respCh:
        for _, m := range r.Msgs {
            line, _ := json.Marshal(map[string]any{
                "seq":     m.Seq,
                "topic":   string(m.Topic),
                "payload": json.RawMessage(m.Payload),
            })
            fmt.Fprintln(stdout, string(line))
        }
        if *sinceLast {
            _ = SaveCursor(hexTaskID(conn.TaskID()), r.NextCursor)
        }
        if r.TimedOut == 1 && len(r.Msgs) == 0 {
            return errors.New("timeout")
        }
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

`hexTaskID` は `cli/agent/util.go` に追加: `func hexTaskID(t protocol.TaskID) string { return hex.EncodeToString(t.Id[:]) }`

- [ ] **Step 2: Build**

```bash
go build ./cli/agent/
```

Expected: クリーン。

- [ ] **Step 3: Commit**

```bash
git add cli/agent/wait.go cli/agent/util.go
git commit -m "cli/agent: wait subcommand with --since-last cursor"
```

---

### Task 6: Inbox subcommand

**Files:**
- Create: `cli/agent/inbox.go`

- [ ] **Step 1: Implement**

```go
package agent

import (
    "context"
    "encoding/json"
    "flag"
    "fmt"
    "io"
    "math/rand"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/trsf/wire"
)

func Inbox(ctx context.Context, args []string, stdout io.Writer) error {
    fs := flag.NewFlagSet("agent inbox", flag.ContinueOnError)
    var f Flags
    serverCID := fs.String("server-cid", "", "")
    taskID := fs.String("task-id", "", "")
    runnerID := fs.String("runner-id", "", "")
    sinceLast := fs.Bool("since-last", false, "use persisted cursor")
    since := fs.Uint64("since", 0, "cursor (ignored if --since-last)")
    asJSON := fs.Bool("json", false, "output JSON Lines (default: also JSON Lines, kept for hook compatibility)")
    if err := fs.Parse(args); err != nil {
        return err
    }
    f.ServerCID, f.TaskID, f.RunnerID = *serverCID, *taskID, *runnerID
    _ = asJSON // current output is always JSON Lines; flag accepted for clarity

    conn, err := ConnectAgent(ctx, f)
    if err != nil {
        return err
    }
    defer conn.Close()

    cursor := *since
    if *sinceLast {
        c, err := LoadCursor(hexTaskID(conn.TaskID()))
        if err == nil {
            cursor = c
        }
    }

    reqID := rand.Uint32()
    respCh := make(chan agentboard.InboxResponse, 1)
    conn.PC().SetOnControl(func(kind wire.ApplicationPayloadKind, p []byte) {
        if kind != wire.ApplicationPayloadKind_AgentMessage {
            return
        }
        msg := &agentboard.AgentMessage{}
        if _, err := msg.Decode(p); err != nil {
            return
        }
        if msg.Kind == agentboard.AgentMessageKind_inbox_response {
            r := msg.InboxResponse()
            if r.RequestId == reqID {
                respCh <- *r
            }
        }
    })

    msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_inbox}
    msg.SetInbox(agentboard.InboxRequest{RequestId: reqID, Since: cursor})
    if err := conn.SendRaw(msg); err != nil {
        return err
    }

    select {
    case r := <-respCh:
        for _, m := range r.Msgs {
            line, _ := json.Marshal(map[string]any{
                "seq":     m.Seq,
                "topic":   string(m.Topic),
                "payload": json.RawMessage(m.Payload),
            })
            fmt.Fprintln(stdout, string(line))
        }
        if *sinceLast {
            _ = SaveCursor(hexTaskID(conn.TaskID()), r.NextCursor)
        }
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

- [ ] **Step 2: Build + commit**

```bash
go build ./cli/agent/
git add cli/agent/inbox.go
git commit -m "cli/agent: inbox subcommand (non-blocking, JSON Lines output)"
```

---

### Task 7: Subscribe / Unsubscribe subcommands

**Files:**
- Create: `cli/agent/subscribe.go`

- [ ] **Step 1: Implement both in one file**

```go
package agent

import (
    "context"
    "errors"
    "flag"
    "fmt"
    "io"
    "math/rand"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/trsf/wire"
)

func subscribeOrUnsub(ctx context.Context, args []string, stdout io.Writer, kind agentboard.AgentMessageKind) error {
    fs := flag.NewFlagSet("agent subscribe", flag.ContinueOnError)
    var f Flags
    serverCID := fs.String("server-cid", "", "")
    taskID := fs.String("task-id", "", "")
    runnerID := fs.String("runner-id", "", "")
    pattern := fs.String("topic", "", "topic to subscribe (exact match in v1)")
    if err := fs.Parse(args); err != nil {
        return err
    }
    if *pattern == "" {
        return errors.New("--topic required")
    }
    f.ServerCID, f.TaskID, f.RunnerID = *serverCID, *taskID, *runnerID

    conn, err := ConnectAgent(ctx, f)
    if err != nil {
        return err
    }
    defer conn.Close()

    reqID := rand.Uint32()
    respCh := make(chan agentboard.SubscribeResponse, 1)
    conn.PC().SetOnControl(func(k wire.ApplicationPayloadKind, p []byte) {
        if k != wire.ApplicationPayloadKind_AgentMessage {
            return
        }
        msg := &agentboard.AgentMessage{}
        if _, err := msg.Decode(p); err != nil {
            return
        }
        if msg.Kind == agentboard.AgentMessageKind_subscribe_response {
            r := msg.SubscribeResponse()
            if r.RequestId == reqID {
                respCh <- *r
            }
        }
    })

    msg := &agentboard.AgentMessage{Kind: kind}
    if kind == agentboard.AgentMessageKind_subscribe {
        msg.SetSubscribe(agentboard.SubscribeRequest{RequestId: reqID, Pattern: []byte(*pattern)})
    } else {
        msg.SetUnsubscribe(agentboard.UnsubscribeRequest{RequestId: reqID, Pattern: []byte(*pattern)})
    }
    if err := conn.SendRaw(msg); err != nil {
        return err
    }
    select {
    case r := <-respCh:
        if r.Status != agentboard.SubscribeStatus_ok {
            return fmt.Errorf("subscribe failed: %v", r.Status)
        }
        fmt.Fprintln(stdout, `{"status":"ok"}`)
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

func Subscribe(ctx context.Context, args []string, stdout io.Writer) error {
    return subscribeOrUnsub(ctx, args, stdout, agentboard.AgentMessageKind_subscribe)
}
func Unsubscribe(ctx context.Context, args []string, stdout io.Writer) error {
    return subscribeOrUnsub(ctx, args, stdout, agentboard.AgentMessageKind_unsubscribe)
}
```

- [ ] **Step 2: Build + commit**

```bash
go build ./cli/agent/
git add cli/agent/subscribe.go
git commit -m "cli/agent: subscribe/unsubscribe subcommands"
```

---

### Task 8: Dispatch (sugar)

**Files:**
- Create: `cli/agent/dispatch.go`

- [ ] **Step 1: Implement**

`dispatch` は `send` + `wait` を 1 コマンドで行う糖衣構文。reply pattern は呼び出し側が指定:

```go
package agent

import (
    "context"
    "encoding/json"
    "errors"
    "flag"
    "fmt"
    "io"
    "math/rand"
    "time"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/trsf/wire"
)

func Dispatch(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
    fs := flag.NewFlagSet("agent dispatch", flag.ContinueOnError)
    var f Flags
    serverCID := fs.String("server-cid", "", "")
    taskID := fs.String("task-id", "", "")
    runnerID := fs.String("runner-id", "", "")
    topic := fs.String("topic", "", "topic to send to")
    replyTopic := fs.String("reply-topic", "", "topic to wait for reply on")
    data := fs.String("data", "-", "payload string or - for stdin")
    timeout := fs.Duration("timeout", 5*time.Minute, "max wait")
    if err := fs.Parse(args); err != nil {
        return err
    }
    if *topic == "" || *replyTopic == "" {
        return errors.New("--topic and --reply-topic required")
    }
    f.ServerCID, f.TaskID, f.RunnerID = *serverCID, *taskID, *runnerID

    var payload []byte
    if *data == "-" {
        b, err := io.ReadAll(stdin)
        if err != nil {
            return err
        }
        payload = b
    } else {
        payload = []byte(*data)
    }

    conn, err := ConnectAgent(ctx, f)
    if err != nil {
        return err
    }
    defer conn.Close()

    sendID, waitID := rand.Uint32(), rand.Uint32()
    sendCh := make(chan agentboard.SendResponse, 1)
    waitCh := make(chan agentboard.WaitResponse, 1)
    conn.PC().SetOnControl(func(kind wire.ApplicationPayloadKind, p []byte) {
        if kind != wire.ApplicationPayloadKind_AgentMessage {
            return
        }
        msg := &agentboard.AgentMessage{}
        if _, err := msg.Decode(p); err != nil {
            return
        }
        switch msg.Kind {
        case agentboard.AgentMessageKind_send_response:
            r := msg.SendResponse()
            if r.RequestId == sendID {
                sendCh <- *r
            }
        case agentboard.AgentMessageKind_wait_response:
            r := msg.WaitResponse()
            if r.RequestId == waitID {
                waitCh <- *r
            }
        }
    })

    sendMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_send}
    sendMsg.SetSend(agentboard.SendRequest{RequestId: sendID, Topic: []byte(*topic), Payload: payload})
    if err := conn.SendRaw(sendMsg); err != nil {
        return err
    }
    select {
    case r := <-sendCh:
        if r.Status != agentboard.SendStatus_ok {
            return fmt.Errorf("send failed: %v", r.Status)
        }
    case <-ctx.Done():
        return ctx.Err()
    }

    waitMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_wait}
    waitMsg.SetWait(agentboard.WaitRequest{
        RequestId: waitID, Pattern: []byte(*replyTopic), Since: 0, TimeoutMs: uint32(timeout.Milliseconds()),
    })
    if err := conn.SendRaw(waitMsg); err != nil {
        return err
    }
    select {
    case r := <-waitCh:
        for _, m := range r.Msgs {
            line, _ := json.Marshal(map[string]any{
                "seq": m.Seq, "topic": string(m.Topic), "payload": json.RawMessage(m.Payload),
            })
            fmt.Fprintln(stdout, string(line))
        }
        if r.TimedOut == 1 && len(r.Msgs) == 0 {
            return errors.New("dispatch reply timeout")
        }
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

- [ ] **Step 2: Build + commit**

```bash
go build ./cli/agent/
git add cli/agent/dispatch.go
git commit -m "cli/agent: dispatch subcommand (send + wait sugar)"
```

---

### Task 9: Register in `cmd/harness-cli/main.go`

**Files:**
- Modify: `cmd/harness-cli/main.go`

- [ ] **Step 1: Add `agent` case to switch**

```go
case "agent":
    if len(args) == 0 {
        agentUsage()
        os.Exit(2)
    }
    sub := args[0]
    rest := args[1:]
    var err error
    switch sub {
    case "send":
        err = agent.Send(ctx, rest, os.Stdin, os.Stdout)
    case "wait":
        err = agent.Wait(ctx, rest, os.Stdout)
    case "inbox":
        err = agent.Inbox(ctx, rest, os.Stdout)
    case "subscribe":
        err = agent.Subscribe(ctx, rest, os.Stdout)
    case "unsubscribe":
        err = agent.Unsubscribe(ctx, rest, os.Stdout)
    case "dispatch":
        err = agent.Dispatch(ctx, rest, os.Stdin, os.Stdout)
    default:
        agentUsage()
        os.Exit(2)
    }
    if err != nil {
        die(err)
    }
```

`agentUsage` 関数を追加し、各 subcommand の help を出す。

- [ ] **Step 2: Build**

```bash
go build ./cmd/harness-cli/
```

Expected: クリーン。

- [ ] **Step 3: Smoke test**

```bash
./harness-cli agent  # → usage
./harness-cli agent send  # → "--topic required"
HARNESS_AUTH_TICKET=$(printf '%032d' 0) ./harness-cli agent send --topic foo --data bar
# → 接続失敗 (server 未起動なら) でも flag/env parse 通過確認
```

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "harness-cli: register agent subcommand group"
```

---

### Task 10: Integration test (2 CLI processes through real server)

**Files:**
- Create: `cli/agent/agent_test.go`

- [ ] **Step 1: Write test that spins up server and runs 2 CLI invocations**

```go
package agent_test

import (
    "context"
    "encoding/hex"
    "io"
    "net"
    "os/exec"
    "strings"
    "testing"
    "time"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/runner/protocol"
)

// TestAgentCLI_E2E: spin up an in-process server, register two tickets,
// invoke `harness-cli agent send` and `harness-cli agent wait` as subprocesses
// (so env propagation is exercised), assert wait yields the sent message.
func TestAgentCLI_E2E(t *testing.T) {
    // 1. Start an in-process harness-server bound to a random localhost port.
    //    Reuse the agentboard e2e fixture from P1 if it's exported, or rebuild here.
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil { t.Fatal(err) }
    defer ln.Close()
    _ = ln // wire up server.Server with Board (see P1 Task 10)

    // 2. Pre-register tickets for two synthetic agents A, B.
    var ridA, ridB protocol.RunnerID
    var tidA, tidB protocol.TaskID
    var ticketA, ticketB [16]byte
    // ... fill, register via Board.Registry().Register

    // 3. Spawn `go run ./cmd/harness-cli/ agent send` with env for agent A.
    //    Capture stdout JSON and assert seq received.
    cmd := exec.Command("go", "run", "./cmd/harness-cli/", "agent", "send",
        "--topic", "task/abc/dispatch", "--data", "hi from A")
    cmd.Env = append(cmd.Env,
        "HARNESS_SERVER_CID=ws:"+ln.Addr().String()+"-*",
        "HARNESS_TASK_ID="+hex.EncodeToString(tidA.Id[:]),
        // ... runner-id, ticket, etc.
    )
    out, err := cmd.CombinedOutput()
    if err != nil { t.Fatalf("send: %v: %s", err, out) }
    if !strings.Contains(string(out), `"status":"ok"`) {
        t.Errorf("send output missing ok: %s", out)
    }

    // 4. Spawn `agent wait` for agent B, expect to see the message.
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    waitCmd := exec.CommandContext(ctx, "go", "run", "./cmd/harness-cli/", "agent", "wait",
        "--topic", "task/abc/dispatch", "--timeout", "3s")
    waitCmd.Env = append(waitCmd.Env, /* B's env ... */)
    out2, err := waitCmd.CombinedOutput()
    if err != nil { t.Fatalf("wait: %v: %s", err, out2) }
    if !strings.Contains(string(out2), "hi from A") {
        t.Errorf("wait output missing payload: %s", out2)
    }
    _ = io.Discard
    _ = agentboard.Board{}
}
```

注: `go run ./cmd/harness-cli/` を test 内で実行するのは遅いが、env 伝播 / subprocess での flag parse を本物に近く検証できる。in-process invocation で済ませる場合は `agent.Send` / `agent.Wait` を直接 import で叩く。

- [ ] **Step 2: Run test**

```bash
go test ./cli/agent/ -run TestAgentCLI_E2E -v -timeout 60s
```

Expected: PASS。失敗時は ConnectAgent が server 側 hello validation を通っているか確認 (registry 登録が間に合っているか)。

- [ ] **Step 3: Commit**

```bash
git add cli/agent/agent_test.go
git commit -m "cli/agent: e2e — send + wait round-trip through real harness-server"
```

---

## Self-review checklist

- [ ] `HARNESS_AUTH_TICKET` を accept する flag を **作っていない**ことを確認 (env-only)
- [ ] 各 subcommand の `OnControl` callback は正しい `request_id` でフィルタしているか?
- [ ] cursor file は task_id 単位で分かれているか? (複数 task が混ざらない)
- [ ] cursor を読み取れない (file 不存在) は **error にしない** で 0 として扱うか?
- [ ] subcommand exit code: success=0 / runtime error=1 / usage error=2 を守っているか?
- [ ] JSON Lines: 1 行 1 json、改行で区切る (1 つの JSON array にしない)
- [ ] `payload` field が任意 binary を含む可能性 — `json.RawMessage` でラップするか、UTF-8 不正だった場合の base64 fallback (v1 は UTF-8 前提でよい)

---

## Done definition

- `go test ./cli/agent/ ./cli/cliopts/ -v` 全 pass
- `go build ./...` クリーン
- 手動 dogfood: 2 つの terminal で `HARNESS_*` env を export し、片方で `harness-cli agent send`、もう片方で `harness-cli agent wait` で round-trip 成功
- claude (実 binary) を agent-runner 経由で起動し、Bash tool で `harness-cli agent send/wait` が動く (P2 で env 注入済の前提)
