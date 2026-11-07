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
// signal picks what idle means (quiet = PTY output quiescent for the
// threshold; lifecycle / program / any: see protocol.AwaitIdleSignal).
// sink=Reply LONG-POLLS: the server defers the response until the watcher
// fires (or the session stops), so the call blocks until then — bound it with ctx if needed. sink=Notify/Board
// return immediately with Status_Armed and the server delivers the fire
// out-of-band. Method form: long-lived consumers (TUI/WebUI) call this on
// their held client.
func (c *Client) AwaitIdle(ctx context.Context, taskIDHex string, thresholdMs uint32, sink protocol.AwaitSink, topic string, signal protocol.AwaitIdleSignal) (*protocol.AwaitIdleResponse, error) {
	raw, err := hex.DecodeString(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("invalid task id %q: %w", taskIDHex, err)
	}
	if len(raw) != 16 {
		return nil, fmt.Errorf("task id must be 16 bytes (32 hex chars)")
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_AwaitIdle}}
	ar := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: thresholdMs, Sink: sink, Signal: signal}
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
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_AwaitList}}
	req.SetAwaitList(q)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.AwaitList()
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
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_AwaitKill}}
	req.SetAwaitKill(protocol.AwaitIdleKillRequest{WatcherId: id})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	kr := resp.AwaitKill()
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

func AwaitIdleSinkString(s protocol.AwaitSink) string {
	switch s {
	case protocol.AwaitSink_Reply:
		return "reply"
	case protocol.AwaitSink_Notify:
		return "notify"
	case protocol.AwaitSink_Board:
		return "board"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// awaitIdleSignalWords is the schema's word for each signal, the one spelling
// the flag, the JSON output, the board payload and the watcher lists share.
var awaitIdleSignalWords = map[protocol.AwaitIdleSignal]string{
	protocol.AwaitIdleSignal_Quiet:     "quiet",
	protocol.AwaitIdleSignal_Lifecycle: "lifecycle",
	protocol.AwaitIdleSignal_Program:   "program",
	protocol.AwaitIdleSignal_Any:       "any",
}

// AwaitIdleSignalString is the signal's word, unknown(N) outside the enum.
func AwaitIdleSignalString(s protocol.AwaitIdleSignal) string {
	if w, ok := awaitIdleSignalWords[s]; ok {
		return w
	}
	return fmt.Sprintf("unknown(%d)", int(s))
}

// ParseAwaitIdleSignal reads a --signal word; "" is the default, quiet.
func ParseAwaitIdleSignal(word string) (protocol.AwaitIdleSignal, error) {
	if word == "" {
		return protocol.AwaitIdleSignal_Quiet, nil
	}
	for s, w := range awaitIdleSignalWords {
		if w == word {
			return s, nil
		}
	}
	return 0, fmt.Errorf("unknown signal %q (want quiet, lifecycle, program or any)", word)
}

// AwaitIdleFiredDetail is what a fire saw, for the leg that fired: "program
// done", "program blocked:permission", "lifecycle waiting". "" for the quiet
// leg, whose surfaces keep their "last output" wording. One spelling for the
// notification, the TUI result line and the WebUI.
func AwaitIdleFiredDetail(firedBy protocol.AwaitIdleSignal, lc protocol.LifecycleState, ps protocol.ProgramState, pk protocol.ProgramKind) string {
	switch firedBy {
	case protocol.AwaitIdleSignal_Program:
		state := ProgramWord(ps)
		if ps == protocol.ProgramState_Blocked && pk != protocol.ProgramKind_None {
			state += ":" + ProgramKindWord(pk)
		}
		return "program " + state
	case protocol.AwaitIdleSignal_Lifecycle:
		return "lifecycle " + LifecycleWord(lc)
	}
	return ""
}

// FilterWatchers keeps the rows of one condition: the registry, and so
// await_list, is shared by await-idle and await-progress, and each verb's
// `ls` shows its own.
func FilterWatchers(ws []protocol.AwaitIdleWatcherInfo, c protocol.AwaitCondition) []protocol.AwaitIdleWatcherInfo {
	out := make([]protocol.AwaitIdleWatcherInfo, 0, len(ws))
	for _, w := range ws {
		if w.Condition == c {
			out = append(out, w)
		}
	}
	return out
}

// AwaitConditionString is the condition in the schema's snake_case.
func AwaitConditionString(c protocol.AwaitCondition) string {
	switch c {
	case protocol.AwaitCondition_Idle:
		return "idle"
	case protocol.AwaitCondition_Progress:
		return "progress"
	default:
		return fmt.Sprintf("unknown(%d)", int(c))
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
		if w.Sink == protocol.AwaitSink_Board {
			sink += " topic=" + string(w.Topic)
		}
		wait := fmt.Sprintf("signal=%s threshold=%dms", AwaitIdleSignalString(w.Signal), w.ThresholdMs)
		if w.Condition == protocol.AwaitCondition_Progress {
			wait = "on=progress"
		}
		out = append(out, fmt.Sprintf("%-6d %-8s  %s  %s  armed=%s  by=%s",
			w.WatcherId, hex.EncodeToString(w.TaskId.Id[:])[:8], sink, wait, age, AwaitIdleWatcherBy(w)))
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
		Condition   string `json:"condition"`
		Signal      string `json:"signal"`
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
		Condition:   AwaitConditionString(w.Condition),
		Signal:      AwaitIdleSignalString(w.Signal),
	}
	b, err := json.Marshal(row)
	if err != nil {
		return "{}"
	}
	return string(b)
}
