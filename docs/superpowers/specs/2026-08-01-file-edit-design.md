# File edit: pull it, change it, push it back

## Problem

The file surface can create a file and read a file. It cannot change one.

- **WebUI Files tab** (`webui/index.html:167-181`): New folder, New text file, Push,
  Pull, Pull dir, Preview, Delete. Changing one line of an existing file means
  Pull (a browser save dialog), open it in a local editor, Push it back, confirm
  the overwrite prompt. The middle step has no answer on a phone, which is where
  this UI is used.
- **TUI filepicker** (`tui/filepicker.go:264-360`): `u` push, `g` pull, `d` delete,
  `D` recursive delete. No create, no edit.
- **CLI / TUI cmdline** (`tui/cmdline.go:606`): `ls | push | pull | mkdir | delete`.

So `New text file` — WebUI only, `webui/static/main.js:1030` and `:3748` — is the
product's only writing surface, and it can only write a file that does not exist
yet. Reopening what it wrote is impossible from any surface.

The immediate use is notes in a task worktree, not source editing. That sets the
bar: write a memo, come back later, add a line, save. Syntax highlighting and
editor keybindings are not part of the bar; being able to reopen what you wrote
is.

Two consequences of that use that shape the design rather than decorate it:

- Files are small. Size limits exist to stop an accident (selecting `main.wasm`
  in the list), not to ration a resource.
- The neighbours of a memo in the same listing are binaries and images. Opening
  one of those in a text editor and saving it back must be impossible, not
  merely discouraged.

## What already exists

- `cli.Client.FilePullBytes` (`cli/file_pull.go:51`) and `FilePushBytes`
  (`cli/file_push.go:46`) — in-memory transfer with progress callbacks. Neither
  file carries a build tag, so native TUI/CLI can call them; today only the wasm
  bridge does (`cmd/harness-webui-wasm/main.go:1755`, `:1817`).
- `webui/index.html:243` `#file-editor-modal` — name input + textarea + Save,
  driven by `openFileEditor(namePrefill)` (`webui/static/main.js:3805`), which
  always opens with `textIn.value = ""`.
- `webui/static/main.js:3774` `pushBytesWithPrompts` — the shared push path with
  the `already_exists` and `not_found` interactive retries.
- `webui/static/main.js:1271` `renderFilePreview` — pulls bytes and dispatches to
  iframe / image / hex dump / text. Its text path decodes with
  `TextDecoder("utf-8", { fatal: false })` (`main.js:1299`), which is correct for
  viewing and wrong for editing: a lossy decode of a mistyped file would write
  U+FFFD back over the original bytes.
- `tui/popup.go` — the existing bubbles `textarea` popup. Its key contract lives
  in `tui/app.go:924-968`: "Submit popup intercepts ALL keys when open", `Esc`
  closes, `Ctrl+J` submits (bubbletea reports Ctrl+Enter as Ctrl+J on most
  terminals), everything else falls through to the textarea.
- `tui/httpform.go:126` — the multi-field form convention: `tab next field`.
- bubbles v1.0.0 `textarea.DefaultKeyMap` (`textarea/textarea.go:74-97`) binds
  `ctrl+a/b/d/e/f/h/k/n/p/t/u/v/w`. `Ctrl+E` is doubly spoken for: `LineEnd`
  there, and `popup.go`'s focus cycle at `tui/app.go:960`.

## Design

### Shared: `cli/file_edit.go` (new)

One implementation of the load/commit rules, used by all three surfaces —
natively by TUI and CLI, through the wasm bridge by the WebUI.

```go
const FileEditMaxBytes = 1 << 20 // 1 MiB, same value as WebUI PREVIEW_MAX_BYTES

type FileEditDoc struct {
    Rel  string // worktree-relative path the text came from
    Text string // LF-normalized, BOM-stripped; what an editor widget shows
    Orig []byte // exact bytes pulled; the conflict-detection baseline
    CRLF bool   // Orig used CRLF line endings
    BOM  bool   // Orig began with a UTF-8 BOM
}

type FileEditStatus int // FileEditPushed | FileEditUnchanged | FileEditConflict

var ErrFileEditTooLarge, ErrFileEditNotText, ErrNoExternalEditor error

func (c *Client) FileEditLoad(ctx context.Context, taskIDHex, rel string, onProgress ProgressFunc) (FileEditDoc, error)
func (d FileEditDoc) Encode(newText string) []byte
func (c *Client) FileEditCommit(ctx context.Context, taskIDHex string, d FileEditDoc, newText string, force bool) (FileEditStatus, error)
func ExternalEditorCommand(path string) (*exec.Cmd, error)
```

`FileEditLoad` pulls with `FilePullBytes`, then applies the byte-fidelity rules
below. `FileEditCommit` takes `force` rather than a confirm callback because the
three surfaces confirm differently (a TUI popup, a CLI stdin prompt, a browser
`confirm()`); the caller re-invokes with `force=true` after its own confirmation.

**Byte-fidelity rules** (authoritative; no surface re-implements them):

| Aspect | Rule |
|---|---|
| Size | `len(Orig) > FileEditMaxBytes` → `ErrFileEditTooLarge`. The WebUI additionally refuses before pulling, using the size it already has from `fileLs`. |
| Encoding | `utf8.Valid(Orig)` false → `ErrFileEditNotText`. Strict, unlike the preview path. |
| BOM | A leading `EF BB BF` is stripped from `Text`, recorded in `BOM`, and re-prepended by `Encode`. |
| Line endings | `CRLF` is true iff `Orig` contains at least one `\r\n` and no bare `\n`. `Text` always has `\r\n` collapsed to `\n`. `Encode` expands every `\n` back to `\r\n` when `CRLF`. |
| Trailing newline | Not synthesized and not stripped. Deleting the last newline in the editor deletes it on the runner. |

The line-ending rule has one deliberate lossy case: a file with *mixed* endings
gets `CRLF=false`, so its `\r\n` lines are saved as `\n`. Detecting and
reproducing per-line endings is not worth carrying for a memo editor; a
uniformly-CRLF file (what a Windows runner produces) round-trips exactly, which
is the case that matters.

**Conflict detection** in `FileEditCommit`, in order:

1. `next := d.Encode(newText)`; if `bytes.Equal(next, d.Orig)` → `FileEditUnchanged`,
   nothing is sent.
2. Re-pull `rel` with `FilePullBytes`. If it differs from `d.Orig` and `force` is
   false → `FileEditConflict`, nothing is sent.
3. `FilePushBytes(next, FilePushOpts{Force: true})` → `FileEditPushed`.

The re-pull is a second transfer of a file already bounded at 1 MiB, on a path
measured at ~20 MB/s native and ~2.8 MB/s wasm. For memo-sized files it is
noise, and it is the only thing standing between "the agent wrote to this file
while the editor was open" and a silent lost update.

A file that vanished between load and commit re-pulls as `not_found`. That is
returned as an error, not treated as "no conflict": the operator asked to edit a
file, and recreating it silently is a different act than editing it.

**`ExternalEditorCommand`** resolves `$VISUAL`, then `$EDITOR`, and returns
`ErrNoExternalEditor` when both are empty. It does not fall back to `vi` or
`notepad`. On this project's Windows client neither is safe: no terminal editor
is guaranteed on `PATH`, and `notepad` is a packaged app whose launcher process
may exit while the window is still open — waiting on it would return before the
operator saves and read as "unchanged". An error naming the two variables is a
better outcome than either fallback. The function returns an unstarted
`*exec.Cmd` because the TUI must run it under `tea.Exec` while the CLI runs it
directly.

### WASM bridge: `cmd/harness-webui-wasm/main.go`

Three exports, following the `harnessFilePullBytes` promise/`rejectFileErr` shape
already in that file:

```
harness.fileEditLoad(taskID, rel[, onProgress])
    -> Promise<{ text, orig: Uint8Array, crlf: bool, bom: bool }>
    rejects with code "too_large" | "not_text" | (runner file errors as today)

harness.fileEditCommit(taskID, rel, orig: Uint8Array, text, crlf, bom, force)
    -> Promise<{ status: "pushed" | "unchanged" | "conflict" }>

harness.fileEditEncode(text, crlf, bom) -> Uint8Array
    pure; no network. Applies the BOM/CRLF rules so the save-as path can hand
    bytes to the existing filePushBytes flow without reimplementing them in JS.
```

`orig` crosses the boundary back rather than being held in a Go-side handle map:
the exchange is stateless, a reload of the page cannot strand a handle, and the
copy is bounded by the same 1 MiB.

### WebUI: `webui/index.html`, `webui/static/main.js`

`openFileEditor` takes an options object instead of a name string:
`{ name, text, title, saveLabel }`. `New text file` passes empty text and today's
labels; edit passes the pulled text, the title `Edit file`, and `Save & push`.
`#file-editor-title` is set from `title` rather than being static markup.

Four entry points:

| Entry | Behaviour |
|---|---|
| `#file-edit-btn` in the Files action row | Enabled on the same condition as Preview (`main.js:898`): a task is selected, an entry is selected, and it is not a directory. |
| Double-click on a file row (`renderFileEntries`, `main.js:945`) | File rows only. Directory rows already descend on the first click, so they never see a second one. |
| `Edit` in the preview modal header | Shown only when the preview rendered as text — not for images, not for the hex dump, not for the oversize note. It re-loads through `fileEditLoad` rather than reusing the bytes the preview holds: a preview opened minutes ago is a stale baseline, and editing from it would report a conflict on the first save. |
| `file edit <task-id> <worktree-rel>` in the command input | Mirrors `file new` (`main.js:3671`, `:3748`). |

Save:

- **Path unchanged** → `fileEditCommit(..., force=false)`. `conflict` raises
  `confirm("<rel> は runner 側で変更されています。上書きしますか?")`; on OK,
  re-commit with `force=true`; on Cancel the modal stays open with the text
  intact. `unchanged` reports "変更なし" and closes without a push.
- **Path edited** → this is a new file at a new path, so there is no baseline to
  compare against. Encode through the loaded doc's CRLF/BOM flags and push via
  the existing `pushBytesWithPrompts`, which supplies the `already_exists` and
  `not_found` prompts.

Both paths end by refreshing the picker and writing the outcome to
`#file-result`, as every other Files action does.

### TUI: `tui/fileedit.go` (new)

A popup modelled on `tui/popup.go`, with key handling in `App.Update` next to
the submit-popup block that already owns this pattern (`tui/app.go:924`).

> **Amended after implementation.** The body started as a bubbles `textarea`
> and is now `tui/editbuf.go`, a windowed buffer. `textarea.View` re-renders
> the entire document every frame, which cost 38ms per keystroke on a 93KB
> Japanese memo (3ms at 1KB) and was visibly laggy on Windows. The replacement
> renders only the visible window: 30µs at 93KB. Two consequences worth
> carrying: it wraps by display cell rather than by word, and it has to do its
> own input sanitizing — dropping that is what let a control character reach a
> saved file and made it fail to reopen. One key changed with it: `Tab` types a
> tab (files require real ones) and field switching moved to `Shift+Tab`.

| Key | Action |
|---|---|
| `Ctrl+J` | Save & push (same binding, same reason, as the submit popup) |
| `Esc` | Cancel; the buffer is discarded |
| `Tab` | Insert a tab in the body (a text-entry key); in the single-line path field, move to the body |
| `Shift+Tab` | Move between the path field and the body |
| `Ctrl+O` | Open the buffer in `$EDITOR` |
| everything else | Delegated to the focused buffer / `textinput` |

`Ctrl+O` is chosen because no other TUI modal's key set binds it. `Ctrl+E`,
the mnemonic first choice, is taken twice over (see What already exists).

The `Ctrl+O` round trip: write the current buffer to a temp file preserving the
original extension (so an external editor picks its own highlighting), return
`tea.Exec` with `ExternalEditorCommand`, and on return read the temp file back
**into the buffer**. It does not push. A non-zero exit leaves the buffer
untouched and prints the exit status in the popup footer. Committing stays on
`Ctrl+J` alone.

That indirection is the point rather than an extra step. `tea.Exec` occupies the
bubbletea event loop synchronously — the constraint `tui/portforward.go:227` and
`tui/rawforward.go:776` already document and work around — so the TUI is frozen
and the terminal is released for as long as the editor runs. If a GUI editor's
launcher returns immediately while its window is still open, the read-back
produces the unchanged buffer and the operator sees it. The same event wired
straight to a push would silently push nothing and report success.

The freeze is announced on the terminal rather than left to look like a hang.
It cannot be announced by rendering it in the TUI: `Program.exec` calls
`ReleaseTerminal` **before** running the command (`bubbletea/exec.go:101-113`),
and that saves and leaves the alt screen (`bubbletea/tea.go:864-880`), so any
frame the popup painted is gone by the time the editor starts. What survives is
what the child writes to the primary screen — so the TUI supplies its own
`tea.ExecCommand` implementation (the interface at `bubbletea/exec.go:60`;
bubbletea hands it the terminal writer via `SetStdout(p.output)` just before
`Run()`) whose `Run` prints which editor is open, on which path, and that the
TUI returns when it exits, then runs the editor. A terminal editor paints over
that line immediately; a GUI editor leaves it on screen for as long as its
window is open, which is exactly where it is needed.

`$EDITOR` unset renders `ErrNoExternalEditor`'s message in the popup footer and
changes nothing else; the popup stays open and the built-in editor keeps
working. The external editor is an escape hatch, never a dependency.

Entry points: `e` (edit selected file) and `n` (new file in the current
directory) in the filepicker's browse mode — both free in its key set
(`tui/filepicker.go:264-360`) — plus `file edit` and `file new` in the cmdline.
`e` on a directory prints "use enter to descend", matching how `d` redirects to
`D`.

Loading and committing go through `DoFileEdit*` `tea.Cmd`s that thread
`a.client`, like every other `Do*` in `tui/file.go`. They never dial.

### CLI: `cmd/harness-cli`

```
harness-cli file edit <task-id> <worktree-rel-path>
harness-cli file new  <task-id> <worktree-rel-path>
```

Both use `ExternalEditorCommand` with stdio inherited — a CLI has no terminal UI
of its own to host an editor widget. `file edit` loads, spools to a temp file, runs the
editor, commits, and prints the status. A conflict prompts on stdin; declining
leaves the temp file in place and prints its path so the work is recoverable.
`$EDITOR` unset is an error naming both variables.

## Surface matrix

Per Pitfall 9, every operator entry point stated explicitly:

| Surface | Edit | New |
|---|---|---|
| CLI binary | `file edit` | `file new` |
| TUI keybindings | filepicker `e` | filepicker `n` |
| TUI cmdline | `file edit` | `file new` (new; `parseFile` gains both verbs) |
| TUI popups | `tui/fileedit.go` editor popup | same popup, empty buffer |
| WebUI buttons/forms | Files-tab button, row double-click, preview-modal button | existing `New text file` button |
| WebUI command input | `file edit` | `file new` (exists) |
| WASM bridge | `fileEditLoad` / `fileEditCommit` | `filePushBytes` (exists) |
| Shared cli | `cli/file_edit.go` | `FilePushBytes` (exists) |

No surface is intentionally omitted. `New` appears here because adding the TUI
editor popup makes TUI/CLI `file new` a few lines on top of the same widget, and
leaving create WebUI-only while edit is everywhere would be a gap with no
rationale behind it.

## Decisions taken

- **The name field stays editable in both modes.** Prefilled with the
  worktree-relative path on edit. Editing it means save-as, which is why the
  conflict check is skipped on that path.
- **The conflict check re-pulls rather than comparing metadata.** `fileLs`
  exposes name, size, mode and `isDir` (`cmd/harness-webui-wasm/main.go:1659`)
  and no mtime; a size comparison would miss same-length edits. Adding an mtime
  field to the listing to support this would be a wire change for a check that
  full bytes answer exactly.
- **The TUI edits in a built-in widget, not an external editor.** External is
  opt-in via `Ctrl+O`. Rationale in the TUI section above; the deciding factor is
  that the Windows client has no safe editor to fall back to.
- **No `vi` / `notepad` fallback anywhere.** An error that names `$EDITOR` and
  `$VISUAL` is better than dropping an operator into an editor they cannot exit,
  or into a launcher that reports success without saving.
- **The external-editor suspension is announced from inside the exec, not from
  the TUI frame.** The alt screen is already gone by then, so a status line in
  the popup would never be seen. A custom `tea.ExecCommand` writing to the
  terminal is the only place the message survives for the duration.
- **`FileEditCommit` takes `force bool`, not a confirm callback.** Three
  surfaces, three confirmation mechanisms, one of which is asynchronous
  JavaScript.
- **The WebUI calls into Go rather than reimplementing the rules in JS.** The
  BOM/CRLF/strict-decode/conflict rules are one implementation with one test
  suite. The JS keeps only the modal and the `confirm()`.
- **1 MiB limit, shared with the preview constant.** No second threshold to
  explain or keep in sync. For memo-sized files it is never reached; for a
  mis-selected binary the encoding check rejects it first anyway.

## Testing

`cli/file_edit_test.go`:

- CRLF round trip: a pure-CRLF document edited and re-encoded produces CRLF; a
  pure-LF one produces LF; a mixed one normalizes to LF (asserting the known
  lossy case, so it fails loudly if the rule is changed by accident).
- BOM round trip, including a BOM-only file.
- `utf8.Valid` rejection: invalid UTF-8 and a NUL-containing file both return
  `ErrFileEditNotText`, and no push is attempted.
- Size rejection at the boundary.
- `FileEditUnchanged` when the encoded text equals `Orig`, including the case
  where the editor round trip changed the in-memory `Text` but not the bytes.
- `FileEditConflict` when the re-pull differs, and the push-through when
  `force=true`.
- `ExternalEditorCommand`: `$VISUAL` preferred over `$EDITOR`; both unset →
  `ErrNoExternalEditor`.

`tui/fileedit_test.go` and `tui/cmdline_test.go`: popup key routing (`Ctrl+J`
commits, `Esc` discards, `Tab` moves focus, an unbound key reaches the body),
and `parseFile` accepting `edit` / `new` with their usages.

Build and vet gates, per `feedback_verify_with_make_targets_not_adhoc`:
`make check`, `make wasm-check`, `make vet`, `make test`.

End-to-end against a dummy server + runner from this checkout's own `bin/`, per
`feedback_dummy_server_runner_e2e_before_done`, driving one real create-then-edit
loop on each surface:

- CLI: `file new` then `file edit` with `EDITOR` set to a scripted non-interactive
  editor.
- TUI: real keystrokes into the popup — `e`, type, `Ctrl+J` — then re-open and
  confirm the change is on the runner. Rendering the popup is not the assertion;
  the file's bytes are (`feedback_verify_interactive_input_not_just_render`).
- WebUI: Playwright, at desktop width and at 390px. Button, double-click, and the
  preview-modal route each opened once, plus one conflict confirmation driven by
  changing the file from the CLI while the modal is open.
