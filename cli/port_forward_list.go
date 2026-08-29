package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

// PortForwardSpecString renders the forward's endpoints as one column. An
// in-process client endpoint has no address to show on the client side, so it
// says so instead of printing the empty bind pair as ":0".
func PortForwardSpecString(fi *protocol.PortForwardInfo) string {
	listen := fmt.Sprintf("%s:%d", fi.BindAddr, fi.BindPort)
	switch {
	case fi.Direction == protocol.PortForwardDirection_Remote:
		listen = "runner:" + listen
	case fi.ClientEndpoint.IsInProcess():
		listen = inProcessLabel(fi.ClientEndpoint)
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
		// A second line rather than five more columns: the spec and origin
		// columns are already 40 and open-ended, and a row that wraps in a
		// normal terminal is worse than one that takes two lines on purpose.
		lines = append(lines, "          "+PortForwardTrafficLine(fi))
	}
	return lines
}

// PortForwardTrafficLine renders what a forward has carried. Exported because
// all three operator surfaces show the same values, and the browser reaches
// this function over the wasm bridge rather than re-deriving the format in JS.
//
// Every field prints, including zeros: `conns=0/0 to-target=0` says the forward
// is idle, while a blank would say the row does not report traffic. `last` is
// the one field that reads as a word — it has no measurement to show until the
// first byte, which is different from a zero duration.
func PortForwardTrafficLine(fi *protocol.PortForwardInfo) string {
	last := "never"
	if fi.LastActivityUnixMs != 0 {
		last = time.Since(time.UnixMilli(int64(fi.LastActivityUnixMs))).Truncate(time.Second).String() + " ago"
	}
	return fmt.Sprintf("conns=%d/%d  to-target=%s  from-target=%s  last=%s  taps=%d",
		fi.ConnsOpen, fi.ConnsTotal,
		FormatByteCount(fi.BytesToTarget), FormatByteCount(fi.BytesFromTarget),
		last, fi.Taps)
}

// portForwardJSON is the single source of truth for the JSON shape of a
// PortForwardInfo. A struct (not map[string]any) keeps field order stable
// across JSON Lines output.
type portForwardJSON struct {
	ForwardID      uint64 `json:"forward_id"`
	Dir            string `json:"dir"`
	Task           string `json:"task"`
	BindAddr       string `json:"bind_addr"`
	BindPort       uint16 `json:"bind_port"`
	TargetHost     string `json:"target_host"`
	TargetPort     uint16 `json:"target_port"`
	ClientEndpoint string `json:"client_endpoint"`
	OriginKind     string `json:"origin_kind"`
	OriginCid      string `json:"origin_cid"`
	// Traffic. Always emitted, zeros included — the JSON form carries
	// everything, with no elision.
	BytesToTarget      uint64 `json:"bytes_to_target"`
	BytesFromTarget    uint64 `json:"bytes_from_target"`
	ConnsTotal         uint64 `json:"conns_total"`
	ConnsOpen          uint32 `json:"conns_open"`
	Taps               uint16 `json:"taps"`
	LastActivityUnixMs uint64 `json:"last_activity_unix_ms"`
}

// clientEndpointJSON renders the JSON contract's own spelling for the enum:
// "os_socket" / "in_process". Deliberately not
// strings.ToLower(fi.ClientEndpoint.String()) — the generated String() produces
// "OsSocket" / "InProcess", which lowercases to "ossocket" / "inprocess". The
// JSON key names and values are the wire contract a consumer scripts against,
// not the generator's label spelling.
func clientEndpointJSON(k protocol.ClientEndpointKind) string {
	// The PREDICATE, not a member list: this used to switch on the single
	// in_process member with `default: os_socket`, so every kind added to the
	// enum would have been reported here as a socket — a lie about the one
	// property this field exists to carry, in the form consumers script
	// against. The specific kind is not exposed in JSON: `in_process` is the
	// contract that already shipped, and narrowing it per kind would change
	// what an existing consumer reads.
	if k.IsInProcess() {
		return "in_process"
	}
	return "os_socket"
}

// inProcessLabel names WHAT the in-process client is, for the one place the
// three UIs render a forward's client side.
//
// `(in-process)` alone said four different things at once, and the operator's
// question was the reasonable one: a row nobody recognises gives no basis for
// deciding whether killing it is safe. The ssh gateway's row holds a remote
// editor's entire session open and read exactly like a preview pin.
//
// The bare member keeps the old label rather than gaining a new one: it is what
// a client older than the split sends, and "in-process, kind unsaid" is exactly
// what it means.
func inProcessLabel(k protocol.ClientEndpointKind) string {
	switch k {
	case protocol.ClientEndpointKind_InProcessStdio:
		return "(stdio)"
	case protocol.ClientEndpointKind_InProcessHttp:
		return "(http)"
	case protocol.ClientEndpointKind_InProcessPane:
		return "(pane)"
	case protocol.ClientEndpointKind_InProcessPreview:
		return "(preview)"
	case protocol.ClientEndpointKind_InProcessSshGateway:
		return "(ssh-gateway)"
	}
	// Reached by the bare member AND by a kind a NEWER peer knows and this
	// build does not — the wire round-trips an unknown enum value unchanged,
	// so this is a live path, not a defensive default.
	return "(in-process)"
}

// PortForwardInfoJSONLine returns one JSON object (single line, no trailing
// newline) for a PortForwardInfo.
func PortForwardInfoJSONLine(fi *protocol.PortForwardInfo) string {
	b, _ := json.Marshal(portForwardJSON{
		ForwardID:      fi.ForwardId,
		Dir:            PortForwardDirFlag(fi.Direction),
		Task:           taskIDStr(fi.TaskId.Id[:]),
		BindAddr:       string(fi.BindAddr),
		BindPort:       fi.BindPort,
		TargetHost:     string(fi.TargetHost),
		TargetPort:     fi.TargetPort,
		ClientEndpoint: clientEndpointJSON(fi.ClientEndpoint),
		OriginKind:     strings.ToLower(fi.OriginKind.String()),
		OriginCid:      string(fi.OriginCid),

		BytesToTarget:      fi.BytesToTarget,
		BytesFromTarget:    fi.BytesFromTarget,
		ConnsTotal:         fi.ConnsTotal,
		ConnsOpen:          fi.ConnsOpen,
		Taps:               fi.Taps,
		LastActivityUnixMs: fi.LastActivityUnixMs,
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

// PortForwardConfigSpec renders a registered forward as the `-L …` / `-R …`
// value a workspace config holds, and reports whether it can be rendered at
// all. It is a SECOND renderer on purpose: PortForwardSpecString above writes
// `bind -> target` for a person reading `forward ls`, which is not a spec any
// parser accepts.
//
// ok is false for an in-process client endpoint. Per the schema
// (runner/protocol/message.bgn, ClientEndpointKind), such a forward's
// client-side address pair is EMPTY — a raw TUI pane, a WebUI preview pin, a -W
// stdio splice — so there is no local port to write down and nothing an apply
// could re-establish. That test lives here, once, rather than at each caller.
//
// The four-field [bind:]port:host:port form is used rather than the three-field
// short form so a non-default bind address survives a save/apply round trip.
func PortForwardConfigSpec(fi *protocol.PortForwardInfo) (string, bool) {
	if fi.ClientEndpoint != protocol.ClientEndpointKind_OsSocket {
		return "", false
	}
	return fmt.Sprintf("%s %s:%d:%s:%d", PortForwardDirFlag(fi.Direction),
		fi.BindAddr, fi.BindPort, fi.TargetHost, fi.TargetPort), true
}
