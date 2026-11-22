package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// TaskRename sets a task's display name (session rename, delegated-regrant
// spec R3). An empty name clears it.
func (c *Client) TaskRename(ctx context.Context, taskID, name string) error {
	return taskRenameOn(ctx, c, taskID, name)
}

func taskRenameOn(ctx context.Context, c taskControlClient, taskID, name string) error {
	// The server refuses past 64 bytes; checked here too because the wire
	// length is a u8 and SetName silently sends an EMPTY name past 255 —
	// which the server reads as "clear" and answers ok.
	if len(name) > 64 {
		return fmt.Errorf("session rename: a name is at most 64 bytes, this one is %d", len(name))
	}
	tid, err := parseTaskIDHex(taskID)
	if err != nil {
		return fmt.Errorf("session rename: %w", err)
	}
	body := protocol.TaskRenameRequest{TaskId: tid}
	body.SetName([]byte(name))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_TaskRename}}
	req.SetTaskRename(body)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	tr := resp.TaskRename()
	if resp.Kind != protocol.TaskControlKind_TaskRename || tr == nil {
		return fmt.Errorf("unexpected response: %+v", resp)
	}
	return taskRenameStatusError(tr.Status)
}

// TaskRenameWith renames over an already-connected client and prints the
// target and the change. Long-lived embedders (TUI/WebUI) call this form.
func TaskRenameWith(ctx context.Context, c taskControlClient, taskID, name string, out io.Writer) error {
	if err := taskRenameOn(ctx, c, taskID, name); err != nil {
		return fmt.Errorf("session rename %s: %w", shortID(taskID), err)
	}
	if name == "" {
		fmt.Fprintf(out, "session rename %s: name cleared\n", shortID(taskID))
		return nil
	}
	fmt.Fprintf(out, "session rename %s: name=%s\n", shortID(taskID), taskNameText([]byte(name)))
	return nil
}

// TaskRenameOut dials, renames and closes (the harness-cli binary).
func TaskRenameOut(ctx context.Context, serverCID objproto.ConnectionID, taskID, name string, out io.Writer) error {
	c, err := Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return TaskRenameWith(ctx, c, taskID, name, out)
}

func taskRenameStatusError(s protocol.TaskRenameStatus) error {
	switch s {
	case protocol.TaskRenameStatus_Ok:
		return nil
	case protocol.TaskRenameStatus_NotFound:
		return fmt.Errorf("no such task within your reach (from inside a task you may rename only your own " +
			"descendants within your resume scope, never yourself)")
	case protocol.TaskRenameStatus_BadName:
		return fmt.Errorf("a name is at most 64 bytes of UTF-8 with no control characters")
	default:
		return fmt.Errorf("session rename failed: %v", s)
	}
}
