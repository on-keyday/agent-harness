package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"strconv"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
)

// Retract is the entry for `harness-cli agent retract <seq>`: withdraw ONE
// message this task published. The message leaves every agent-facing path
// (deliver / inbox / wait / read_seq / retained) and survives only on the
// operator surfaces (`harness-cli board read`, TUI, WebUI), where it shows as
// retracted until it ages out with the topic.
//
// What it is for: a recipient whose context was reset re-reads a topic's
// retained ring and re-executes instructions it already carried out. Only the
// SENDER knows an instruction is spent, so withdrawing it has to be the
// sender's move — the reset recipient has nothing to distinguish handled from
// unhandled, which is the failure itself.
//
// No capability is required. The check is authorship: the server withdraws the
// message only when its recorded sender is this task, so retract can reach
// nothing the caller did not write. `agent purge` still needs Capability_Purge
// — it erases the bytes for real, including from the operator's view, and can
// take a whole topic of other agents' unread messages with it.
//
// Seq comes from the `seq` field this task got back when it sent the message
// (`agent send` prints it), or from `agent retained --topic <t>`, which lists
// each retained message's seq and sender.
func Retract(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent retract", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "server ConnectionID (env: HARNESS_SERVER_CID)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agent retract <seq>")
	}
	seq, perr := strconv.ParseUint(fs.Arg(0), 10, 64)
	if perr != nil || seq == 0 {
		return fmt.Errorf("seq must be a positive integer, got %q", fs.Arg(0))
	}

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.RetractResponse, 1)
	conn.SetOnControl(func(k appwire.AppKind, p []byte) {
		if k != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_RetractResponse {
			r := msg.RetractResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Retract}
	msg.SetRetract(agentboard.RetractRequest{RequestId: reqID, Seq: seq})
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		switch r.Status {
		case agentboard.RetractStatus_Ok:
			fmt.Fprintf(stdout, "{\"status\":\"ok\",\"seq\":%d}\n", seq)
			return nil
		case agentboard.RetractStatus_NotFound:
			// Idempotent, and deliberately blind: "no live message with that
			// seq" (never published, rotated out, already retracted, purged)
			// and "published by somebody else" are one answer. seq is
			// board-global and consecutive, so separating them would confirm
			// the existence of any seq on any topic the caller cannot name.
			fmt.Fprintf(stdout, "{\"status\":\"not_found\",\"seq\":%d}\n", seq)
			return nil
		default:
			return fmt.Errorf("retract: unexpected status %v", r.Status)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}
