package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// FileEditMaxBytes bounds what the edit surfaces will load into an editor
// widget. It is the same value as the WebUI's PREVIEW_MAX_BYTES so there is
// one threshold to explain: it exists to stop an accident (a binary picked
// out of the listing), not to ration a resource.
const FileEditMaxBytes = 1 << 20 // 1 MiB

var (
	// ErrFileEditTooLarge means the file exceeds FileEditMaxBytes. Pull it
	// instead.
	ErrFileEditTooLarge = errors.New("file edit: file is too large to edit")
	// ErrFileEditNotText means the bytes are not editable text: invalid
	// UTF-8, or containing a NUL. Deliberately strict — the preview path
	// decodes leniently because it only displays, while a lossy decode here
	// would push U+FFFD back over the original bytes.
	ErrFileEditNotText = errors.New("file edit: not editable text (needs valid UTF-8 without NUL)")
	// ErrNoExternalEditor means neither $VISUAL nor $EDITOR is set. There is
	// deliberately no vi / notepad fallback: on Windows no terminal editor is
	// guaranteed on PATH, and notepad is a packaged app whose launcher may
	// exit while its window is open, which would read as "unchanged".
	ErrNoExternalEditor = errors.New("file edit: no external editor (set $EDITOR or $VISUAL)")
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// FileEditDoc is a file pulled for editing: the text an editor widget shows,
// plus everything needed to put the original bytes back together and to tell
// whether the runner-side file moved underneath while it was open.
type FileEditDoc struct {
	Rel  string // worktree-relative path the text came from
	Text string // LF-normalized, BOM-stripped
	Orig []byte // exact bytes pulled; the conflict-detection baseline
	CRLF bool   // Orig used CRLF line endings
	BOM  bool   // Orig began with a UTF-8 BOM
}

// FileEditStatus is what a commit did. The zero value is deliberately not a
// real outcome so an error return cannot read as a success.
type FileEditStatus int

const (
	FileEditStatusInvalid FileEditStatus = iota
	FileEditPushed
	FileEditUnchanged
	FileEditConflict
)

func (s FileEditStatus) String() string {
	switch s {
	case FileEditPushed:
		return "pushed"
	case FileEditUnchanged:
		return "unchanged"
	case FileEditConflict:
		return "conflict"
	}
	return "invalid"
}

// newFileEditDoc applies the byte-fidelity rules to freshly pulled bytes.
// Split out from FileEditLoad so the rules are testable without a client.
func newFileEditDoc(rel string, orig []byte) (FileEditDoc, error) {
	if len(orig) > FileEditMaxBytes {
		return FileEditDoc{}, fmt.Errorf("%w (%d bytes, limit %d)", ErrFileEditTooLarge, len(orig), FileEditMaxBytes)
	}
	body := orig
	hasBOM := bytes.HasPrefix(body, utf8BOM)
	if hasBOM {
		body = body[len(utf8BOM):]
	}
	// Say WHICH rule failed. "not editable text" alone is fine for a binary
	// picked out of the listing by accident, but it is useless when a file
	// that IS text has picked up a stray byte — naming the offset is what
	// tells the operator it is repairable rather than a wrong selection.
	if i := bytes.IndexByte(body, 0); i >= 0 {
		return FileEditDoc{}, fmt.Errorf("%w: NUL byte at offset %d", ErrFileEditNotText, i)
	}
	if !utf8.Valid(body) {
		return FileEditDoc{}, fmt.Errorf("%w: invalid UTF-8", ErrFileEditNotText)
	}
	// CRLF only when every newline is one: a mixed file normalizes to LF,
	// which is lossy for its CRLF lines and is the documented trade.
	crlf := bytes.Contains(body, []byte("\r\n")) && !hasBareLF(body)
	return FileEditDoc{
		Rel:  rel,
		Text: string(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))),
		Orig: orig,
		CRLF: crlf,
		BOM:  hasBOM,
	}, nil
}

// hasBareLF reports whether b holds an "\n" not preceded by "\r".
func hasBareLF(b []byte) bool {
	for i := range b {
		if b[i] == '\n' && (i == 0 || b[i-1] != '\r') {
			return true
		}
	}
	return false
}

// Encode turns edited text back into the bytes to write, restoring the BOM
// and line endings the file had when it was loaded. Any CRLF the editor
// widget itself produced is collapsed first, so the doc's flag alone decides
// what goes on the wire.
func (d FileEditDoc) Encode(newText string) []byte {
	body := bytes.ReplaceAll([]byte(newText), []byte("\r\n"), []byte("\n"))
	if d.CRLF {
		body = bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n"))
	}
	if d.BOM {
		body = append(append([]byte{}, utf8BOM...), body...)
	}
	return body
}

// fileEditDecide is the commit decision with the transfers factored out:
// orig is what we loaded, next is what we would write, remote is what the
// runner holds now. Pure so the branch table is testable.
func fileEditDecide(orig, next, remote []byte, force bool) FileEditStatus {
	if bytes.Equal(next, orig) {
		return FileEditUnchanged
	}
	if !force && !bytes.Equal(remote, orig) {
		return FileEditConflict
	}
	return FileEditPushed
}

// FileEditLoad pulls rel out of taskIDHex's worktree and returns it in
// editable form. Errors with ErrFileEditTooLarge / ErrFileEditNotText when
// the file is not something an editor should open.
func (c *Client) FileEditLoad(ctx context.Context, taskIDHex, rel string, onProgress ProgressFunc) (FileEditDoc, error) {
	data, err := c.FilePullBytes(ctx, taskIDHex, rel, onProgress)
	if err != nil {
		return FileEditDoc{}, err
	}
	return newFileEditDoc(rel, data)
}

// FileEditCommit writes newText back to d.Rel. It sends nothing when the
// bytes are unchanged, and nothing when the runner-side file moved since
// FileEditLoad unless force is set — the caller confirms that with whatever
// its surface uses and calls again with force=true.
//
// A file that vanished between load and commit surfaces as an error rather
// than as "no conflict": the operator asked to edit a file, and recreating
// one is a different act.
func (c *Client) FileEditCommit(ctx context.Context, taskIDHex string, d FileEditDoc, newText string, force bool) (FileEditStatus, error) {
	next := d.Encode(newText)
	var remote []byte
	if !bytes.Equal(next, d.Orig) && !force {
		var err error
		remote, err = c.FilePullBytes(ctx, taskIDHex, d.Rel, nil)
		if err != nil {
			return FileEditStatusInvalid, fmt.Errorf("file edit: re-read %s before overwriting: %w", d.Rel, err)
		}
	}
	switch st := fileEditDecide(d.Orig, next, remote, force); st {
	case FileEditUnchanged, FileEditConflict:
		return st, nil
	}
	if err := c.FilePushBytes(ctx, taskIDHex, next, d.Rel, FilePushOpts{Force: true}, nil); err != nil {
		return FileEditStatusInvalid, err
	}
	return FileEditPushed, nil
}

// ExternalEditorCommand builds (but does not start) the editor process for
// path. The command is returned unstarted because the TUI must run it under
// tea.Exec while the CLI runs it directly.
//
// $EDITOR may carry flags ("code -w", "emacsclient -nw"), so it is split on
// whitespace. That is a simplification against a program path containing
// spaces; the fix for such a case is a wrapper script, and this project's
// Windows client is expected to hit ErrNoExternalEditor rather than this
// path at all.
func ExternalEditorCommand(path string) (*exec.Cmd, error) {
	name := os.Getenv("VISUAL")
	if strings.TrimSpace(name) == "" {
		name = os.Getenv("EDITOR")
	}
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return nil, ErrNoExternalEditor
	}
	return exec.Command(fields[0], append(fields[1:], path)...), nil
}

// NewFileEditDocForTest exposes the load rules to tests in other packages —
// the TUI's edit/save/reopen cycle test needs to assert that bytes this
// editor produced are accepted again, without standing up a transfer.
func NewFileEditDocForTest(rel string, orig []byte) (FileEditDoc, error) {
	return newFileEditDoc(rel, orig)
}
