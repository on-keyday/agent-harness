package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Archives is the entry for `harness-cli agent archives --topic T`: the
// archives of one topic this task may read, one JSON Lines record each. No
// capability; every archive of every topic is `board archives`, which needs
// board_observe. See docs/superpowers/specs/2026-11-02-agentboard-flush-archive-design.md.
func Archives(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("archives", args)
	if perr != nil {
		return perr
	}
	return ArchivesWith(ctx, a, stdout)
}

// ArchivesWith is Archives for a caller that already has the parsed action.
func ArchivesWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	if a.Topic == "" {
		return errors.New("agent archives: --topic is required")
	}
	c, err := connectClient(ctx, a.ServerCID)
	if err != nil {
		return err
	}
	defer c.Close()

	lr := protocol.AgentArchivesRequest{}
	lr.SetTopic([]byte(a.Topic))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentArchives}
	req.SetAgentArchives(lr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_AgentArchives); err != nil {
		return err
	}
	r := resp.AgentArchives()
	switch {
	case r == nil:
		return errors.New("agent archives: response variant is nil")
	case r.Status == protocol.BoardStatus_NotFound:
		return fmt.Errorf("topic %q is not readable by this task", a.Topic)
	case r.Status != protocol.BoardStatus_Ok:
		return fmt.Errorf("agent archives --topic %s: %v (see the server log)", a.Topic, r.Status)
	}
	var body protocol.BoardArchivesBody
	if err := cli.ReadResponseBody(ctx, c.Transport(), r.StreamId, &body); err != nil {
		return fmt.Errorf("agent archives: %w", err)
	}
	enc := json.NewEncoder(stdout)
	for _, row := range cli.ArchiveRows(body.Rows) {
		if err := enc.Encode(row.JSON()); err != nil {
			return err
		}
	}
	return nil
}

// readArchive is `agent read --archive <id>`: one page of an archive whose
// topic this task may read, live messages only, newest first.
func readArchive(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	c, cerr := connectClient(ctx, a.ServerCID)
	if cerr != nil {
		return cerr
	}
	defer c.Close()

	ar := protocol.AgentArchiveReadRequest{BeforeSeq: a.Before, Limit: uint16(a.Limit)}
	if !ar.SetId([]byte(a.Archive)) {
		return errors.New("read --archive: id too long")
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentArchiveRead}
	req.SetAgentArchiveRead(ar)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_AgentArchiveRead); err != nil {
		return err
	}
	r := resp.AgentArchiveRead()
	switch {
	case r == nil:
		return errors.New("read --archive: response variant is nil")
	case r.Status == protocol.AgentReadPageStatus_NotFound:
		// Merged by the server: an unknown id and an archive of a topic this
		// task may not read are one answer.
		return fmt.Errorf("archive %q is not readable: no such archive, or its topic is not one this task may read", a.Archive)
	case r.Status != protocol.AgentReadPageStatus_Ok:
		return fmt.Errorf("read --archive %s: the server could not read the archive (%v)", a.Archive, r.Status)
	}
	msgs, rerr := readDelivered(ctx, c, r.StreamId)
	if rerr != nil {
		return fmt.Errorf("read --archive %s: %w", a.Archive, rerr)
	}
	for _, m := range msgs {
		emitMessageLine(stdout, m, m.Payload)
	}
	return nil
}

// purgeArchive is `agent purge --archive <id>`: board_archive_purge, as
// agent purge uses board_purge. Needs the purge bit.
func purgeArchive(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	c, err := connectClient(ctx, a.ServerCID)
	if err != nil {
		return err
	}
	defer c.Close()
	purged, found, err := c.BoardArchivePurge(ctx, a.Archive, a.Seq)
	if err != nil {
		return err
	}
	status := "ok"
	if !found {
		status = "not_found"
	}
	fmt.Fprintf(stdout, "{\"status\":%q,\"archive\":%q,\"purged\":%d}\n", status, a.Archive, purged)
	return nil
}
