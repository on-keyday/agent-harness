package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// ConnListWith queries the server for a snapshot of all live connections and
// returns the decoded slice. It reuses the caller's existing *Client so
// no additional dial/close happens — suitable for TUI/WebUI which hold a
// long-lived client.
//
// The wire path mirrors Snapshot / cli/list.go exactly:
//  1. Send TaskControlKind_ListConns with a ConnListQuery.
//  2. Receive ConnListResult — the server opens a trsf send-stream with the
//     encoded ConnListResultBody.
//  3. Read that stream until EOF and decode.
func (c *Client) ConnListWith(ctx context.Context) ([]protocol.ConnInfo, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ListConns}
	req.SetListConns(protocol.ConnListQuery{Reserved: 0})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.ListConns()
	if lr == nil {
		return nil, fmt.Errorf("expected ListConns response, got kind=%v", resp.Kind)
	}
	if lr.StreamId == 0 {
		return nil, fmt.Errorf("server returned no stream id (could not allocate)")
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(lr.StreamId))
	if st == nil {
		return nil, fmt.Errorf("conn-list stream %d not visible after response", lr.StreamId)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("conn-list stream read: %w", err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			break
		}
	}
	body := &protocol.ConnListResultBody{}
	if err := body.DecodeExact(raw); err != nil {
		return nil, fmt.Errorf("decode ConnListResultBody (%d bytes): %w", len(raw), err)
	}
	return body.Conns, nil
}

// ConnList opens a fresh Client, calls ConnListWith, and closes the client.
// Suitable for short-lived CLI invocations (harness-cli conns). Long-lived
// consumers (TUI, WebUI) should hold a *Client and call ConnListWith instead.
func ConnList(ctx context.Context, peerCID objproto.ConnectionID) ([]protocol.ConnInfo, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ConnListWith(ctx)
}

// ConnInfoTextLine renders one human-readable line for a ConnInfo (exported for
// cmd/harness-cli). Format: remote-addr  role  principal(short)  age  [unident]
func ConnInfoTextLine(ci *protocol.ConnInfo) string {
	return connInfoTextLine(ci)
}

// ConnInfoJSONLine returns a JSON line for a ConnInfo (exported for
// cmd/harness-cli).
func ConnInfoJSONLine(ci *protocol.ConnInfo) string {
	return connInfoJSONLine(ci)
}

// ConnInfoLines returns the header line plus one text line per ConnInfo. Used
// by cmd/harness-cli to render the connections table without exposing
// renderConns directly.
func ConnInfoLines(conns []protocol.ConnInfo) []string {
	lines := make([]string, 0, len(conns)+2)
	lines = append(lines, "CONNECTIONS")
	if len(conns) == 0 {
		lines = append(lines, "  (none)")
		return lines
	}
	lines = append(lines, fmt.Sprintf("  %-22s  %-11s  %-8s  %s", "REMOTE-ADDR", "ROLE", "PRINCIPAL", "AGE"))
	for i := range conns {
		lines = append(lines, "  "+connInfoTextLine(&conns[i]))
	}
	return lines
}

// connInfoTextLine renders one human-readable line for a ConnInfo.
// Format: remote-addr  role  principal(short)  age  [unident]
func connInfoTextLine(ci *protocol.ConnInfo) string {
	addr := string(ci.RemoteAddr)
	role := strings.ToLower(ci.Role.String())
	principal := principalShort(ci.PrincipalTask.Id[:])
	age := connAge(ci.ConnectedAt)
	unident := ""
	if !ci.Identified() {
		unident = "  unident"
	}
	return fmt.Sprintf("%-22s  %-11s  %s  %s%s", addr, role, principal, age, unident)
}

// connInfoJSONLine returns a JSON object (single line, no trailing newline) for
// a ConnInfo. Fields: remote_addr, role, principal_task (hex), age_sec,
// connected_at (unix nano), identified.
func connInfoJSONLine(ci *protocol.ConnInfo) string {
	m := map[string]any{
		"remote_addr":    string(ci.RemoteAddr),
		"role":           strings.ToLower(ci.Role.String()),
		"principal_task": taskIDStr(ci.PrincipalTask.Id[:]),
		"age_sec":        connAgeSec(ci.ConnectedAt),
		"connected_at":   ci.ConnectedAt,
		"identified":     ci.Identified(),
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// principalShort returns the first 8 hex characters of a task id, or "-" if
// all bytes are zero (i.e. no principal task, e.g. cli/tui/webui/runner conns).
// Reuses taskIDStr (cli/list.go) for the all-zero "-" check rather than
// re-implementing it, then truncates the full hex to 8 chars.
func principalShort(b []byte) string {
	full := taskIDStr(b)
	if full == "-" {
		return "-"
	}
	if len(full) > 8 {
		return full[:8]
	}
	return full
}

// connAgeSec returns the age of a connection in whole seconds. Returns 0 when
// ConnectedAt is zero (unset).
func connAgeSec(connectedAtNano uint64) int64 {
	if connectedAtNano == 0 {
		return 0
	}
	since := time.Since(time.Unix(0, int64(connectedAtNano)))
	if since < 0 {
		return 0
	}
	return int64(since.Seconds())
}

// connAge returns a human-readable age string, e.g. "90s" or "3m45s".
func connAge(connectedAtNano uint64) string {
	secs := connAgeSec(connectedAtNano)
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	m := secs / 60
	s := secs % 60
	return fmt.Sprintf("%dm%ds", m, s)
}

// PrintConns queries the server and writes a human-readable connection table to
// out. Method form for long-lived callers.
func (c *Client) PrintConns(ctx context.Context, out io.Writer) error {
	conns, err := c.ConnListWith(ctx)
	if err != nil {
		return err
	}
	renderConns(conns, out)
	return nil
}

// renderConns writes the connection table to out.
func renderConns(conns []protocol.ConnInfo, out io.Writer) {
	fmt.Fprintln(out, "CONNECTIONS")
	if len(conns) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}
	fmt.Fprintf(out, "  %-22s  %-11s  %-8s  %s\n", "REMOTE-ADDR", "ROLE", "PRINCIPAL", "AGE")
	for i := range conns {
		fmt.Fprintln(out, " ", connInfoTextLine(&conns[i]))
	}
}
