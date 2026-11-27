package cli

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// HostLogOpts selects the tail or byte-range window for `server log` and `runner log`.
type HostLogOpts struct {
	HasOffset bool
	Offset    uint64
	MaxBytes  uint32 // 0 => default (64 KiB); capped at 4 MiB on server/runner
}

// HostLogSnapshot is one decoded host-log stream answer.
type HostLogSnapshot struct {
	Source      protocol.HostLogSource
	TotalSize   uint64
	StartOffset uint64
	Data        []byte
}

const hostLogAnswerWait = 15 * time.Second

func hostLogStatusError(verb string, st protocol.HostLogStatus, msg []byte) error {
	what := EnumWireName(st.String())
	switch st {
	case protocol.HostLogStatus_Ok:
		return nil
	case protocol.HostLogStatus_NoSuchRunner:
		what += ": no such runner"
	case protocol.HostLogStatus_RunnerOffline:
		what += ": runner is not connected (process logs live on the runner host)"
	case protocol.HostLogStatus_RunnerUnsupported:
		what += ": this runner predates host logs; rebuild and restart it"
	case protocol.HostLogStatus_Unavailable:
		what += ": host log reader is not configured on that process"
	}
	if len(msg) > 0 {
		what += ": " + string(msg)
	}
	return fmt.Errorf("%s: %s", verb, what)
}

func (c *Client) hostLogRoundTrip(ctx context.Context, verb string, reqBody protocol.HostLogRequest) (HostLogSnapshot, error) {
	req := &protocol.TaskControlRequest{
		Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_HostLog},
	}
	req.SetHostLog(reqBody)

	rctx, cancel := context.WithTimeout(ctx, hostLogAnswerWait)
	defer cancel()
	resp, err := c.RoundTripTaskControl(rctx, req)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return HostLogSnapshot{}, fmt.Errorf("%s: no answer from the server in %s — a server older than host logs drops the request", verb, hostLogAnswerWait)
		}
		return HostLogSnapshot{}, err
	}
	if resp.Kind != protocol.TaskControlKind_HostLog {
		return HostLogSnapshot{}, fmt.Errorf("%s: unexpected response kind %v", verb, resp.Kind)
	}
	r := resp.HostLog()
	if r == nil {
		return HostLogSnapshot{}, fmt.Errorf("%s: empty response", verb)
	}
	if err := hostLogStatusError(verb, r.Status, nil); err != nil {
		return HostLogSnapshot{}, err
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return HostLogSnapshot{}, fmt.Errorf("%s: stream %d not visible after the response", verb, r.StreamId)
	}
	defer st.CloseBoth()
	// Best-effort half-close: for fast local server log reads, the server may
	// have already finished writing and closed the stream before we get here,
	// in which case AppendData returns EOF while the buffered bytes remain readable.
	_ = st.AppendData(true)

	br := bufio.NewReaderSize(streamCtxReader{ctx, st}, 64<<10)
	var res protocol.HostLogResult
	if err := res.Read(br); err != nil {
		return HostLogSnapshot{}, fmt.Errorf("%s: answer cut before header: %w", verb, err)
	}
	if err := hostLogStatusError(verb, res.Status, res.Message); err != nil {
		return HostLogSnapshot{}, err
	}
	data := make([]byte, res.ReturnedBytes)
	if _, err := io.ReadFull(br, data); err != nil {
		return HostLogSnapshot{}, fmt.Errorf("%s: stream cut while reading %d bytes: %w", verb, res.ReturnedBytes, err)
	}
	return HostLogSnapshot{
		Source:      res.Source,
		TotalSize:   res.TotalSize,
		StartOffset: res.StartOffset,
		Data:        data,
	}, nil
}

func buildHostLogRequest(target protocol.HostLogTarget, runnerID protocol.RunnerID, opts HostLogOpts) protocol.HostLogRequest {
	mode := protocol.HostLogMode_Tail
	var offset uint64
	if opts.HasOffset {
		mode = protocol.HostLogMode_Range
		offset = opts.Offset
	}
	return protocol.HostLogRequest{
		Target:   target,
		RunnerId: runnerID,
		Mode:     mode,
		Offset:   offset,
		MaxBytes: opts.MaxBytes,
	}
}

// resolveRunnerIdentity parses a full 32-hex runner ID directly, or resolves
// an 8..31-hex prefix against the server's live runner list.
func resolveRunnerIdentity(ctx context.Context, c *Client, prefix string) (protocol.RunnerID, error) {
	p := strings.ToLower(strings.TrimSpace(prefix))
	if len(p) == 32 {
		b, err := hex.DecodeString(p)
		if err != nil || len(b) != 16 {
			return protocol.RunnerID{}, fmt.Errorf("runner log: invalid runner id %q (expected 8-32 hex)", prefix)
		}
		var id protocol.RunnerID
		copy(id.Id[:], b)
		return id, nil
	}
	if len(p) < 8 || len(p) > 32 {
		return protocol.RunnerID{}, fmt.Errorf("runner log: invalid runner id prefix %q (expected 8-32 hex)", prefix)
	}
	if _, err := hex.DecodeString(p + strings.Repeat("0", len(p)%2)); err != nil {
		return protocol.RunnerID{}, fmt.Errorf("runner log: invalid hex prefix %q", prefix)
	}
	snap, err := c.SnapshotFinished(ctx, 0)
	if err != nil {
		return protocol.RunnerID{}, fmt.Errorf("runner log: resolve runner prefix: %w", err)
	}
	var matches []protocol.RunnerID
	for _, r := range snap.Runners {
		idHex := hex.EncodeToString(r.Id.Id[:])
		if strings.HasPrefix(idHex, p) {
			matches = append(matches, r.Id)
		}
	}
	switch len(matches) {
	case 0:
		return protocol.RunnerID{}, fmt.Errorf("runner log: no connected runner matches prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return protocol.RunnerID{}, fmt.Errorf("runner log: prefix %q matches %d runners; give more hex digits", prefix, len(matches))
	}
}

// HostLogServerWith fetches a tail or byte-range snapshot of the server's process log.
func HostLogServerWith(ctx context.Context, c *Client, opts HostLogOpts) (HostLogSnapshot, error) {
	return c.hostLogRoundTrip(ctx, "server log", buildHostLogRequest(protocol.HostLogTarget_Server, protocol.RunnerID{}, opts))
}

// HostLogRunnerWith fetches a tail or byte-range snapshot of a connected runner's process log.
func HostLogRunnerWith(ctx context.Context, c *Client, runnerPrefix string, opts HostLogOpts) (HostLogSnapshot, error) {
	id, err := resolveRunnerIdentity(ctx, c, runnerPrefix)
	if err != nil {
		return HostLogSnapshot{}, err
	}
	return c.hostLogRoundTrip(ctx, "runner log", buildHostLogRequest(protocol.HostLogTarget_Runner, id, opts))
}

// HostLogServer dials serverCID, fetches the server's process log, and closes.
func HostLogServer(ctx context.Context, serverCID objproto.ConnectionID, opts HostLogOpts) (HostLogSnapshot, error) {
	c, err := Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		return HostLogSnapshot{}, err
	}
	defer c.Close()
	return HostLogServerWith(ctx, c, opts)
}

// HostLogRunner dials serverCID, fetches the named runner's process log, and closes.
func HostLogRunner(ctx context.Context, serverCID objproto.ConnectionID, runnerPrefix string, opts HostLogOpts) (HostLogSnapshot, error) {
	c, err := Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		return HostLogSnapshot{}, err
	}
	defer c.Close()
	return HostLogRunnerWith(ctx, c, runnerPrefix, opts)
}

// FormatHostLogHeader formats a one-line metadata header describing a snapshot.
func FormatHostLogHeader(target string, snap HostLogSnapshot) string {
	end := snap.StartOffset + uint64(len(snap.Data))
	return fmt.Sprintf("%s log (%s, bytes %d..%d of %d)",
		target, EnumWireName(snap.Source.String()), snap.StartOffset, end, snap.TotalSize)
}
