package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// connectClient dials the server as an ordinary task-control client.
//
// The agent verbs used to open their own connection (ConnectAgent) because
// they spoke a different frame family and had to own its demux. They do not
// any more, and no handshake is lost with it: cli.Dial runs the same merged
// PSK+identity exchange, and buildMergedClientHello upgrades the announced
// kind to Agent with AgentInfo whenever HARNESS_TASK_ID / HARNESS_RUNNER_ID /
// HARNESS_AUTH_TICKET are populated — which, for a verb that only works
// inside a task, is always.
//
// ClientKind_Cli is what ConnectAgent announced too. It is the fallback the
// hello uses when the agent env is absent, not a claim to be an operator.
func connectClient(ctx context.Context, serverCID string) (*cli.Client, error) {
	cid, err := cliopts.ResolveServerCID(serverCID)
	if err != nil {
		return nil, err
	}
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return nil, fmt.Errorf("agent dial: %w", err)
	}
	return c, nil
}

// expectKind checks a response envelope before its variant is read.
//
// It does NOT handle a permission denial, and that absence is the point: both
// RoundTripTaskControl and BeginTaskControl already turn one into a
// *cli.CapabilityDeniedError, which names the capability through CapsLabel —
// the single renderer every other surface uses. A denial therefore never
// reaches here, and no caller in this package spells a capability out. Two
// used to (`"board_observe"` in topics.go, `"purge"` in purge.go), each with
// wording the operator surface did not share.
func expectKind(resp *protocol.TaskControlResponse, want protocol.TaskControlKind) error {
	if resp == nil {
		return errors.New("agent: empty response")
	}
	if resp.Kind != want {
		return fmt.Errorf("agent: unexpected response kind=%v (want %v)", resp.Kind, want)
	}
	return nil
}

// fetchDeliveredPayload reads one announced delivery stream to EOF.
//
// A free function over the transport rather than a method, because the
// connection type it used to hang off is gone: every agent verb now rides the
// ordinary client, and the transport is what any of them can hand over.
//
// It polls briefly for the stream to appear. The server announces every stream
// id in the response and writes the bodies afterwards, so the frame that
// creates a stream can still be in flight when the response is parsed —
// arriving second is expected, not an error.
func fetchDeliveredPayload(ctx context.Context, tr trsf.Transport, streamID uint64) ([]byte, error) {
	if streamID == 0 {
		return nil, fmt.Errorf("delivered message stream_id is 0")
	}
	id := trsf.StreamID(streamID)
	st := tr.GetReceiveStream(id)
	if st == nil {
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
	wait:
		for st == nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				return nil, fmt.Errorf("payload stream %d not visible after 2s", id)
			case <-tick.C:
				st = tr.GetReceiveStream(id)
				if st != nil {
					break wait
				}
			}
		}
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("payload stream read: %w", err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			return raw, nil
		}
	}
}
