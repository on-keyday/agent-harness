package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// payloadErrGrace bounds how long a failed payload write waits for the
// server's explanation before reporting the local error instead.
const payloadErrGrace = 2 * time.Second

// TaskControlWithPayload sends a task-control request whose body travels on a
// client-initiated send-stream, and returns the server's response. build gets
// the stream id and returns the request naming it. Streaming the body (instead
// of stuffing it into the request envelope) keeps the envelope inside path MTU
// on UDP. Every board publish -- agent send, agent dispatch, board send --
// goes through here.
//
// The request goes out BEFORE the body. It names the stream id, so until the
// server has it, nobody drains the payload stream: AppendData blocks once the
// send buffer fills (1MB) and stays blocked once the peer's receive window
// (16MB) is exhausted, so a body past the window used to deadlock with the
// request still unsent. Announcing first gives the server a reader — which is
// also what lets it stop an over-long body mid-flight instead of discovering
// the length after the fact. The server polls briefly for the stream to become
// visible, so arriving first is expected (server/agent_taskcontrol.go,
// readAgentPayloadStream). BeginTaskControl rather than RoundTripTaskControl
// for exactly that ordering: a round trip would block for the response before
// the body could be written.
//
// A returned error is a local failure. A server refusal comes back as the
// result, for the caller to read its status.
func (c *Client) TaskControlWithPayload(ctx context.Context, build func(streamID uint64) (*protocol.TaskControlRequest, error), payload []byte) (TaskControlResult, error) {
	stream := c.Transport().CreateSendStream()
	if stream == nil {
		return TaskControlResult{}, errors.New("failed to allocate payload stream")
	}
	return taskControlWithPayload(ctx, stream, c.BeginTaskControl, build, payload)
}

// TaskControlWithPayloads is TaskControlWithPayload for a request that names
// several body streams — an open's prompt beside its grant conditions, a
// schedule spec's the same. build gets the stream ids in the order of
// payloads; the bodies are written in that order after the request.
func (c *Client) TaskControlWithPayloads(ctx context.Context, build func(streamIDs []uint64) (*protocol.TaskControlRequest, error), payloads [][]byte) (TaskControlResult, error) {
	streams := make([]trsf.SendStream, 0, len(payloads))
	for range payloads {
		st := c.Transport().CreateSendStream()
		if st == nil {
			for _, s := range streams {
				_ = s.Close()
			}
			return TaskControlResult{}, errors.New("failed to allocate payload stream")
		}
		streams = append(streams, st)
	}
	return taskControlWithPayloads(ctx, streams, c.BeginTaskControl, build, payloads)
}

// taskControlWithPayload is TaskControlWithPayload once the stream exists, so
// the failure paths can be tested without a connection.
func taskControlWithPayload(ctx context.Context, stream trsf.SendStream, begin func(*protocol.TaskControlRequest) (<-chan TaskControlResult, error), build func(streamID uint64) (*protocol.TaskControlRequest, error), payload []byte) (TaskControlResult, error) {
	return taskControlWithPayloads(ctx, []trsf.SendStream{stream}, begin,
		func(ids []uint64) (*protocol.TaskControlRequest, error) { return build(ids[0]) }, [][]byte{payload})
}

// taskControlWithPayloads is the several-stream form, once the streams exist.
func taskControlWithPayloads(ctx context.Context, streams []trsf.SendStream, begin func(*protocol.TaskControlRequest) (<-chan TaskControlResult, error), build func(streamIDs []uint64) (*protocol.TaskControlRequest, error), payloads [][]byte) (TaskControlResult, error) {
	closeAll := func() {
		for _, s := range streams {
			_ = s.Close()
		}
	}
	ids := make([]uint64, len(streams))
	for i, s := range streams {
		ids[i] = uint64(s.ID())
	}
	req, err := build(ids)
	if err != nil {
		closeAll()
		return TaskControlResult{}, err
	}
	// A request that never went out — refused for size, a dead connection —
	// names streams nobody will read; close them rather than leave them
	// behind for the life of the connection.
	respCh, err := begin(req)
	if err != nil {
		closeAll()
		return TaskControlResult{}, err
	}
	// AppendDataContext, not AppendData: the latter passes context.Background()
	// internally, so a stalled write would ignore the caller's deadline and
	// hang instead of failing.
	var writeErr error
	for i, stream := range streams {
		writeErr = stream.AppendDataContext(ctx, false, payloads[i])
		if writeErr == nil {
			writeErr = stream.AppendDataContext(ctx, true)
		}
		if writeErr != nil {
			for _, rest := range streams[i+1:] {
				_ = rest.Close()
			}
			break
		}
	}
	if writeErr != nil {
		// The server tears the payload stream down when it refuses the body,
		// which surfaces here as a bare io.EOF. The actionable reason is in its
		// response, so wait briefly and prefer that; "EOF" tells the sender
		// nothing about what to do differently.
		select {
		case r := <-respCh:
			return r, nil
		case <-time.After(payloadErrGrace):
			return TaskControlResult{}, fmt.Errorf("payload stream write: %w", writeErr)
		case <-ctx.Done():
			return TaskControlResult{}, fmt.Errorf("payload stream write: %w", writeErr)
		}
	}
	select {
	case r := <-respCh:
		return r, nil
	case <-ctx.Done():
		return TaskControlResult{}, ctx.Err()
	}
}

// payloadSender is a client that can send a request with a body stream —
// *Client, and a test's fake.
type payloadSender interface {
	TaskControlWithPayload(ctx context.Context, build func(streamID uint64) (*protocol.TaskControlRequest, error), payload []byte) (TaskControlResult, error)
}

// roundTripWithBody sends the request build makes. With no body it is one
// plain round trip (build(0)); with one, the body rides its own stream named
// in the request, as a grant-writing request's grant conditions do (grant
// conditions C6).
func roundTripWithBody(ctx context.Context, c taskControlClient, body []byte, build func(streamID uint64) *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	if body == nil {
		return c.RoundTripTaskControl(ctx, build(0))
	}
	ps, ok := c.(payloadSender)
	if !ok {
		return nil, errors.New("this client cannot send a request body on a stream")
	}
	res, err := ps.TaskControlWithPayload(ctx, func(id uint64) (*protocol.TaskControlRequest, error) { return build(id), nil }, body)
	if err != nil {
		return nil, err
	}
	if res.Err != nil {
		return nil, res.Err
	}
	return res.Resp, nil
}
