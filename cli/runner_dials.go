package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// runner dial add / ls / rm and runner kick
// (docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md).
// As with the runner key verbs, every surface calls the *With forms on the
// client it already holds and prints through the Format* functions below.

// RunnerDialRow is one dial record and the state of its loop.
type RunnerDialRow struct {
	ID          uint32
	Label       string
	Key         ed25519.PublicKey
	Candidates  []string
	State       protocol.RunnerDialState
	Runner      protocol.RunnerID // zero when none
	Answered    int               // index into Candidates; -1 none
	NextAttempt time.Time         // zero unless waiting
	LastError   string
	AddedAt     time.Time
	AddedBy     string
	DuplicateOf uint32
}

// RunnerKickResult is what a kick answered about.
type RunnerKickResult struct {
	Runner       RunnerKeyUser
	Tasks        []protocol.TaskID
	RemovedDials []uint32
}

// FormatDialID is how a dial record id is shown and typed: 8 lower hex.
func FormatDialID(id uint32) string { return fmt.Sprintf("%08x", id) }

// ParseDialID reads FormatDialID's form back.
func ParseDialID(s string) (uint32, error) {
	if len(s) != 8 {
		return 0, fmt.Errorf("a dial record id is 8 hex characters, got %q", s)
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("a dial record id is 8 hex characters, got %q", s)
	}
	return uint32(v), nil
}

// RunnerLinkStr is how a runner row shows its link: in, out, dial:<8 hex>,
// via:<8 hex>. The one formatter every surface uses.
func RunnerLinkStr(r protocol.RunnerInfo) string {
	switch r.Link {
	case protocol.RunnerLink_DialedIn:
		return "in"
	case protocol.RunnerLink_DialedOut:
		return "out"
	case protocol.RunnerLink_DialRecord:
		return "dial:" + FormatDialID(r.LinkRecord)
	case protocol.RunnerLink_Via:
		return "via:" + hex.EncodeToString(r.LinkVia.Id[:4])
	}
	return r.Link.String()
}

func RunnerDialAddWith(ctx context.Context, c taskControlClient, candidates []string, keyPrefix, label string) (protocol.RunnerDialStatus, uint32, error) {
	r := protocol.RunnerDialAddRequest{}
	if !r.SetKeyPrefix([]byte(keyPrefix)) {
		return 0, 0, fmt.Errorf("runner dial add: --key too long")
	}
	if !r.SetLabel([]byte(label)) {
		return 0, 0, fmt.Errorf("runner dial add: --label too long")
	}
	cs := make([]protocol.RunnerDialCandidate, len(candidates))
	for i, c := range candidates {
		if !cs[i].SetText([]byte(c)) {
			return 0, 0, fmt.Errorf("runner dial add: candidate %q too long (max 255 bytes)", c)
		}
	}
	if !r.SetCandidates(cs) {
		return 0, 0, fmt.Errorf("runner dial add: too many candidates")
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerDialAdd}
	req.SetRunnerDialAdd(r)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, 0, err
	}
	a := resp.RunnerDialAdd()
	if resp.Kind != protocol.TaskControlKind_RunnerDialAdd || a == nil {
		return 0, 0, fmt.Errorf("runner dial add: unexpected response kind=%v", resp.Kind)
	}
	return a.Status, a.Id, nil
}

func RunnerDialListWith(ctx context.Context, c *Client) ([]RunnerDialRow, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerDialList}
	req.SetRunnerDialList(protocol.RunnerDialListRequest{})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	l := resp.RunnerDialList()
	if resp.Kind != protocol.TaskControlKind_RunnerDialList || l == nil {
		return nil, fmt.Errorf("runner dial ls: unexpected response kind=%v", resp.Kind)
	}
	var body protocol.RunnerDialListBody
	if err := ReadResponseBody(ctx, c.Transport(), l.StreamId, &body); err != nil {
		return nil, fmt.Errorf("runner dial ls: %w", err)
	}
	rows := make([]RunnerDialRow, 0, len(body.Rows))
	for _, r := range body.Rows {
		row := RunnerDialRow{
			ID: r.Id, Label: string(r.Label), Key: append(ed25519.PublicKey(nil), r.Key[:]...),
			State: r.DialState, Runner: r.Runner, Answered: -1, LastError: string(r.LastError),
			AddedAt: time.UnixMilli(int64(r.AddedAtUnixMs)).UTC(), AddedBy: string(r.AddedBy), DuplicateOf: r.DuplicateOf,
		}
		for _, c := range r.Candidates {
			row.Candidates = append(row.Candidates, string(c.Text))
		}
		if r.Answered != 0xff {
			row.Answered = int(r.Answered)
		}
		if r.NextAttemptUnixMs != 0 {
			row.NextAttempt = time.UnixMilli(int64(r.NextAttemptUnixMs)).UTC()
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func RunnerDialRemoveWith(ctx context.Context, c taskControlClient, id uint32) (protocol.RunnerDialStatus, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerDialRemove}
	req.SetRunnerDialRemove(protocol.RunnerDialRemoveRequest{Id: id})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, err
	}
	r := resp.RunnerDialRemove()
	if resp.Kind != protocol.TaskControlKind_RunnerDialRemove || r == nil {
		return 0, fmt.Errorf("runner dial rm: unexpected response kind=%v", resp.Kind)
	}
	return r.Status, nil
}

func RunnerKickWith(ctx context.Context, c *Client, prefix string, force bool) (protocol.RunnerKickStatus, RunnerKickResult, error) {
	r := protocol.RunnerKickRequest{}
	if !r.SetPrefix([]byte(prefix)) {
		return 0, RunnerKickResult{}, fmt.Errorf("runner kick: id prefix too long")
	}
	r.SetForce(force)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerKick}
	req.SetRunnerKick(r)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, RunnerKickResult{}, err
	}
	k := resp.RunnerKick()
	if resp.Kind != protocol.TaskControlKind_RunnerKick || k == nil {
		return 0, RunnerKickResult{}, fmt.Errorf("runner kick: unexpected response kind=%v", resp.Kind)
	}
	if k.StreamId == 0 {
		return k.Status, RunnerKickResult{}, nil
	}
	var body protocol.RunnerKickBody
	if err := ReadResponseBody(ctx, c.Transport(), k.StreamId, &body); err != nil {
		return k.Status, RunnerKickResult{}, fmt.Errorf("runner kick: %w", err)
	}
	return k.Status, RunnerKickResult{
		Runner:       RunnerKeyUser{ID: body.Runner.Id, Hostname: string(body.Runner.Hostname)},
		Tasks:        body.Tasks,
		RemovedDials: body.RemovedDials,
	}, nil
}

// Short-lived forms for the harness-cli binary: dial, ask, close.
func RunnerDialAdd(ctx context.Context, server objproto.ConnectionID, candidates []string, keyPrefix, label string) (protocol.RunnerDialStatus, uint32, error) {
	c, err := Dial(ctx, server, protocol.ClientKind_Cli)
	if err != nil {
		return 0, 0, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return RunnerDialAddWith(ctx, c, candidates, keyPrefix, label)
}

func RunnerDialList(ctx context.Context, server objproto.ConnectionID) ([]RunnerDialRow, error) {
	c, err := Dial(ctx, server, protocol.ClientKind_Cli)
	if err != nil {
		return nil, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return RunnerDialListWith(ctx, c)
}

func RunnerDialRemove(ctx context.Context, server objproto.ConnectionID, id uint32) (protocol.RunnerDialStatus, error) {
	c, err := Dial(ctx, server, protocol.ClientKind_Cli)
	if err != nil {
		return 0, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return RunnerDialRemoveWith(ctx, c, id)
}

func RunnerKick(ctx context.Context, server objproto.ConnectionID, prefix string, force bool) (protocol.RunnerKickStatus, RunnerKickResult, error) {
	c, err := Dial(ctx, server, protocol.ClientKind_Cli)
	if err != nil {
		return 0, RunnerKickResult{}, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return RunnerKickWith(ctx, c, prefix, force)
}

// FormatRunnerDialAdd renders `runner dial add`; ok is false for a refusal.
func FormatRunnerDialAdd(st protocol.RunnerDialStatus, id uint32, candidates []string) (string, bool) {
	switch st {
	case protocol.RunnerDialStatus_Ok:
		return fmt.Sprintf("dial %s added: %s", FormatDialID(id), strings.Join(candidates, ", ")), true
	case protocol.RunnerDialStatus_InvalidCandidates:
		return "runner dial add: invalid candidates (1-8 of transport:host:port-id, 512 bytes in all, a transport this server listens on; label at most 64 bytes)", false
	case protocol.RunnerDialStatus_KeyNotFound:
		return "runner dial add: no enrolled key has that prefix (see runner keys)", false
	case protocol.RunnerDialStatus_AmbiguousKey:
		return "runner dial add: more than one enrolled key has that prefix; give more of it", false
	case protocol.RunnerDialStatus_DuplicateCandidate:
		return "runner dial add: a candidate is already another record's (see runner dial ls)", false
	}
	return fmt.Sprintf("runner dial add: %v", st), false
}

func FormatRunnerDialRemove(st protocol.RunnerDialStatus, id uint32) (string, bool) {
	if st == protocol.RunnerDialStatus_Ok {
		return fmt.Sprintf("dial %s removed; its runner stays connected until it drops (runner kick to disconnect it)", FormatDialID(id)), true
	}
	if st == protocol.RunnerDialStatus_NotFound {
		return "runner dial rm: no dial record " + FormatDialID(id), false
	}
	return fmt.Sprintf("runner dial rm %s: %v", FormatDialID(id), st), false
}

func shortRunner(id protocol.RunnerID) string { return hex.EncodeToString(id.Id[:4]) }

// FormatRunnerDialList renders `runner dial ls`: a line per record, its
// candidates under it, then the count — printed at zero too.
func FormatRunnerDialList(rows []RunnerDialRow) string {
	var sb strings.Builder
	var zero protocol.RunnerID
	for _, r := range rows {
		runner := "-"
		if r.Runner != zero {
			runner = shortRunner(r.Runner)
		}
		fmt.Fprintf(&sb, "%s state=%s label=%s key=%s runner=%s", FormatDialID(r.ID), r.State, r.Label, KeyPrefix16(r.Key), runner)
		if r.State == protocol.RunnerDialState_Connected && r.Answered >= 0 && r.Answered < len(r.Candidates) {
			fmt.Fprintf(&sb, " answered=%s", r.Candidates[r.Answered])
		}
		if r.State == protocol.RunnerDialState_Waiting {
			fmt.Fprintf(&sb, " next=%s err=%q", r.NextAttempt.UTC().Format(time.RFC3339), r.LastError)
		}
		if r.State == protocol.RunnerDialState_Duplicate {
			fmt.Fprintf(&sb, " duplicate_of=%s", FormatDialID(r.DuplicateOf))
		}
		fmt.Fprintf(&sb, " by=%s added=%s\n", r.AddedBy, r.AddedAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(&sb, "  candidates: %s\n", strings.Join(r.Candidates, ", "))
	}
	fmt.Fprintf(&sb, "records=%d\n", len(rows))
	return sb.String()
}

type runnerDialJSON struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Key         string   `json:"key"`
	Candidates  []string `json:"candidates"`
	State       string   `json:"state"`
	Runner      string   `json:"runner"` // "" when none
	Answered    int      `json:"answered"`
	NextAttempt string   `json:"next_attempt"` // "" unless waiting
	LastError   string   `json:"last_error"`
	AddedAt     string   `json:"added_at"`
	AddedBy     string   `json:"added_by"`
	DuplicateOf string   `json:"duplicate_of"` // "" unless duplicate
}

// FormatRunnerDialListJSON renders `runner dial ls --json`: JSON Lines, every
// field on every record.
func FormatRunnerDialListJSON(rows []RunnerDialRow) string {
	var sb strings.Builder
	var zero protocol.RunnerID
	for _, r := range rows {
		j := runnerDialJSON{ID: FormatDialID(r.ID), Label: r.Label, Key: hex.EncodeToString(r.Key),
			Candidates: append([]string{}, r.Candidates...), State: r.State.String(), Answered: r.Answered,
			LastError: r.LastError, AddedAt: r.AddedAt.UTC().Format(time.RFC3339), AddedBy: r.AddedBy}
		if r.Runner != zero {
			j.Runner = r.Runner.Hex()
		}
		if !r.NextAttempt.IsZero() {
			j.NextAttempt = r.NextAttempt.UTC().Format(time.RFC3339)
		}
		if r.DuplicateOf != 0 {
			j.DuplicateOf = FormatDialID(r.DuplicateOf)
		}
		b, _ := json.Marshal(j)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func dialIDList(ids []uint32) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = FormatDialID(id)
	}
	return strings.Join(parts, " ")
}

// FormatRunnerKick renders `runner kick`; ok is false for a refusal.
func FormatRunnerKick(st protocol.RunnerKickStatus, res RunnerKickResult) (string, bool) {
	who := shortRunner(res.Runner.ID) + " " + res.Runner.Hostname
	removed := "removed dial record(s) " + dialIDList(res.RemovedDials)
	if len(res.RemovedDials) == 0 {
		removed = "removed 0 dial record(s)"
	}
	switch st {
	case protocol.RunnerKickStatus_Left:
		return fmt.Sprintf("kicked %s: it left; ended %d task(s); %s", who, len(res.Tasks), removed), true
	case protocol.RunnerKickStatus_ClosedWithoutLeaving:
		return fmt.Sprintf("kicked %s: the runner did not leave (too old to know the request?); a dial-mode runner reconnects. "+
			"The server closed its connection; ended %d task(s); %s", who, len(res.Tasks), removed), true
	case protocol.RunnerKickStatus_InUse:
		var sb strings.Builder
		fmt.Fprintf(&sb, "refused: %s has %d live task(s) (pass --force to end them):\n", who, len(res.Tasks))
		for _, t := range res.Tasks {
			fmt.Fprintf(&sb, "  %s\n", hex.EncodeToString(t.Id[:]))
		}
		return strings.TrimSuffix(sb.String(), "\n"), false
	case protocol.RunnerKickStatus_NotFound:
		return "runner kick: no registered runner has that id prefix (8-32 hex of the id= column)", false
	case protocol.RunnerKickStatus_AmbiguousPrefix:
		return "runner kick: more than one runner has that id prefix; give more of it", false
	}
	return fmt.Sprintf("runner kick: %v", st), false
}
