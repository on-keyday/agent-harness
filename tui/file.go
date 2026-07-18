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
// Op is a short verb ("ls", "push", "pull", "delete") used to compose
// the cmdresult line. Output is non-empty on `ls` success (the listing
// to render); Err is non-nil on failure for any verb. Both empty
// indicates a silent success for a write op (`push`, `pull`,
// `delete`); the dispatch handler renders an "ok: ..." line in that
// case.
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
// force overwrite.
func DoFilePush(c *cli.Client, taskID, localSrc, remoteDst string, recursive, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var err error
		if recursive {
			err = c.FilePushDir(ctx, taskID, localSrc, remoteDst, cli.FilePushOpts{Force: force})
		} else {
			err = c.FilePush(ctx, taskID, localSrc, remoteDst, cli.FilePushOpts{Force: force})
		}
		return FileResultMsg{
			Op:     "push",
			TaskID: taskID,
			Detail: fmt.Sprintf("%s -> %s", localSrc, remoteDst),
			Err:    err,
		}
	}
}

// DoFilePull copies from the task's worktree to a local destination.
// The recursive variant uses dir_pull (tar over the wire); the
// non-recursive variant uses the single-file pull path with optional
// force overwrite of the local destination.
func DoFilePull(c *cli.Client, taskID, remoteSrc, localDst string, recursive, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var err error
		if recursive {
			err = c.FilePullDir(ctx, taskID, remoteSrc, localDst, force)
		} else {
			err = c.FilePull(ctx, taskID, remoteSrc, localDst, force)
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
