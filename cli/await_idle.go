package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// AwaitIdle sends an AwaitIdle TaskControl request over an existing *Client.
// sink=Reply LONG-POLLS: the server defers the response until the session's
// PTY output has been quiescent for the threshold (or the session stops), so
// the call blocks until then — bound it with ctx if needed. sink=Notify/Board
// return immediately with Status_Armed and the server delivers the fire
// out-of-band. Method form: long-lived consumers (TUI/WebUI) call this on
// their held client.
func (c *Client) AwaitIdle(ctx context.Context, taskIDHex string, thresholdMs uint32, sink protocol.AwaitIdleSink, topic string) (*protocol.AwaitIdleResponse, error) {
	raw, err := hex.DecodeString(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("invalid task id %q: %w", taskIDHex, err)
	}
	if len(raw) != 16 {
		return nil, fmt.Errorf("task id must be 16 bytes (32 hex chars)")
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle}
	ar := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: thresholdMs, Sink: sink}
	ar.SetTopic([]byte(topic))
	req.SetAwaitIdle(ar)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	ai := resp.AwaitIdle()
	if resp.Kind != protocol.TaskControlKind_AwaitIdle || ai == nil {
		return nil, fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	return ai, nil
}

// AwaitIdleListWith reports the armed watchers this caller can see: its own on
// tasks still visible to it, or every one for the operator.
func (c *Client) AwaitIdleListWith(ctx context.Context, taskFilter string) ([]protocol.AwaitIdleWatcherInfo, error) {
	var q protocol.AwaitIdleListRequest
	if taskFilter != "" {
		tid, err := parseTaskIDHex(taskFilter)
		if err != nil {
			return nil, fmt.Errorf("await-idle ls: parse task id: %w", err)
		}
		q.TaskId = tid
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdleList}
	req.SetAwaitIdleList(q)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.AwaitIdleList()
	if lr == nil {
		return nil, fmt.Errorf("await-idle ls: expected AwaitIdleList response, got kind=%v", resp.Kind)
	}
	if lr.StreamId == 0 {
		return nil, errors.New("await-idle ls: server returned no stream id (could not allocate)")
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(lr.StreamId))
	if st == nil {
		return nil, fmt.Errorf("await-idle ls: stream %d not visible after response", lr.StreamId)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, rerr := st.ReadDirect(64 * 1024)
		if rerr != nil {
			return nil, fmt.Errorf("await-idle ls: read: %w", rerr)
		}
		raw = append(raw, data...)
		if eof {
			break
		}
	}
	var body protocol.AwaitIdleListBody
	if derr := body.DecodeExactCopy(raw); derr != nil {
		return nil, fmt.Errorf("await-idle ls: decode: %w", derr)
	}
	return body.Watchers, nil
}

// AwaitIdleList is the short-lived-CLI form of AwaitIdleListWith.
func AwaitIdleList(ctx context.Context, peerCID objproto.ConnectionID, taskFilter string) ([]protocol.AwaitIdleWatcherInfo, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.AwaitIdleListWith(ctx, taskFilter)
}

// AwaitIdleKillWith disarms one watcher by id.
func (c *Client) AwaitIdleKillWith(ctx context.Context, id uint64) error {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdleKill}
	req.SetAwaitIdleKill(protocol.AwaitIdleKillRequest{WatcherId: id})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	kr := resp.AwaitIdleKill()
	if kr == nil {
		return fmt.Errorf("await-idle kill: expected AwaitIdleKill response, got kind=%v", resp.Kind)
	}
	switch kr.Status {
	case protocol.AwaitIdleKillStatus_Ok:
		return nil
	case protocol.AwaitIdleKillStatus_NotFound:
		// Unknown, already fired, or not the caller's — deliberately one
		// answer, so a foreign id is not an existence oracle.
		return fmt.Errorf("await-idle kill: no such watcher %d", id)
	default:
		return fmt.Errorf("await-idle kill: %s", kr.Status.String())
	}
}

// AwaitIdleKill is the short-lived-CLI form of AwaitIdleKillWith.
func AwaitIdleKill(ctx context.Context, peerCID objproto.ConnectionID, id uint64) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.AwaitIdleKillWith(ctx, id)
}

// AwaitIdleStatusString renders the wire enum in the schema's snake_case (the
// generated String() is CamelCase). The CLI's JSON and the wasm bridge both
// print it; there used to be one hand-written copy in each.
func AwaitIdleStatusString(s protocol.AwaitIdleStatus) string {
	switch s {
	case protocol.AwaitIdleStatus_Fired:
		return "fired"
	case protocol.AwaitIdleStatus_Armed:
		return "armed"
	case protocol.AwaitIdleStatus_SessionStopped:
		return "session_stopped"
	case protocol.AwaitIdleStatus_NotFound:
		return "not_found"
	case protocol.AwaitIdleStatus_BadRequest:
		return "bad_request"
	case protocol.AwaitIdleStatus_Cancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

func AwaitIdleSinkString(s protocol.AwaitIdleSink) string {
	switch s {
	case protocol.AwaitIdleSink_Reply:
		return "reply"
	case protocol.AwaitIdleSink_Notify:
		return "notify"
	case protocol.AwaitIdleSink_Board:
		return "board"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// AwaitIdleWatcherBy names who armed a watcher: "operator" for the zero
// principal, otherwise the task's 8-hex prefix.
func AwaitIdleWatcherBy(w *protocol.AwaitIdleWatcherInfo) string {
	if w.Requester.Id == ([16]byte{}) {
		return "operator"
	}
	return hex.EncodeToString(w.Requester.Id[:])[:8]
}

// AwaitIdleWatcherLines renders the listing as text. An empty list says so
// rather than printing nothing, so "none armed" and "the command printed
// nothing" cannot be confused.
func AwaitIdleWatcherLines(ws []protocol.AwaitIdleWatcherInfo) []string {
	if len(ws) == 0 {
		return []string{"no armed watchers"}
	}
	out := make([]string, 0, len(ws))
	for i := range ws {
		w := &ws[i]
		age := "-"
		if w.ArmedUnixMs > 0 {
			age = time.Since(time.UnixMilli(int64(w.ArmedUnixMs))).Truncate(time.Second).String()
		}
		sink := "sink=" + AwaitIdleSinkString(w.Sink)
		if w.Sink == protocol.AwaitIdleSink_Board {
			sink += " topic=" + string(w.Topic)
		}
		out = append(out, fmt.Sprintf("%-6d %-8s  %s  threshold=%dms  armed=%s  by=%s",
			w.WatcherId, hex.EncodeToString(w.TaskId.Id[:])[:8], sink, w.ThresholdMs, age, AwaitIdleWatcherBy(w)))
	}
	return out
}

// AwaitIdleWatcherJSONLine renders one row with the full ids. requester is ""
// for the operator.
func AwaitIdleWatcherJSONLine(w *protocol.AwaitIdleWatcherInfo) string {
	requester := ""
	if w.Requester.Id != ([16]byte{}) {
		requester = hex.EncodeToString(w.Requester.Id[:])
	}
	row := struct {
		WatcherID   uint64 `json:"watcher_id"`
		TaskID      string `json:"task_id"`
		Sink        string `json:"sink"`
		Topic       string `json:"topic"`
		ThresholdMs uint32 `json:"threshold_ms"`
		ArmedUnixMs uint64 `json:"armed_unix_ms"`
		Requester   string `json:"requester"`
		OriginKind  string `json:"origin_kind"`
		OriginCID   string `json:"origin_cid"`
	}{
		WatcherID:   w.WatcherId,
		TaskID:      hex.EncodeToString(w.TaskId.Id[:]),
		Sink:        AwaitIdleSinkString(w.Sink),
		Topic:       string(w.Topic),
		ThresholdMs: w.ThresholdMs,
		ArmedUnixMs: w.ArmedUnixMs,
		Requester:   requester,
		OriginKind:  w.OriginKind.String(),
		OriginCID:   string(w.OriginCid),
	}
	b, err := json.Marshal(row)
	if err != nil {
		return "{}"
	}
	return string(b)
}
