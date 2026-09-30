# Stream sessions: answering AskUserQuestion

Date: 2026-09-30

**Status: IMPLEMENTED.** Every design point below is marked DECIDED,
including the one taken with the operator. §Implementation notes at the end
records the deviations and the live verification.

Builds on [`2026-08-20-event-stream-agent-design.md`](2026-08-20-event-stream-agent-design.md)
(the stream kind, its adapter, the neutral protocol, and the approval path).

## Problem

A stream-kind task runs claude under `--permission-prompt-tool stdio`, so when
claude calls `AskUserQuestion` the call reaches the harness as an ordinary
`can_use_tool` request. Every surface renders a request the same way, as a
tool approval, with allow and deny as the only choices:

- the TUI chat: `a` / `d` / esc, plus the suggestion digits
- the WebUI chat's approval control
- `session stream approve --allow|--deny`

So the operator has no way to pick an option. Observed 2026-09-30: the
operator was driving a stream task from the TUI when claude asked a question,
and could not answer it. The two outcomes available are both wrong:

1. **allow answers nothing.** The adapter echoes the original input on an
   allow, and it keeps no original input, so it sends `updatedInput: {}`
   (`claudeAdapter.originalInput` is a placeholder). The tool then reports
   that the user did not answer.
2. **deny refuses the tool.** The agent reads that as the user declining to
   be asked at all.

The agent then has to guess, or ask again in plain text. That second case is
what an agent in this repo now does on purpose
(`feedback_no_askuserquestion_in_stream_kind`), and that workaround is what
this design retires.

## What the vendor documents

From code.claude.com/docs/en/agent-sdk/user-input:

- `AskUserQuestion` triggers `canUseTool` with the tool name
  `"AskUserQuestion"`. Its input is
  `{questions: [{question, header, options: [{label, description}],
  multiSelect}]}`: 1–4 questions, each with 2–4 options.
- The host answers with `allow`, and `updatedInput` set to `{questions: <the
  original array>, answers: {<question text>: <label>}}`.
  - A multi-select answer is an array of labels, or the labels joined with
    `", "`.
  - For free text (an "Other"), the value is the user's text itself.
- An optional `response` holds a freeform reply that answers no specific
  question. When it is set, Claude receives "The user responded: …" instead
  of the answer list.
- `deny` blocks the tool, and Claude sees the message.
- The callback may stay pending indefinitely. That is what §4 of the base
  spec already assumes for approvals.

## Design

### 1. The adapter translates the question; the protocol carries it neutrally — DECIDED

`AskUserQuestion` is a claude tool name with a claude input shape. Recognising
it in the runner or in a client would put vendor knowledge on the harness side
of the seam, which the base spec's §2 exists to prevent. So the adapter does
it, and the neutral protocol gains the shape.

**On `Request`**, an additive field `Questions []Question`
(`json:"questions,omitempty"`):

```
Question { Question string; Header string; Options []Option; MultiSelect bool }
Option   { Label string; Description string }
```

- A request whose `Questions` is non-empty is a question to answer, not a
  tool to approve.
- The adapter fills `Questions` when `tool_name == "AskUserQuestion"` and the
  input decodes to that shape.
- When the input does not decode, the adapter leaves `Questions` empty. The
  request then falls back to being an ordinary approval, which is today's
  behaviour, rather than a question the harness cannot render.
- `Tool`, `Input` and the rest stay as they are, so a client that does not
  know the field sees exactly today's approval.

**On `Response`**, two additive fields:

- `Answers map[string][]string` (`json:"answers,omitempty"`). The key is a
  question's text or its header, and the value is the chosen labels. Free
  text goes in as the value itself. A single-select answer is a one-element
  list.
- `Reply string` (`json:"reply,omitempty"`), the vendor's `response`.

**Versioning:** additive, `ProtocolVersion` stays 1. This is the rule written
on `ProtocolVersion` (2026-09-30). An old client reads the request as a plain
approval, which is safe: it is exactly what happens today. An old adapter
ignores the two response fields, and the result is today's empty allow.

### 2. The adapter builds the vendor answer, and is the one place that validates it — DECIDED

In `runner/streamagent/claude.go`:

- **Keep the request's input.** `pending` maps our request id to the
  vendor's id, and now also to the request's `Input`. That retires the
  `originalInput` placeholder for every tool: an allow with no
  `UpdatedInput` echoes the real input rather than `{}`. The wiring log
  measured that `{}` happened to work for Write. It cannot work for
  `AskUserQuestion`, whose answer is built from that input.
- **On a response carrying `Answers` or `Reply`** to a request that has
  questions:
  - Resolve each `Answers` key to a question, by exact question text first
    and then by header.
  - A key that matches no question is refused, and the request stays
    pending. This is the same refusal an unknown request id gets today: an
    answer aimed at something that is not there must not be applied to
    something that is.
  - Build `updatedInput` as `{questions: <original>, answers: {<question
    text>: <value>}}`. The value is the single label or text for a
    single-select question, and the array for a multi-select one. Add
    `response: Reply` when `Reply` is set.
  - Send it as an allow.
  - A question left unanswered is allowed. The vendor's own shape permits a
    partial answer, and the chat UIs are what require every question before
    they enable send (§3).
- **A response carrying `Answers` or `Reply` to a request with no questions**
  is refused as malformed: those fields have no meaning on a tool approval.
- **Deny is unchanged.** `Message` is optional and reaches the agent
  verbatim.

Validation lives HERE and nowhere else. The CLI and the bridge pass keys as
typed, so every surface gets the same resolution and the same refusal.

### 3. Surfaces — DECIDED

| Surface | Change |
|---|---|
| **TUI chat** (`tui/chat.go`) | A pending request with questions renders as a question block instead of the tool-input dump. For each question it shows the header, the question text and numbered options with descriptions, and a `[x]` / `( )` mark. Keys: `tab` / `shift+tab` moves between questions. `1`–`9` picks an option: it toggles on a multi-select question and replaces the choice on a single-select one. `o` opens the existing one-line editor (the deny-reason editor's machinery) for free text on the current question. `r` opens it for a freeform `Reply`. `enter` sends, once every question has a choice or text, or a Reply is set. `d` denies, with an optional reason as today. `esc` leaves. The suggestion digits do not apply while a question block is shown; a question request carries none. |
| **WebUI chat** (`#chat-approval`) | For each question, the options as radios for single-select and checkboxes for multi-select, plus an "Other" text input. These are the controls the page already uses elsewhere (surface-parity item 34a). Then a "自由回答" textarea for `Reply`, 送信 (disabled until the same rule as the TUI holds) and 拒否. |
| **wasm bridge** | `harness.streamAnswer(taskID, requestID, answers, reply)`: `answers` is an object of key → array of strings, and the bridge builds the `Response` in Go. The page assembles no protocol message itself. |
| **CLI / TUI cmdline / WebUI cmdline** | A new verb `session stream answer <task-id> <request-id> [--answer KEY=VALUE]... [--reply TEXT]`, declared once in `cli/verb/table.go` so all three command lines parse it. `--answer` repeats. Two `--answer`s with the same KEY add a second label, for a multi-select question. KEY is the question text or its header. At least one of `--answer` / `--reply` is required, via an `AtLeastOne` rule. |
| **task log** (`RenderText`) | A request with questions renders as `❓ question: <first question's header>: <first question> (+N more) (<id>)` instead of `⏸ approval needed: AskUserQuestion (<id>)`. The runner tap's response line becomes `▶ <id>: answered` for an answer. |
| **`pending=N`** | Unchanged: a question is a pending request. |
| **`session stream attach`** | Nothing beyond `RenderText`. |

The line text of the question block, and each option line, come from ONE Go
renderer in `streamagent` (a `QuestionLines(Request)` next to `DisplayOf`).
The TUI draws it and the WebUI gets it through the bridge, the same rule
`DisplayOf` follows. The WebUI builds only the controls; it words nothing.

### 4. What this does not change

- A tool approval is unchanged in every surface.
- `defer` (the vendor's third option for a slow human) is still not used. A
  question blocks, like an approval.
- `toolConfig.askUserQuestion.previewFormat` is not requested, so options
  carry no `preview`. It is an SDK option with no CLI flag that this design
  knows of. If it is ever set, `Option` gains the field additively.

## Tests

- **Adapter**, against the fake agent:
  - An `AskUserQuestion` `control_request` becomes a Request whose
    `Questions` mirror the input.
  - An answer by question text, by header, multi-select, free text, and
    Reply each produces the documented `updatedInput`.
  - An unknown key is refused and the request stays pending.
  - `Answers` on a non-question request is refused.
  - Malformed question input falls back to a plain approval.
  - An allow with no `UpdatedInput` now echoes the ORIGINAL input. This is
    the regression test for the placeholder.
- **cli/verb:** the `answer` row's rules (`AtLeastOne`, repeated
  `--answer`), and a surface verdict for all three command lines.
- **TUI:** the question block renders from `QuestionLines`. The keys select,
  toggle, go to free text, and send, and send stays disabled until every
  question is answered. The built `Response` is checked through the real
  `EncodeStreamMsg`.
- **End to end, once, against real claude:** a stream session prompted to ask
  a two-question `AskUserQuestion`, one of them multi-select, answered:
  - from the TUI chat under `script(1)`
  - from the WebUI chat
  - with `session stream answer`

  Each time, the agent's next turn must quote the chosen labels back. That
  is what proves the answer reached the model and not just the wire.

## Decisions taken with the operator

1. **The TUI keys — DECIDED (operator, 2026-09-30):**
   - `tab` / `shift+tab`: move between questions
   - digits: pick an option
   - `o`: free text for the current question
   - `r`: freeform reply
   - `enter`: send
   - `d`: deny

   `r` means reattach/resume on the main screen, but it is free inside the
   chat overlay, and the operator kept it.

## Depends on

- **`resolved`** (`2026-08-20-event-stream-agent-design.md` §Amendment
  2026-09-30c, landed): an answered question has to end on the stream. Without
  it a reattach would ask the operator the same question again. The adapter's
  `Resolved` line for an answer carries `behavior: allow`.

## Implementation notes

- **Where the code lives.**
  - The neutral types are `Request.Questions`, `Question`, `Option`, and
    `Response.Answers` / `Reply` (`runner/streamagent/proto.go`).
  - The adapter side is `questionsOf`, `resolveAnswers` and `answeredInput`,
    plus the pending table, which is now a struct (`pendingRequest`) holding
    the vendor id, the input, the questions and the raw suggestions.
  - The shared helpers are `QuestionSummary`, `QuestionComplete` and
    `AnswerResponse` (`display.go`), and `cli.ParseAnswers` /
    `Client.StreamAnswer`.
  - The wasm bridge gained `streamAnswer`, `questionSummary` and
    `questionComplete`.
- **Deviation from §3's single renderer.** The TUI draws the option rows
  itself, because the selection marks are its own state. The WebUI draws the
  options from the request's structure as controls. What IS shared through Go
  is:
  - the notice line (`QuestionSummary`, which the WebUI reaches over the
    bridge);
  - the rule that enables send (`QuestionComplete`, same);
  - the answer builder;
  - the `KEY=VALUE` grammar.
  A shared "option line" renderer would have had no second consumer.
- **Found and fixed on the way: accepting a suggestion did nothing.**
  - `Response.AcceptSuggestion` reached the adapter from the TUI digits,
    `approve --suggestion` and the WebUI's ＋ buttons.
  - The adapter never read it, so "stop asking" was a plain allow.
  - Now the adapter keeps each request's `permission_suggestions` verbatim
    and echoes the chosen one back in `updatedPermissions`. That is the
    documented "Approve and remember" shape.
  - Pinned by `TestAnAcceptedSuggestionIsEchoedAsUpdatedPermissions`, whose
    negative control goes red.
- **Also retired: the `originalInput` placeholder.** A plain allow now echoes
  the request's real input. It used to echo `{}`.
- **Verified live** (dummy harness, Opus 5.5, 2026-09-30). The agent was
  prompted to call `AskUserQuestion` with a single-select Color question and
  a multi-select Size question, and each surface answered one round:
  - **CLI**: `session stream answer … --answer Color=Blue --answer Size=Small
    --answer Size=Huge`. The tool result read `"…color…"="Blue",
    "…sizes…"="Small,Huge"`, and the agent quoted both choices back.
  - **TUI chat**: `1`, `tab`, `2`, `3`, `enter`. The block showed `(•) Red`
    and `[x] Large` / `[x] Huge`. The agent replied "Red" and "Large,Huge".
  - **WebUI chat**: real clicks on Blue and Small, and "Tiny" typed into
    Size's その他. 送信 stayed disabled until both questions were answered.
    The agent replied "Blue" and "Small,Tiny", and noted Tiny as a custom
    answer.

