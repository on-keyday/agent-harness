# harness-tui — Bubble Tea Frontend Design

Status: draft, pending user review
Date: 2026-04-25

## 1. Goal

`harness-cli` の主要操作 (submit / ls / logs / cancel / prune / watch) を 1 画面で完結する TUI。`cmd/harness-tui` 単独バイナリ。Bubble Tea + bubbles + lipgloss で実装。`harness-cli` は script 用途で残す。

リファレンス手癖: `https://github.com/on-keyday/ncdn/blob/seccamp/controller/cmd/controller/main.go`。レイアウトの「上=テーブル群、下=log、最下段=command line + result」基本形を踏襲しつつ、テーブル行選択をきちんと有効化する。

## 2. Non-goals (v1)

- 複数 server / 複数プロファイルの切替
- prompt 履歴 (readline 風) の保存・補完
- log の検索・フィルタ
- 永続的な user 設定ファイル
- マウス対応
- `prune` の dry-run モード
- bubbletea 公式 teatest ベースの自動 UI test

## 3. Architecture

```
┌────────────────────────────────── tea.Program ─────────────────────────────────┐
│  harness-tui · ws://localhost:8539 · CONNECTED                  (header: 1 line) │
│                                                                                  │
│   ┌───────────────────────┐  ┌──────────────────────────────────────────────┐  │
│   │ Runners table         │  │ Tasks table                                   │  │
│   │ Idle/Busy + repo      │  │ status / id-prefix / repo / prompt           │  │
│   └───────────────────────┘  └──────────────────────────────────────────────┘  │
│                                                                                  │
│   ┌──────────────────────────────────────────────────────────────────────────┐  │
│   │ Log viewport — follows the selected task (task.<id>.log)                  │  │
│   └──────────────────────────────────────────────────────────────────────────┘  │
│                                                                                  │
│   ┌─ cmdresult (last cmd output) ────────────────────────────────────────────┐  │
│   │ submit /repo "prompt"                                                     │  │
│   │ → 9d50...                                                                 │  │
│   └──────────────────────────────────────────────────────────────────────────┘  │
│   > [cmdline textinput]                                                          │
│   (s submit · c cancel · tab focus · ? help · q quit)                           │
│                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────┘
```

Submit popup (overlay on `s`):
```
┌─ New task ─────────────────────────────────────────────────────────────┐
│ Repo: /home/kforfk/workspace/remote-agent-harness  (autodetected from cwd) │
│                                                                         │
│ Prompt:                                                                  │
│ ┌──────────────────────────────────────────────────────────────────────┐ │
│ │ <multiline textarea>                                                  │ │
│ │                                                                       │ │
│ └──────────────────────────────────────────────────────────────────────┘ │
│                                                                         │
│ Ctrl+Enter: submit  ·  Esc: cancel                                       │
└─────────────────────────────────────────────────────────────────────────┘
```

### 3.1 Data flow

```
        ┌─ initial load ──────────────────┐
        │  cli.Dial → cli.List()           │
        │  → snapshotMsg                   │
        └──────────────┬───────────────────┘
                       ▼
         ┌─ tea.Program (Update) ─────┐
   key   │  - WindowSizeMsg → resize  │  view
   ──▶  │  - pubsub event msgs       │  ──▶ rendered string
         │  - cli action result msgs  │
         └──┬──────────────────────┬──┘
            │                      │
            ▼                      ▼
   goroutine A:                   goroutine B:
   - JOIN tasks.status            - one per cli action invocation
   - JOIN runners.status          - cli.Submit / Cancel / PruneTasks
   - JOIN task.<sel>.log          - dispatches resultMsg back via Send
   - dispatch event msgs via
     program.Send
```

### 3.2 Component / file structure

```
cmd/harness-tui/main.go    Entry: flags, server dial, tea.Program startup
tui/
  app.go                    Top-level Model (Init/Update/View), focus router
  runners.go                Runners table sub-model
  tasks.go                  Tasks table sub-model
  logs.go                   Log viewport sub-model + topic JOIN/LEAVE 切替
  cmdline.go                Command input + parser (shlex)
  cmdresult.go              Viewport for last-command output
  popup.go                  Submit multiline overlay (textarea)
  events.go                 pubsub → tea.Msg bridge (goroutines + program.Send)
  styles.go                 lipgloss style consts (panel borders, focus highlight)
  client.go                 Thin wrapper exposing cli.Dial / Submit / Cancel etc.
                            as tea.Cmd factories returning result msgs.
```

Each file has one responsibility. `app.go` is the only place that knows about all sub-models and orchestrates focus + key dispatch.

### 3.3 New dependencies

Added to `go.mod`:

| Module | Purpose |
|---|---|
| `github.com/charmbracelet/bubbletea` | TEA runtime |
| `github.com/charmbracelet/bubbles` | table / viewport / textinput / textarea |
| `github.com/charmbracelet/lipgloss` | styling |
| `github.com/google/shlex` | command-line argv tokenization |

These dependencies live only inside `cmd/harness-tui` and the `tui/` package; the existing CLI / server / runner / pubsub / objproto packages are not affected.

## 4. UX details

### 4.1 Layout

| Region | Size hint | Content |
|---|---|---|
| Header   | width, height 1 | `harness-tui · <addr> · CONNECTED|DISCONNECTED` |
| Top-left half  | width/2, height ~10 | Runners table |
| Top-right half | width/2, height ~10 | Tasks table |
| Middle (full)  | width, height = max(5, term - 22) | Log viewport for selected task |
| Below middle   | width, height 5     | cmdresult (last command output) |
| Bottom         | width, height 1     | cmdline textinput |
| Footer         | width, height 1     | hotkey hints |

(`term - 22` = total terminal height minus 1 header + 10 top tables + 5 cmdresult + 1 cmdline + 1 footer + 4 panel-border rows.)

Panels have lipgloss borders; the focused panel uses an accent color (defined in `tui/styles.go`).

### 4.2 Keybindings

**Global**

| Key | Action |
|---|---|
| `Tab` / `Shift+Tab` | Focus cycle: runners → tasks → logs → cmdline → runners |
| `s` | Open submit popup |
| `?` | Open help overlay |
| `Ctrl+C` | Quit |
| `q` | Quit (only when cmdline / popup is NOT focused) |

**Tasks-table focus**

| Key | Action |
|---|---|
| `↑`/`↓` or `j`/`k` | Row move |
| `Enter` | Follow log of the selected task in the log viewport |
| `c` | Cancel the selected task |
| `y` | Echo the selected task ID into cmdresult (copy aid) |

**Runners-table focus**

| Key | Action |
|---|---|
| `↑`/`↓` or `j`/`k` | Row move (informational only) |

**cmdline focus**

| Key | Action |
|---|---|
| `Enter` | Run command, output to cmdresult, clear input |
| `Esc` | Drop focus back to last table |

**Submit popup**

| Key | Action |
|---|---|
| `Ctrl+Enter` | Submit the prompt |
| `Esc` | Cancel and close popup |

### 4.3 cmdline commands

| Cmd | Equivalent |
|---|---|
| `submit <prompt>` | `cli.Submit(ctx, addr, cwd, prompt)` |
| `cancel <id-prefix>` | `cli.Cancel(ctx, addr, fullID)` if exactly one task matches the prefix; otherwise refuse |
| `prune --before=<dur>` | `cli.Prune(ctx, addr, cwd, dur, w)` |
| `prune --offline --before=<dur>` | `cli.Prune(ctx, "", cwd, dur, w)` |
| `clear` | clear cmdresult |
| `help` | open help overlay |
| `quit` / `exit` | `tea.Quit` |

**Parsing pipeline** (two stages):

1. Tokenization: `github.com/google/shlex.Split` → `[]string` argv with quoting / escapes handled.
2. Subcommand + flag parse: stdlib `flag.NewFlagSet` per subcommand (`flag.ContinueOnError`, `SetOutput(io.Discard)` so errors come back as Go errors instead of being printed). This matches the convention already used in `cmd/harness-cli/main.go`.

The parsed result is converted into a small action struct (`submitAction{repo, prompt}` / `cancelAction{idPrefix}` / `pruneAction{before, offline}` / `clearAction{}` / `quitAction{}` / `helpAction{}`). The action type is a `tea.Cmd` factory consumed by `app.go`.

## 5. Real-time updates

- Initial state: `cli.List()` once on connect.
- pubsub topic JOINs at startup:
  - `tasks.status` → `TaskStatusEvent` decode → row patch in tasks table
  - `runners.status` → `RunnerStatusEvent` decode → row patch in runners table
- Log viewport: `task.<id>.log` JOINed lazily when the user focuses a task; LEAVE on focus change.
- Goroutines bridge pubsub byte streams to `tea.Msg` via `program.Send(...)`.
- One persistent `objproto.Connection` (with `trsf.AutoPing` keep-alive) for the lifetime of the TUI.

Reconnection: v1 detects loss only via cmdline-level RPC failures (each round-trip dials its own conn, so its error message lands in cmdresult). If the persistent pubsub conn drops silently, events stop arriving but the header keeps showing CONNECTED. Manual restart needed. Auto-reconnect is a v2 item — see §9 open questions.

## 6. Error handling

| Failure | UI behavior |
|---|---|
| Initial Dial fails | Print error to stderr, exit non-zero |
| Mid-run conn drop | v1: silent — header keeps showing CONNECTED, events stop. RPC errors during cmdline ops surface in cmdresult. Manual TUI restart. (Auto-reconnect deferred to v2.) |
| Submit / Cancel / Prune RPC error | Error string shown in cmdresult, no state change |
| Pubsub stream EOF (server LEAVE) | Try to JOIN again; log a single line in cmdresult |
| Decode failure on event payload | Drop event, log to cmdresult footer "warn: decode error" |
| Window too small (< 80×24) | Render a single line "terminal too small" until resized |

## 7. Configuration

CLI flags on `harness-tui`:

| Flag | Default | Purpose |
|---|---|---|
| `--server` | `localhost:8539` | `host:port` of harness-server |
| `--repo` | `.` (cwd) | submit popup's default repo path |

No config file in v1. Future: `~/.config/harness/tui.toml` can override defaults.

## 8. Open questions

None at design time. UX details (keybind ergonomics, color choices, popup width) will be tuned after the user runs the binary against the live server. The keybinds chosen here are an opinionated starting point.

## 9. Out of scope (potential v2)

- **Auto-reconnect** of the persistent pubsub connection (v1 needs a manual restart on drop)
- Multi-server support (switch profile via header)
- Persistent user config + history
- Log search (`/`-mode within viewport)
- Mouse / scroll-wheel
- Submit form: repo picker, advanced options (timeout override, etc.)
- Themes (light / dark / high-contrast)
- Diff viewer pane: when a task finishes, show its `git diff` against base in a side pane

## 10. Testing strategy

| Layer | Approach |
|---|---|
| `tui/cmdline.go` parser | Unit tests for `submit`/`cancel`/`prune`/`quit`/error cases |
| `tui/events.go` decode helpers | Unit tests with hand-rolled bgn payloads |
| Whole-app rendering | Manual smoke (run server + runner + tui, verify layout / shortcuts work) |
| pubsub bridge | Smoke via integration: submit a task, watch the row appear in <1s |

bubbletea's `teatest` is available for golden-file rendering tests but is deliberately out of scope for v1.

## 11. Implementation order (hint for planning)

1. New deps: `go.mod` add bubbletea, bubbles, lipgloss, shlex
2. `tui/styles.go` — color / border consts
3. `tui/cmdline.go` + parser unit tests
4. `tui/runners.go` + `tui/tasks.go` — static tables fed from a fake snapshot
5. `tui/app.go` skeleton — focus rotation, render of all panels, no live data yet
6. `tui/client.go` + `tui/events.go` — pubsub bridge, cli wrappers
7. Wire initial `cli.List()` and pubsub events into table updates
8. `tui/logs.go` — focus-driven log viewport
9. `tui/popup.go` — submit popup, plumbed through to `cli.Submit`
10. cmdline executions: `cancel`, `prune`, `clear`, `quit`, `help`
11. Disconnection handling + reconnect ticker
12. README update with screenshots / asciicast link
