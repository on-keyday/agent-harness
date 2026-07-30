package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// PortForwardListWith queries the server for the forwards this caller may see.
// Reuses the caller's existing *Client — no extra dial. Wire path is the same
// three steps as ConnListWith (cli/conns.go:28): round-trip, pick up the
// server-initiated send-stream by id, read to EOF, decode.
func (c *Client) PortForwardListWith(ctx context.Context, taskFilter string) ([]protocol.PortForwardInfo, error) {
	var q protocol.PortForwardListQuery
	if taskFilter != "" {
		tid, err := parseTaskIDHex(taskFilter)
		if err != nil {
			return nil, fmt.Errorf("forward ls: parse task id: %w", err)
		}
		q.TaskId = tid
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ListPortForwards}
	req.SetListPortForwards(q)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.ListPortForwards()
	if lr == nil {
		return nil, fmt.Errorf("expected ListPortForwards response, got kind=%v", resp.Kind)
	}
	if lr.StreamId == 0 {
		return nil, fmt.Errorf("server returned no stream id (could not allocate)")
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(lr.StreamId))
	if st == nil {
		return nil, fmt.Errorf("forward-list stream %d not visible after response", lr.StreamId)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("forward-list stream read: %w", err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			break
		}
	}
	body := &protocol.PortForwardListResultBody{}
	if err := body.DecodeExact(raw); err != nil {
		return nil, fmt.Errorf("decode PortForwardListResultBody (%d bytes): %w", len(raw), err)
	}
	return body.Forwards, nil
}

// PortForwardList opens a fresh Client, lists, and closes it. For short-lived
// harness-cli invocations only — TUI/WebUI hold a *Client and call the With form.
func PortForwardList(ctx context.Context, peerCID objproto.ConnectionID, taskFilter string) ([]protocol.PortForwardInfo, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.PortForwardListWith(ctx, taskFilter)
}

// KillPortForwardWith closes one registered forward by id.
func (c *Client) KillPortForwardWith(ctx context.Context, id uint64) error {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_KillPortForward}
	req.SetKillPortForward(protocol.KillPortForwardRequest{ForwardId: id})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	kr := resp.KillPortForward()
	if kr == nil {
		return fmt.Errorf("expected KillPortForward response, got kind=%v", resp.Kind)
	}
	switch kr.Status {
	case protocol.KillPortForwardStatus_Ok:
		return nil
	case protocol.KillPortForwardStatus_NoSuchForward:
		return fmt.Errorf("forward kill: no such forward %d", id)
	default:
		return fmt.Errorf("forward kill: server error (status=%d)", kr.Status)
	}
}

// KillPortForward is the short-lived-CLI form of KillPortForwardWith.
func KillPortForward(ctx context.Context, peerCID objproto.ConnectionID, id uint64) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.KillPortForwardWith(ctx, id)
}

// PortForwardSpecString renders a forward's address pair. The "runner:" prefix
// marks where the listener lives for a remote forward.
func PortForwardSpecString(fi *protocol.PortForwardInfo) string {
	listen := fmt.Sprintf("%s:%d", fi.BindAddr, fi.BindPort)
	if fi.Direction == protocol.PortForwardDirection_Remote {
		listen = "runner:" + listen
	}
	return fmt.Sprintf("%s -> %s:%d", listen, fi.TargetHost, fi.TargetPort)
}

// PortForwardDirFlag renders the direction as the CLI flag that creates it.
func PortForwardDirFlag(d protocol.PortForwardDirection) string {
	if d == protocol.PortForwardDirection_Remote {
		return "-R"
	}
	return "-L"
}

// PortForwardInfoLines returns the header plus one line per forward, matching
// the shape of ConnInfoLines.
func PortForwardInfoLines(fs []protocol.PortForwardInfo) []string {
	lines := []string{"PORT FORWARDS"}
	if len(fs) == 0 {
		return append(lines, "  (none)")
	}
	lines = append(lines, fmt.Sprintf("  %-6s  %-3s  %-12s  %-40s  %s", "ID", "DIR", "TASK", "SPEC", "ORIGIN"))
	for i := range fs {
		fi := &fs[i]
		lines = append(lines, fmt.Sprintf("  %-6d  %-3s  %-12s  %-40s  %s",
			fi.ForwardId, PortForwardDirFlag(fi.Direction), principalShort(fi.TaskId.Id[:]),
			PortForwardSpecString(fi), PortForwardOrigin(fi)))
	}
	return lines
}

// portForwardJSON is the single source of truth for the JSON shape of a
// PortForwardInfo. A struct (not map[string]any) keeps field order stable
// across JSON Lines output.
type portForwardJSON struct {
	ForwardID  uint64 `json:"forward_id"`
	Dir        string `json:"dir"`
	Task       string `json:"task"`
	BindAddr   string `json:"bind_addr"`
	BindPort   uint16 `json:"bind_port"`
	TargetHost string `json:"target_host"`
	TargetPort uint16 `json:"target_port"`
	OriginKind string `json:"origin_kind"`
	OriginCid  string `json:"origin_cid"`
}

// PortForwardInfoJSONLine returns one JSON object (single line, no trailing
// newline) for a PortForwardInfo.
func PortForwardInfoJSONLine(fi *protocol.PortForwardInfo) string {
	b, _ := json.Marshal(portForwardJSON{
		ForwardID:  fi.ForwardId,
		Dir:        PortForwardDirFlag(fi.Direction),
		Task:       taskIDStr(fi.TaskId.Id[:]),
		BindAddr:   string(fi.BindAddr),
		BindPort:   fi.BindPort,
		TargetHost: string(fi.TargetHost),
		TargetPort: fi.TargetPort,
		OriginKind: strings.ToLower(fi.OriginKind.String()),
		OriginCid:  string(fi.OriginCid),
	})
	return string(b)
}

// PortForwardOrigin renders "kind cid" for the ORIGIN column, e.g.
// "cli ws:…-ab". Exported so every operator surface (harness-cli's `forward
// ls`, the TUI's forwards modal) renders "origin" identically — the CID half
// is what actually distinguishes two forwards with an identical spec started
// by different clients, which is the whole point of a shared registry.
func PortForwardOrigin(fi *protocol.PortForwardInfo) string {
	return strings.ToLower(fi.OriginKind.String()) + " " + string(fi.OriginCid)
}
