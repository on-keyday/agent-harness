# board-render

A standalone, offline renderer for agentboard message records.

`harness-cli agent inbox --json` emits one JSON record per message, but a body
whose bytes are neither JSON nor UTF-8 arrives ONLY as `payload_b64` - a blob
a human (or a model) reading by eye cannot read and easily confabulates. This
tool reads the JSON Lines stream on stdin and writes a transcript a reader can
actually read: JSON is pretty-printed, UTF-8 is decoded, and a body that
genuinely cannot be text says so explicitly (with a hex dump) instead of ever
being shown as base64.

Usage:

    harness-cli agent inbox --json | python3 examples/board-render/board_render.py

Python 3, standard library only. Exit status is 1 if any input line was
malformed, 0 otherwise.

## Rendering rules

Output order = input order. Nothing is reordered.

Per record, a header line then the body:

    #<seq> <topic>  from=<agent>@<hostname> task=<first 8 hex of from.task_id>

- `  reply-to=<reply_to_topic>` is appended when that field is present, and
  nothing when it is absent.
- When `from.agent` is `""` (server-originated), the header prints
  `from=?@<hostname>`.
- When `in_reply_to != 0`, `  in-reply-to=#<n>` is appended.

Body selection, in this priority order:

1. `payload_omitted` true -> `<body omitted: <payload_bytes> bytes - run: <read_with>>`
2. `payload` present -> pretty-printed JSON, 2-space indent, key order preserved
3. `payload_text` present -> verbatim (except control characters, see below)
4. `payload_b64` only -> DECODE it. If the bytes are valid UTF-8, render the
   decoded text (except control characters, see below). If not, render
   `<non-text body: <N> bytes>` followed by a hex dump of the first 64 bytes
   (lowercase hex, space-separated, 16 bytes per line).
5. nothing at all / empty body -> `<empty body>`. An empty selected body -
   empty `payload_b64` (the form cli/agent/json_emit.go emits for a zero-byte
   payload), empty `payload_text`, or no body key at all - all render
   `<empty body>` rather than silence.

Every body line is indented by 4 spaces (empty body lines stay empty so no
trailing whitespace is emitted; a single trailing newline in a body is not a
line). A raw base64 blob is NEVER printed as the body.

## Terminal safety (control characters)

A body arrives from an untrusted peer, so raw control bytes NEVER reach
stdout: the rendered output contains no C0 control byte other than `\n` and
`\t`, and no `\x7f`, no matter what the input holds. Any other control
character in a body (or a header field) renders as a visible escape in the
form `\xNN` - for example the ANSI clear-screen sequence `ESC [ 2 J` renders
as the six literal characters `\x1b[2J`.

This applies to:

- the `payload_text` path and the decoded-`payload_b64` path;
- header fields interpolated from the record: `topic`, `from.hostname`,
  `from.agent`, the `from.task_id` prefix, and `reply_to_topic`.

The `payload` (JSON) path needs no extra handling: `json.dumps` escapes
control characters inside strings (e.g. `\u001b`), and a test pins that
assumption. `\n` and `\t` stay verbatim in body text so multi-line bodies and
tabs remain readable.

Reply nesting: if `in_reply_to` names a seq that appeared EARLIER in the same
stream, that record is indented by 2 extra spaces per level of depth (chains
nest further). If the parent seq is absent from the stream (or appears only
later), the record renders at depth 0 - the same "orphan" rule
`cli/tasktree.go` uses for tasks whose creator is absent.

Malformed input: a line that is not valid JSON is reported on STDERR as
`line <N>: not JSON: <first 80 chars>` and skipped; every other line still
renders. If any line was bad, exit status is 1; otherwise 0.
