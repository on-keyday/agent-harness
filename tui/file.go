package tui

import (
	"bytes"
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

// FileResultMsg is delivered to App.Update after a file op completes.
// Op is a short verb ("ls", "push", "pull", "mkdir", "delete") used to
// compose the cmdresult line. Output is non-empty on `ls` success (the
// listing to render); Err is non-nil on failure for any verb. Both
// empty indicates a silent success for a write op (`push`, `pull`,
// `mkdir`, `delete`); the dispatch handler renders an "ok: ..." line
// in that case.
type FileResultMsg struct {
	Op     string
	TaskID string
	Detail string // for ls: rel path; for push/pull/delete: a short summary
	Output string
	Err    error
}

// DoFileLs lists a directory under the task's worktree. Captures the
// runner's listing into a buffer (the cli method writes to an
// io.Writer) and delivers it via FileResultMsg.Output.
func DoFileLs(c *cli.Client, taskID, relPath string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var buf bytes.Buffer
		err := c.FileLs(ctx, taskID, relPath, &buf)
		return FileResultMsg{
			Op:     "ls",
			TaskID: taskID,
			Detail: relPath,
			Output: buf.String(),
			Err:    err,
		}
	}
}

// DoFilePush copies a local source into the task's worktree. The
// recursive variant uses dir_push (tar over the wire); the
// non-recursive variant uses the single-file push path with optional
// force overwrite. Parents creates missing parent directories of
// remoteDst before the push (mkdir -p semantics).
func DoFilePush(c *cli.Client, taskID, localSrc, remoteDst string, recursive, force, parents bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		opts := cli.FilePushOpts{Force: force, MkdirParents: parents}
		var err error
		if recursive {
			err = c.FilePushDir(ctx, taskID, localSrc, remoteDst, opts)
		} else {
			err = c.FilePush(ctx, taskID, localSrc, remoteDst, opts)
		}
		return FileResultMsg{
			Op:     "push",
			TaskID: taskID,
			Detail: fmt.Sprintf("%s -> %s", localSrc, remoteDst),
			Err:    err,
		}
	}
}

// DoFileMkdir creates a directory under the task's worktree. parents
// mirrors mkdir -p (create missing parents, existing dir is ok);
// without it the runner is strict.
func DoFileMkdir(c *cli.Client, taskID, relPath string, parents bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := c.FileMkdir(ctx, taskID, relPath, parents)
		label := relPath
		if parents {
			label += " (-p)"
		}
		return FileResultMsg{
			Op:     "mkdir",
			TaskID: taskID,
			Detail: label,
			Err:    err,
		}
	}
}

// DoFilePull copies from the task's worktree to a local destination.
// The recursive variant uses dir_pull (tar over the wire); the
// non-recursive variant uses the single-file pull path with optional
// force overwrite of the local destination.
func DoFilePull(c *cli.Client, taskID, remoteSrc, localDst string, recursive, force bool, rng cli.FileTransferRange) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var err error
		if recursive {
			err = c.FilePullDir(ctx, taskID, remoteSrc, localDst, force)
		} else {
			err = c.FilePull(ctx, taskID, remoteSrc, localDst, rng, force)
		}
		return FileResultMsg{
			Op:     "pull",
			TaskID: taskID,
			Detail: fmt.Sprintf("%s -> %s", remoteSrc, localDst),
			Err:    err,
		}
	}
}

// DoFileDelete removes a path from the task's worktree. The recursive
// variant uses dir_delete; force on recursive makes it equivalent to
// os.RemoveAll, otherwise the runner only removes empty directories.
// Force without recursive is no-op (single-file delete has no force
// semantics) but accepted to keep the flag set uniform.
func DoFileDelete(c *cli.Client, taskID, relPath string, recursive, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		var label string
		if recursive {
			err = c.FileDeleteDir(ctx, taskID, relPath, force)
			label = relPath + " (recursive"
			if force {
				label += ", force"
			}
			label += ")"
		} else {
			err = c.FileDelete(ctx, taskID, relPath)
			label = relPath
		}
		return FileResultMsg{
			Op:     "delete",
			TaskID: taskID,
			Detail: label,
			Err:    err,
		}
	}
}

// FileEditLoadedMsg carries a pulled file back to App, which opens the
// editor popup on it.
type FileEditLoadedMsg struct {
	TaskID string
	Rel    string
	Doc    cli.FileEditDoc
	Err    error
}

// FileEditCommittedMsg carries a commit outcome back to App.
type FileEditCommittedMsg struct {
	Rel    string
	Status cli.FileEditStatus
	Err    error
}

// DoFileEditLoad pulls a file for editing. Threads a.client like every other
// Do* here — it never dials.
func DoFileEditLoad(c *cli.Client, taskID, rel string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		doc, err := c.FileEditLoad(ctx, taskID, rel, nil)
		return FileEditLoadedMsg{TaskID: taskID, Rel: rel, Doc: doc, Err: err}
	}
}

// DoFileEditCommit writes an edited buffer back to the file it came from,
// re-reading the runner-side file first unless force is set.
func DoFileEditCommit(c *cli.Client, taskID string, doc cli.FileEditDoc, text string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		st, err := c.FileEditCommit(ctx, taskID, doc, text, force)
		return FileEditCommittedMsg{Rel: doc.Rel, Status: st, Err: err}
	}
}

// DoFileEditCreate writes a buffer to a path that has no baseline: a new
// file, or an edit retargeted to a different path (save-as). Force stays off
// so an accidental collision is reported rather than silently overwritten;
// parents are created, matching the WebUI's prompt-and-retry outcome.
func DoFileEditCreate(c *cli.Client, taskID, rel, text string, doc cli.FileEditDoc) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		err := c.FilePushBytes(ctx, taskID, doc.Encode(text), rel, cli.FilePushOpts{MkdirParents: true}, nil)
		st := cli.FileEditPushed
		if err != nil {
			st = cli.FileEditStatusInvalid
		}
		return FileEditCommittedMsg{Rel: rel, Status: st, Err: err}
	}
}
