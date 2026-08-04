# Sender agent-profile attestation on the agentboard — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every agentboard message tells its receiver which agent runtime published it, attested by the server and frozen at publish time.

**Architecture:** The server resolves `TaskEntry.AgentProfile` at agent-hello and stores it in the board's per-(runner, task) `taskState`, exactly where the hostname already lives. `Board.Send` stamps it into the topic ring alongside the other sender-attestation fields, so a message keeps the profile that published it even after `--resume` re-opens the same task id under a different runtime. Three `.bgn` formats gain one length-prefixed string each; every surface that already renders `from_hostname` gains the new field next to it.

**Tech Stack:** Go, brgen schema codegen (`make protoregen`), GOOS=js/wasm for the WebUI module, vanilla JS for the WebUI DOM.

**Spec:** `docs/superpowers/specs/2026-08-05-board-sender-agent-profile-design.md`

## Global Constraints

- **Never hand-edit generated Go.** `agentboard/agentboard.go` and `runner/protocol/message.go` are brgen output. Change the `.bgn` and run `make protoregen ARGS='<path to .bgn>'`.
- **The agent never supplies the profile.** The value is read server-side from `TaskStore` and threaded through `taskState`. No new field in `AgentInfo` / `ClientHello`.
- **`""` means "the server could not attribute a runtime"** — never "runner default". Both submit and open-interactive resolve to a concrete name before `Create` (`server/task_handler.go:611-613`, `:949`).
- **Verification before each commit:** `make check` and `make wasm-check` (plus the task's own `go test` invocation). `go build ./...` alone hides explicit-pattern breaks.
- **Commit style:** repo convention is a lowercase `type(scope):` prefix and a descriptive sentence, e.g. `feat(agentboard): a message remembers the runtime that published it`. End every commit message with:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  ```
- **Field name is `from_agent_profile`** on the wire, `FromAgentProfile` in Go, `from.agent` in agent-facing JSON, `agent=` in operator text renderings, `agentProfile` in the wasm→JS bridge. Do not vary these.

---

### Task 1: agentboard carries the profile from attach to the ring

**Files:**
- Modify: `agentboard/agentboard.bgn` (`DeliveredMessage` at :86-98, `RetainedMeta` at :192-199)
- Regenerate: `agentboard/agentboard.go`
- Modify: `agentboard/topic.go:10-18` (`RetainedMessage`), `:33-50` (`topic.append`)
- Modify: `agentboard/taskstate.go:16-23` (`taskState`), `:83-95` (`setIdentity` / `identity`)
- Modify: `agentboard/conn.go:34-42` (`ConnState.Identity`)
- Modify: `agentboard/board.go:81-93` (`RegisterTask`), `:110-134` (`Attach`), `:199-253` (`Send`)
- Test: `agentboard/board_test.go` (new test + existing call sites)
- Test call sites to update: `agentboard/board_test.go`, `agentboard/board_listings_test.go`, `agentboard/e2e_test.go`, `server/capabilities_test.go:964`, `cli/board_e2e_test.go:99,100,143`

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  ```go
  // agentboard
  type RetainedMessage struct {
      Seq              uint64
      Topic            string
      Payload          []byte
      FromRunner       protocol.RunnerID
      FromTask         protocol.TaskID
      FromHostname     string
      FromAgentProfile string   // NEW — frozen at publish
      ReceivedAt       time.Time
  }

  func (b *Board) RegisterTask(rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte, agentProfile string)
  func (b *Board) Attach(rid RunnerID, tid TaskID, hostname, agentProfile string) *ConnState
  func (b *Board) Send(topicName string, payload []byte, fromRid protocol.RunnerID, fromTid protocol.TaskID, fromHost, fromProfile string) (uint64, error)
  func (c *ConnState) Identity() (protocol.RunnerID, protocol.TaskID, string, string) // rid, tid, hostname, agentProfile
  ```
  Generated: `DeliveredMessage.FromAgentProfile` / `SetFromAgentProfile([]uint8) bool`, and the same pair on `RetainedMeta`.

- [ ] **Step 1: Write the failing test**

Append to `agentboard/board_test.go`:

```go
// TestBoard_RetainedProfileFrozenAcrossReattach is the regression this field
// exists for. `session new --resume <task-id>` reuses a task id — and so its
// chat.<short-id> topic — while TaskStore.Resume overwrites the task's agent
// profile. The topic ring outlives the publishing taskState, so a message
// published under the old profile must keep reporting the old profile after
// the same (rid, tid) re-attaches under a new one.
func TestBoard_RetainedProfileFrozenAcrossReattach(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	first := b.Attach(RunnerID{}, TaskID{}, "test-host", "codex")
	if err := b.Subscribe(first, "topic/resumed"); err != nil {
		t.Fatal(err)
	}
	rid, tid, host, profile := first.Identity()
	if profile != "codex" {
		t.Fatalf("Identity() profile = %q, want %q", profile, "codex")
	}
	if _, err := b.Send("topic/resumed", []byte("from codex"), rid, tid, host, profile); err != nil {
		t.Fatal(err)
	}
	b.Detach(first)

	// Same (rid, tid) returns under a different profile, as --resume does.
	// Detach preserves the taskState, so this re-attach overwrites identity
	// in place — precisely the case a read-time lookup would get wrong.
	second := b.Attach(RunnerID{}, TaskID{}, "test-host", "claude")
	defer b.Detach(second)
	rid, tid, host, profile = second.Identity()
	if profile != "claude" {
		t.Fatalf("Identity() after re-attach = %q, want %q", profile, "claude")
	}
	if _, err := b.Send("topic/resumed", []byte("from claude"), rid, tid, host, profile); err != nil {
		t.Fatal(err)
	}

	msgs, found := b.ListRetained("topic/resumed")
	if !found || len(msgs) != 2 {
		t.Fatalf("ListRetained = %d msgs (found=%v), want 2", len(msgs), found)
	}
	if msgs[0].FromAgentProfile != "codex" {
		t.Errorf("retained[0].FromAgentProfile = %q, want %q (frozen at publish)", msgs[0].FromAgentProfile, "codex")
	}
	if msgs[1].FromAgentProfile != "claude" {
		t.Errorf("retained[1].FromAgentProfile = %q, want %q", msgs[1].FromAgentProfile, "claude")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./agentboard/ -run TestBoard_RetainedProfileFrozenAcrossReattach`
Expected: FAIL — compile errors, `too many arguments in call to b.Attach` / `b.Send`, and `msgs[0].FromAgentProfile undefined`.

- [ ] **Step 3: Extend the schema**

In `agentboard/agentboard.bgn`, add to `DeliveredMessage` immediately after the `from_hostname` pair:

```
    # from_agent_profile is the agent profile name the sending task was
    # running under AT PUBLISH TIME (e.g. "claude" / "codex"), resolved
    # server-side from the task record — the agent does not supply it.
    # Frozen per message because a task's profile is per-open: `--resume`
    # reuses a task id under a different runtime, so resolving it at read
    # time would misattribute everything already in the ring.
    # Empty means the server could not attribute a runtime (e.g. a
    # server-originated publish); it never means "runner default".
    from_agent_profile_len :u8
    from_agent_profile :[from_agent_profile_len]u8
```

Add the same two lines (no comment needed; reference `DeliveredMessage`) to `RetainedMeta` after its `from_hostname` pair:

```
    # See DeliveredMessage.from_agent_profile.
    from_agent_profile_len :u8
    from_agent_profile :[from_agent_profile_len]u8
```

- [ ] **Step 4: Regenerate the Go**

Run: `make protoregen ARGS='agentboard/agentboard.bgn'`
Expected: `agentboard/agentboard.go` rewritten, `git diff --stat` shows only that file.

- [ ] **Step 5: Thread the field through the in-memory types**

`agentboard/topic.go` — add the field and the `append` parameter:

```go
type RetainedMessage struct {
	Seq              uint64
	Topic            string
	Payload          []byte
	FromRunner       protocol.RunnerID
	FromTask         protocol.TaskID
	FromHostname     string
	FromAgentProfile string
	ReceivedAt       time.Time
}

func (t *topic) append(seq uint64, payload []byte, fromRid protocol.RunnerID, fromTid protocol.TaskID, fromHost, fromProfile string) {
	// ... unchanged, plus:
	//   FromAgentProfile: fromProfile,
}
```

`agentboard/taskstate.go` — add `profile string` to the struct and to both accessors:

```go
func (t *taskState) setIdentity(rid protocol.RunnerID, tid protocol.TaskID, hostname, agentProfile string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rid = rid
	t.tid = tid
	t.host = hostname
	t.profile = agentProfile
}

func (t *taskState) identity() (protocol.RunnerID, protocol.TaskID, string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rid, t.tid, t.host, t.profile
}
```

`agentboard/conn.go` — widen `Identity()` to four values, keeping the nil guard:

```go
// Identity returns the authenticated (RunnerID, TaskID, hostname, agentProfile)
// captured at Attach time. The server uses this to attribute published messages
// to the correct sender without trusting agent-supplied fields.
func (c *ConnState) Identity() (protocol.RunnerID, protocol.TaskID, string, string) {
	if c == nil || c.task == nil {
		return protocol.RunnerID{}, protocol.TaskID{}, "", ""
	}
	return c.task.identity()
}
```

`agentboard/board.go` — three signatures:
- `RegisterTask(rid, tid, ticket, agentProfile string)`: pass `agentProfile` to `ts.setIdentity(rid, tid, "", agentProfile)`.
- `Attach(rid, tid, hostname, agentProfile string)`: pass to `ts.setIdentity(protoRid, protoTid, hostname, agentProfile)`.
- `Send(..., fromHost, fromProfile string)`: pass to `t.append(seq, payload, fromRid, fromTid, fromHost, fromProfile)`. Inside the delivery loop, `ts.identity()` now returns four values — update the discard to `rid, tid, _, _ := ts.identity()`.

Also update the `Attach` and `Send` doc comments to mention the profile alongside the hostname.

- [ ] **Step 6: Keep the server compiling**

Widening these signatures breaks every non-test caller in `server/`. Task 2
supplies the real values; here, pass a placeholder so the tree stays green —
except `await_idle_handler.go`, where `""` is the **final** value, because a
server-originated publish has no agent behind it.

| Site | Change |
|---|---|
| `server/agent_handler.go:117` | `s.Board.Attach(rid, tid, string(info.Hostname), "")` — Task 2 replaces `""` with the store lookup |
| `server/agent_handler.go:150-151` | `fromRid, fromTid, fromHost, _ := ac.state.Identity()`, then `s.Board.Send(..., fromHost, "")` — Task 2 threads the real profile |
| `server/dispatch.go:198` | `RegisterTask(..., ticket, "")` — Task 2 passes `task.AgentProfile` |
| `server/task_handler.go:1022` | `RegisterTask(..., ticket, "")` — Task 2 passes `resolved` |
| `server/server.go:1148` | `RegisterTask(..., ticket, "")` — Task 2 passes `entry.AgentProfile` |
| `server/await_idle_handler.go:165` | `h.Board.Send(topic, []byte(payload), placeholderRunnerID(), requester, "server", "")` — **final**, not a placeholder |

Then pin the last one down with an assertion. In
`server/await_idle_test.go`, inside `TestHandleAwaitIdle_BoardSinkPublishesOnFire`,
after the existing payload-substring loop:

```go
	// A server-originated publish has no agent behind it: the profile is
	// empty and the sender is identifiable by hostname alone.
	if msgs[0].FromHostname != "server" {
		t.Errorf("FromHostname = %q, want %q", msgs[0].FromHostname, "server")
	}
	if msgs[0].FromAgentProfile != "" {
		t.Errorf("FromAgentProfile = %q, want empty (not attributed)", msgs[0].FromAgentProfile)
	}
```

- [ ] **Step 7: Update the existing test call sites**

Append the new argument. Use `""` where the test does not care about the profile, since `""` is the defined "not attributed" value:

```bash
# Inspect first, then edit each hit by hand — do not blind-sed, the argument
# position differs between Attach (4th) and Send (6th).
grep -rn "\.Attach(\|\.Send(\|RegisterTask(" --include=*_test.go agentboard server cli
```

Files with hits: `agentboard/board_test.go`, `agentboard/board_listings_test.go`, `agentboard/e2e_test.go`, `server/capabilities_test.go:964`, `cli/board_e2e_test.go:99,100,143`. Ignore `.Send(` hits on `SessionMux` / `ConnHandle` in `server/session_mux*_test.go` and `server/mode_tracker_test.go` — different receivers.

- [ ] **Step 8: Run the tests**

Run: `go test ./agentboard/... ./server/... ./cli/...`
Expected: PASS, including `TestBoard_RetainedProfileFrozenAcrossReattach`. `server/...` has a known flaky `OpenInteractive` SessionMux test (~1 run in 4) that predates this change — re-run once before investigating it.

- [ ] **Step 9: Verify the build and commit**

```bash
make check && make wasm-check
git add agentboard/ server/ cli/board_e2e_test.go
git commit -m "$(cat <<'EOF'
feat(agentboard): a retained message remembers the runtime that published it

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: the server resolves the profile, and the agent sees it

**Files:**
- Modify: `server/agent_handler.go:107-120` (`establishAgentIdentity`), `:151` (`Board.Send` call), `:293-300` (wait builder), `:335-342` (inbox builder), `:494-501` (retained builder)
- Modify: `server/dispatch.go:198`, `server/task_handler.go:1022`, `server/server.go:1148` (the three `RegisterTask` call sites)
- Modify: `cli/agent/json_emit.go:21-37` (`emitMessageLine`)
- Modify: `cli/agent/inbox.go:110,115`, `cli/agent/wait.go:94`, `cli/agent/dispatch.go:134` (call sites)
- Modify: `cli/agent/retained.go:92-94` (the metadata line)
- Modify: `runner/agentskills/harness-cli/SKILL.md` and the synced copy `.claude/skills/harness-cli/SKILL.md`
- Test: `cli/agent/agent_e2e_test.go` (new E2E)

**Interfaces:**
- Consumes: everything Task 1 produced — `Board.Attach(rid, tid, hostname, agentProfile)`, `Board.Send(..., fromHost, fromProfile)`, `Board.RegisterTask(rid, tid, ticket, agentProfile)`, `ConnState.Identity() (rid, tid, host, profile)`, `RetainedMessage.FromAgentProfile`, and the generated `SetFromAgentProfile`.
- Produces: agent-facing JSON gains `from.agent` (string, always present, possibly `""`); `agent retained` lines gain `"from_agent"`.

- [ ] **Step 1: Write the failing test**

Append to `cli/agent/agent_e2e_test.go`. Note this test creates a real `TaskStore` entry — unlike the existing E2Es, which register a ticket directly and therefore have no task record to resolve a profile from:

```go
// TestAgentCLI_E2E_DeliveredMessageCarriesSenderProfile asserts that the
// server attests the SENDER's agent profile on delivery: agent B learns that
// agent A runs under "codex" without holding InfoGlobal (board delivery is
// uncapped, `ls` is not), and without A having supplied the value.
func TestAgentCLI_E2E_DeliveredMessageCarriesSenderProfile(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	const ridStrA = "ws:1.2.3.4:9010-1"
	const ridStrB = "ws:5.6.7.8:9011-2"
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9010, 1)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9011, 2)

	// A has a task record resolved to the "codex" profile; B does not need one.
	taskHexA := srv.Tasks().Create("/repo", "p", protocol.TaskKind_Interactive,
		protocol.ClientKind_Cli, protocol.TaskID{}, ridStrA, protocol.RunnerSelector{},
		nil, protocol.Capability_All, "codex")
	var tidA protocol.TaskID
	raw, err := hex.DecodeString(taskHexA)
	if err != nil || len(raw) != 16 {
		t.Fatalf("task id %q: decode err=%v len=%d", taskHexA, err, len(raw))
	}
	copy(tidA.Id[:], raw)
	tidB := mkTidE2E(0x7B)

	var ticketA, ticketB [16]byte
	ticketA[0] = 0xA1
	ticketB[0] = 0xB1
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	// The payload deliberately lies about the sender: the attested field must
	// not come from anything the agent wrote.
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/profile-e2e", "--data", `{"agent":"not-really-me"}`},
		nil, &sendOut,
	); err != nil {
		restoreA()
		t.Fatalf("agent.Send: %v", err)
	}
	restoreA()

	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var waitOut bytes.Buffer
	if err := agent.Wait(ctx,
		[]string{"--topic", "topic/profile-e2e", "--timeout", "2s"},
		&waitOut,
	); err != nil {
		restoreB()
		t.Fatalf("agent.Wait: %v", err)
	}
	restoreB()

	var rec struct {
		From struct {
			Agent    string `json:"agent"`
			Hostname string `json:"hostname"`
		} `json:"from"`
	}
	line := strings.TrimSpace(waitOut.String())
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("wait output is not JSON Lines: %v\n%s", err, waitOut.String())
	}
	if rec.From.Agent != "codex" {
		t.Errorf("from.agent = %q, want %q", rec.From.Agent, "codex")
	}
}
```

Add `"encoding/json"` to the file's imports if it is not already there.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cli/agent/ -run TestAgentCLI_E2E_DeliveredMessageCarriesSenderProfile -v`
Expected: FAIL with `from.agent = "", want "codex"` — the JSON record has no `agent` key yet, so it unmarshals to the empty string.

- [ ] **Step 3: Resolve the profile server-side**

`server/agent_handler.go`, in `establishAgentIdentity`, between `Validate` and `Attach`:

```go
	status := s.Board.Registry().Validate(rid, tid, info.AuthTicket)
	if status == agentboard.HelloStatusOk {
		// The profile is authority-side data: read it from the task record,
		// never from the agent's own hello. Empty when the store has no entry
		// for this id — a defined "not attributed", not "runner default".
		var profile string
		if s.tasks != nil {
			if e, ok := s.tasks.Get(hex.EncodeToString(info.TaskId.Id[:])); ok {
				profile = e.AgentProfile
			}
		}
		ac := s.getOrCreateAgentConn(conn)
		ac.helloed = true
		ac.state = s.Board.Attach(rid, tid, string(info.Hostname), profile)
	}
```

Add `"encoding/hex"` to the imports if absent.

- [ ] **Step 4: Put it on the wire**

`server/agent_handler.go`:
- `agentHandleSend` (`:150-151`): `fromRid, fromTid, fromHost, fromProfile := ac.state.Identity()` then pass `fromProfile` as `Board.Send`'s last argument.
- Wait builder (`:293-300`) and inbox builder (`:335-342`): after `dm.SetFromHostname(...)`, add `dm.SetFromAgentProfile([]byte(m.FromAgentProfile))`.
- Retained builder (`:494-501`): after `meta.SetFromHostname(...)`, add `meta.SetFromAgentProfile([]byte(m.FromAgentProfile))`.

The three `RegisterTask` call sites seed the same `taskState` before any agent attaches, so they must pass the profile too — `Attach` overwrites identity wholesale and a value seeded here would otherwise be the only one lost if a publish happened pre-attach:
- `server/dispatch.go:198` → `d.Board.RegisterTask(runnerIDFromConnID(runner.ID), taskIDFromHex(task.ID), ticket, task.AgentProfile)`
- `server/task_handler.go:1022` → `h.Board.RegisterTask(runnerIDFromConnID(runner.ID), taskIDFromHex(taskIDHex), ticket, resolved)`
- `server/server.go:1148` → `entry` is already in scope from the `s.tasks.Get` above at `:1139`; pass `entry.AgentProfile`.

- [ ] **Step 5: Surface it to the agent**

`cli/agent/json_emit.go` — widen the signature and the `from` block:

```go
// The from block carries server-attested sender info (RunnerID, TaskID,
// hostname, agent profile). It is always present, even for legacy messages
// where the bytes may be zero — that lets jq/grep consumers reliably address
// `.from.*`. An empty `agent` means the server could not attribute a runtime
// to the sender (e.g. a server-originated publish); it never means "runner
// default".
func emitMessageLine(w io.Writer, seq uint64, topic string, payload []byte, fromRid agentboard.RunnerID, fromTid agentboard.TaskID, fromHost, fromAgent string) {
	rec := map[string]any{
		"seq":         seq,
		"topic":       topic,
		"payload_b64": base64.StdEncoding.EncodeToString(payload),
		"from": map[string]any{
			"runner_id": boardRunnerIDString(fromRid),
			"task_id":   hex.EncodeToString(fromTid.Id[:]),
			"hostname":  fromHost,
			"agent":     fromAgent,
		},
	}
	// ... unchanged
}
```

Update the four call sites to pass `string(m.FromAgentProfile)` as the new final argument: `cli/agent/inbox.go:110`, `:115`, `cli/agent/wait.go:94`, `cli/agent/dispatch.go:134`.

`cli/agent/retained.go:92-94` — add the field to the metadata line, keeping the existing key order and adding the new key after `from_hostname`:

```go
				fmt.Fprintf(stdout,
					"{\"seq\":%d,\"from_task\":%q,\"from_hostname\":%q,\"from_agent\":%q,\"size\":%d,\"received_at_ms\":%d}\n",
					m.Seq, hex.EncodeToString(m.FromTask.Id[:]), string(m.FromHostname),
					string(m.FromAgentProfile), m.Size, m.ReceivedAtUnixMs)
```

- [ ] **Step 6: Run the tests**

Run: `go test ./cli/agent/... ./server/... ./agentboard/...`
Expected: PASS, including the new E2E.

- [ ] **Step 7: Document it in the skill**

Edit `runner/agentskills/harness-cli/SKILL.md` — this is the `go:embed` source of truth. In the "Reaching another agent" section, after the id-directed paragraph, add:

```markdown
Every delivered message carries `from.agent`: the agent profile the sending
task was running under at publish time (`"claude"`, `"codex"`, …), attested by
the server — the sender cannot set it. Use it before assuming your reply will
be read: the auto-inbox hook lives in Claude's `.claude/settings.json`, so a
peer whose `from.agent` is not `claude` may only see your message when it polls
`harness-cli agent inbox` itself. An empty `from.agent` means the server could
not attribute a runtime — a server-originated message such as an `await-idle`
notification, identifiable by `from.hostname == "server"`.
```

Then copy the file to the `.claude/skills/` mirror so the in-repo skill matches the embedded one:

```bash
cp runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md
git diff --stat .claude/skills/harness-cli/SKILL.md
```

Note: `.claude/skills/harness-cli/SKILL.md` already has uncommitted local edits from before this task. Keep them — re-apply by hand if the copy clobbers them, or diff the two files first and merge.

- [ ] **Step 8: Verify the build and commit**

```bash
make check && make wasm-check
git add server/ cli/agent/ runner/agentskills/ .claude/skills/harness-cli/SKILL.md
git commit -m "$(cat <<'EOF'
feat(server,cli): an agent learns which runtime sent it a message

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: the operator board view shows it too

**Files:**
- Modify: `runner/protocol/message.bgn:673-679` (`BoardMessageRow`)
- Regenerate: `runner/protocol/message.go`
- Modify: `server/board_handler.go:61-77` (`handleBoardRead` row builder)
- Modify: `cli/board.go:22-29` (`BoardMessage`), `:82-92` (row mapping)
- Modify: `cli/cmd_board.go:49-52` (text line)
- Modify: `tui/board.go:283` (message header), `:353-359` (list line)
- Modify: `cmd/harness-webui-wasm/main.go:678-689` (JS bridge object)
- Modify: `webui/static/main.js:3451-3455` (header spans), `:3480` (append order)
- Modify: `webui/static/style.css:1324-1326` (header span colors)
- Test: `cli/board_e2e_test.go` (extend `TestClientBoard_TopicsReadPurge`)

**Interfaces:**
- Consumes: `RetainedMessage.FromAgentProfile` (Task 1); the server-side resolution (Task 2) is what makes the value non-empty in production, but this task's test sets it directly via `Board.Send`.
- Produces: `cli.BoardMessage.FromAgentProfile string`; wasm bridge key `agentProfile`.

- [ ] **Step 1: Write the failing test**

In `cli/board_e2e_test.go`, extend `TestClientBoard_TopicsReadPurge`. Change the two seed publishes at `:99-100` to carry a profile, and add an assertion after the `BoardRead` result is checked:

```go
	srv.Board().Send("chat.x", []byte("hello"), protocol.RunnerID{}, protocol.TaskID{}, "h", "codex") //nolint:errcheck
	srv.Board().Send("chat.x", []byte("world"), protocol.RunnerID{}, protocol.TaskID{}, "h", "claude") //nolint:errcheck
```

```go
	// The operator board view attributes each message to the runtime that
	// published it — the two seeds above were published under different ones.
	if msgs[0].FromAgentProfile != "codex" {
		t.Errorf("msgs[0].FromAgentProfile = %q, want %q", msgs[0].FromAgentProfile, "codex")
	}
	if msgs[1].FromAgentProfile != "claude" {
		t.Errorf("msgs[1].FromAgentProfile = %q, want %q", msgs[1].FromAgentProfile, "claude")
	}
```

(If Task 1 already appended `""` to these two `Send` calls, replace those arguments rather than adding new ones.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cli/ -run TestClientBoard_TopicsReadPurge`
Expected: FAIL — `msgs[0].FromAgentProfile undefined (type cli.BoardMessage has no field or method FromAgentProfile)`.

- [ ] **Step 3: Extend the protocol schema and regenerate**

In `runner/protocol/message.bgn`, `BoardMessageRow`, after the `from_hostname` pair:

```
    # from_agent_profile is the agent profile the sending task was running
    # under at publish time. See agentboard/agentboard.bgn
    # DeliveredMessage.from_agent_profile. Empty = not attributed.
    from_agent_profile_len :u8
    from_agent_profile :[from_agent_profile_len]u8
```

Run: `make protoregen ARGS='runner/protocol/message.bgn'`
Expected: only `runner/protocol/message.go` changes.

- [ ] **Step 4: Fill it server-side and thread it to the operator client**

`server/board_handler.go`, in the `handleBoardRead` loop, after `row.SetFromHostname(...)`:

```go
		row.SetFromAgentProfile([]byte(m.FromAgentProfile))
```

`cli/board.go`:

```go
type BoardMessage struct {
	Seq              uint64
	FromTaskHex      string
	FromHostname     string
	FromAgentProfile string
	ReceivedAtMs     uint64
	Payload          []byte
}
```

and in the row mapping, `FromAgentProfile: string(m.FromAgentProfile),`.

- [ ] **Step 5: Render it on the three operator surfaces**

`cli/cmd_board.go:49-52`:

```go
		for _, m := range msgs {
			fmt.Fprintf(out, "#%d from=%s host=%s agent=%s size=%d at=%s\n",
				m.Seq, m.FromTaskHex, m.FromHostname, boardAgentOrDash(m.FromAgentProfile),
				len(m.Payload), boardMsToRFC3339(m.ReceivedAtMs))
```

Add the helper next to `boardMsToRFC3339` in the same file:

```go
// boardAgentOrDash renders an unattributed sender profile as "-" so the
// column never collapses to an empty run of spaces.
func boardAgentOrDash(profile string) string {
	if profile == "" {
		return "-"
	}
	return profile
}
```

`tui/board.go:283` — extend the detail header:

```go
	agentName := msg.FromAgentProfile
	if agentName == "" {
		agentName = "-"
	}
	header := fmt.Sprintf("seq=%d  from=%s  host=%s  agent=%s  at=%s", msg.Seq, fromShort, msg.FromHostname, agentName, at)
```

`tui/board.go:353-359` — extend the list line the same way:

```go
			agentName := msg.FromAgentProfile
			if agentName == "" {
				agentName = "-"
			}
			at := time.UnixMilli(int64(msg.ReceivedAtMs)).UTC().Format("15:04:05Z")
			msgList.WriteString(fmt.Sprintf("%s[%d] seq=%-5d  from=%s  agent=%s  %s\n",
				cursor, i+1, msg.Seq, fromShort, agentName, at))
```

`cmd/harness-webui-wasm/main.go:678-689` — add the bridge key and update the doc comment at `:656` to list it:

```go
					"fromHostname": m.FromHostname,
					"agentProfile": m.FromAgentProfile,
```

`webui/static/main.js:3451-3455` — add a span after `hostSpan`, following the existing class-name convention:

```js
        const agentSpan = document.createElement("span");
        agentSpan.className = "board-msg-agent";
        agentSpan.textContent = `agent=${m.agentProfile || "-"}`;
```

Append it to the header right after `hostSpan` — `webui/static/main.js:3480` reads `hdr.appendChild(hostSpan);`, so insert `hdr.appendChild(agentSpan);` between it and `hdr.appendChild(timeSpan);` on the next line.

`webui/static/style.css` — the header spans each get one color rule at `:1324-1326`. Add a fourth, in the same one-line form, using a hue not yet in that block (`#ce9178`, the only other accent already present in this stylesheet, at `:793`):

```css
.board-msg-agent { color: #ce9178; }
```

- [ ] **Step 6: Run the tests**

Run: `go test ./cli/... ./server/... ./agentboard/... ./tui/...`
Expected: PASS.

- [ ] **Step 7: Verify every build target**

```bash
make check && make wasm-check && make webui-build
```
Expected: all three succeed. `webui-build` proves the wasm bridge change compiles into the served module.

- [ ] **Step 8: Commit**

```bash
git add runner/protocol/ server/board_handler.go cli/ tui/board.go cmd/harness-webui-wasm/main.go webui/static/
git commit -m "$(cat <<'EOF'
feat(cli,tui,webui): the operator board view attributes each message to a runtime

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Close-out

- [ ] Run the full suite once: `go test ./...` (expect the known flaky `server` SessionMux `OpenInteractive` test to need at most one re-run).
- [ ] `make build` — landing on this project includes refreshing `bin/`.
- [ ] Report which spec problem-statement bullets the work covers and any left open.
