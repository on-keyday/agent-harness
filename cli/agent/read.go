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

// Read is the entry for `harness-cli agent read <seq>`: one retained message,
// addressed directly. It is what the inbox hooks point at when a body is too
// large to inline, so it never truncates — the whole reason to run it is to
// get the body the hook withheld.
//
// Reading is scoped to topics this task subscribes to. A seq outside them is
// reported exactly like one that has rotated out of its ring, so the error
// names both possibilities rather than confirming which.
func Read(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent read", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "server ConnectionID (env: HARNESS_SERVER_CID)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agent read <seq>")
	}
	seq, perr := strconv.ParseUint(fs.Arg(0), 10, 64)
	if perr != nil || seq == 0 {
		return fmt.Errorf("seq must be a positive integer, got %q", fs.Arg(0))
	}

	conn, cerr := ConnectAgent(ctx, Flags{ServerCID: *serverCID})
	if cerr != nil {
		return cerr
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.ReadSeqResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_ReadSeqResponse {
			r := msg.ReadSeqResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ReadSeq}
	if !msg.SetReadSeq(agentboard.ReadSeqRequest{RequestId: reqID, Seq: seq}) {
		return errors.New("agent: SetReadSeq failed")
	}
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		if r.Status != agentboard.ReadSeqStatus_Ok || len(r.Msgs) == 0 {
			return fmt.Errorf("seq %d is not readable: it has rotated out of its topic's ring "+
				"(64 messages) or its 30-minute TTL, was purged, or is on a topic this task "+
				"does not subscribe to", seq)
		}
		m := r.Msgs[0]
		payload, perr := conn.FetchDeliveredPayload(ctx, m.PayloadStreamId)
		if perr != nil {
			return fmt.Errorf("fetch payload seq=%d: %w", m.Seq, perr)
		}
		emitMessageLine(stdout, m, payload)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
