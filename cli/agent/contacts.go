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

// Contacts fetches the calling task's contacts — the reply routes it may
// publish to without board_peer — and emits one JSON Lines record per route,
// newest first.
func Contacts(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("contacts", args)
	if perr != nil {
		return perr
	}
	return ContactsWith(ctx, a, stdout)
}

// ContactsWith is Contacts for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func ContactsWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	c, err := connectClient(ctx, a.ServerCID)
	if err != nil {
		return err
	}
	defer c.Close()

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentListContacts}
	req.SetAgentListContacts(protocol.AgentListContactsRequest{})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_AgentListContacts); err != nil {
		return err
	}
	r := resp.AgentListContacts()
	if r == nil {
		return errors.New("agent: contacts response variant is nil")
	}
	var body protocol.ContactsBody
	if err := cli.ReadResponseBody(ctx, c.Transport(), r.StreamId, &body); err != nil {
		return fmt.Errorf("contacts: %w", err)
	}
	for _, ct := range body.Contacts {
		line, _ := json.Marshal(map[string]any{"topic": string(ct.Topic)})
		fmt.Fprintln(stdout, string(line))
	}
	return nil
}
