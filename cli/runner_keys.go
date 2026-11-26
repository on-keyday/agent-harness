package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// The runner key verbs: runner enroll / keys / unenroll. Every surface calls
// the *With forms on the client it already holds and prints through the
// Format* functions below, so the three say the same thing.

// RunnerKeyUser is a registered runner, as `runner keys` lists it.
type RunnerKeyUser struct {
	ID       protocol.RunnerID
	Hostname string
}

// RunnerKeyRow is one enrolled key.
type RunnerKeyRow struct {
	Key        ed25519.PublicKey
	Label      string
	EnrolledAt time.Time
	EnrolledBy string
	Runners    []RunnerKeyUser
	// Bound is the tasks a runner on this key may take; nil = unbound
	// (bound enrollment, C2).
	Bound []protocol.TaskID
}

// RunnerUnenrollResult is what unenroll answered about: the key (in full, for
// the undo) and the runners it closed — or, refused, the runners in the way.
type RunnerUnenrollResult struct {
	Key     ed25519.PublicKey
	Label   string
	Runners []RunnerKeyUser
	// RemovedDials are the dial records bound to the key, removed with it.
	RemovedDials []uint32
	// Bound is the removed key's binding, so the undo restores it.
	Bound []protocol.TaskID
}

func runnerKeyUsers(us []protocol.RunnerKeyUser) []RunnerKeyUser {
	out := make([]RunnerKeyUser, 0, len(us))
	for _, u := range us {
		out = append(out, RunnerKeyUser{ID: u.Id, Hostname: string(u.Hostname)})
	}
	return out
}

// RunnerEnrollWith enrolls key; bound, when non-empty, binds it to those
// tasks (bound enrollment, C2), which the server checks against the caller's
// runner_enroll scope.
func RunnerEnrollWith(ctx context.Context, c taskControlClient, key ed25519.PublicKey, label string, bound []protocol.TaskID) (protocol.RunnerKeyStatus, error) {
	if len(key) != ed25519.PublicKeySize {
		return 0, fmt.Errorf("runner enroll: a key is %d bytes, got %d", ed25519.PublicKeySize, len(key))
	}
	r := protocol.RunnerEnrollRequest{}
	copy(r.Key[:], key)
	if !r.SetLabel([]byte(label)) {
		return 0, fmt.Errorf("runner enroll: label too long (%d bytes, max 255)", len(label))
	}
	if !r.SetBound(bound) {
		return 0, fmt.Errorf("runner enroll: %d bound tasks is too many", len(bound))
	}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_RunnerEnroll}}
	req.SetRunnerEnroll(r)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, err
	}
	if resp.Kind != protocol.TaskControlKind_RunnerEnroll || resp.RunnerEnroll() == nil {
		return 0, fmt.Errorf("runner enroll: unexpected response kind=%v", resp.Kind)
	}
	return resp.RunnerEnroll().Status, nil
}

func RunnerKeysWith(ctx context.Context, c *Client) ([]RunnerKeyRow, error) {
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_RunnerKeys}}
	req.SetRunnerKeys(protocol.RunnerKeysRequest{})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	rk := resp.RunnerKeys()
	if resp.Kind != protocol.TaskControlKind_RunnerKeys || rk == nil {
		return nil, fmt.Errorf("runner keys: unexpected response kind=%v", resp.Kind)
	}
	var body protocol.RunnerKeysBody
	if err := ReadResponseBody(ctx, c.Transport(), rk.StreamId, &body); err != nil {
		return nil, fmt.Errorf("runner keys: %w", err)
	}
	rows := make([]RunnerKeyRow, 0, len(body.Keys))
	for _, r := range body.Keys {
		rows = append(rows, RunnerKeyRow{
			Key:        append(ed25519.PublicKey(nil), r.Key[:]...),
			Label:      string(r.Label),
			EnrolledAt: time.UnixMilli(int64(r.EnrolledAtUnixMs)).UTC(),
			EnrolledBy: string(r.EnrolledBy),
			Runners:    runnerKeyUsers(r.Runners),
			Bound:      r.Bound,
		})
	}
	return rows, nil
}

func RunnerUnenrollWith(ctx context.Context, c *Client, prefix string, force bool) (protocol.RunnerKeyStatus, RunnerUnenrollResult, error) {
	r := protocol.RunnerUnenrollRequest{}
	r.SetPrefix([]byte(prefix))
	r.SetForce(force)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_RunnerUnenroll}}
	req.SetRunnerUnenroll(r)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, RunnerUnenrollResult{}, err
	}
	u := resp.RunnerUnenroll()
	if resp.Kind != protocol.TaskControlKind_RunnerUnenroll || u == nil {
		return 0, RunnerUnenrollResult{}, fmt.Errorf("runner unenroll: unexpected response kind=%v", resp.Kind)
	}
	if u.StreamId == 0 {
		return u.Status, RunnerUnenrollResult{}, nil
	}
	var body protocol.RunnerUnenrollBody
	if err := ReadResponseBody(ctx, c.Transport(), u.StreamId, &body); err != nil {
		return u.Status, RunnerUnenrollResult{}, fmt.Errorf("runner unenroll: %w", err)
	}
	return u.Status, RunnerUnenrollResult{
		Key:          append(ed25519.PublicKey(nil), body.Key[:]...),
		Label:        string(body.Label),
		Runners:      runnerKeyUsers(body.Runners),
		RemovedDials: body.RemovedDials,
		Bound:        body.Bound,
	}, nil
}

// RunnerEnroll / RunnerKeys / RunnerUnenroll are the short-lived forms for the
// harness-cli binary: dial, ask, close.
func RunnerEnroll(ctx context.Context, server objproto.ConnectionID, key ed25519.PublicKey, label string, bound []protocol.TaskID) (protocol.RunnerKeyStatus, error) {
	c, err := Dial(ctx, server, protocol.ClientKind_Cli)
	if err != nil {
		return 0, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return RunnerEnrollWith(ctx, c, key, label, bound)
}

func RunnerKeys(ctx context.Context, server objproto.ConnectionID) ([]RunnerKeyRow, error) {
	c, err := Dial(ctx, server, protocol.ClientKind_Cli)
	if err != nil {
		return nil, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return RunnerKeysWith(ctx, c)
}

func RunnerUnenroll(ctx context.Context, server objproto.ConnectionID, prefix string, force bool) (protocol.RunnerKeyStatus, RunnerUnenrollResult, error) {
	c, err := Dial(ctx, server, protocol.ClientKind_Cli)
	if err != nil {
		return 0, RunnerUnenrollResult{}, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return RunnerUnenrollWith(ctx, c, prefix, force)
}

// KeyPrefix16 is how a key is shown: its first 16 hex characters.
func KeyPrefix16(k ed25519.PublicKey) string {
	h := hex.EncodeToString(k)
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

func runnerUserLines(sb *strings.Builder, us []RunnerKeyUser) {
	for _, u := range us {
		fmt.Fprintf(sb, "  %s %s\n", hex.EncodeToString(u.ID.Id[:4]), u.Hostname)
	}
}

// BindingLabel is how a key's binding is shown in a row: "unbound", or each
// bound task's first 8 hex. Always a value, never elided: "unbound" is an
// answer, and the widest one.
func BindingLabel(ids []protocol.TaskID) string {
	if len(ids) == 0 {
		return "unbound"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = hex.EncodeToString(id.Id[:4])
	}
	return strings.Join(parts, ",")
}

// BindingIDs is a binding in full, as `runner enroll --bind` takes it.
func BindingIDs(ids []protocol.TaskID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = hex.EncodeToString(id.Id[:])
	}
	return strings.Join(parts, ",")
}

// FormatRunnerEnroll is the one result line of `runner enroll`.
func FormatRunnerEnroll(st protocol.RunnerKeyStatus, key ed25519.PublicKey, label string, bound []protocol.TaskID) string {
	switch st {
	case protocol.RunnerKeyStatus_Ok:
		return fmt.Sprintf("enrolled %s label=%s binding=%s", KeyPrefix16(key), label, BindingLabel(bound))
	case protocol.RunnerKeyStatus_OutOfReach:
		if len(bound) == 0 {
			return fmt.Sprintf("runner enroll %s: refused: an unbound key needs runner_enroll at global scope; bind it with --bind to tasks inside your runner_enroll scope", KeyPrefix16(key))
		}
		return fmt.Sprintf("runner enroll %s: refused: a --bind task is outside your runner_enroll scope", KeyPrefix16(key))
	case protocol.RunnerKeyStatus_BindingDiffers:
		return fmt.Sprintf("runner enroll %s: refused: the key is enrolled with another binding, which is fixed at enroll; unenroll it and enroll again to change it", KeyPrefix16(key))
	case protocol.RunnerKeyStatus_BadBinding:
		return fmt.Sprintf("runner enroll %s: refused: --bind takes at most 16 tasks, none all-zero", KeyPrefix16(key))
	case protocol.RunnerKeyStatus_AlreadyEnrolled:
		if label != "" {
			return fmt.Sprintf("already enrolled %s, label=%s", KeyPrefix16(key), label)
		}
		return "already enrolled " + KeyPrefix16(key)
	}
	return fmt.Sprintf("runner enroll %s: %v", KeyPrefix16(key), st)
}

// FormatRunnerKeys renders `runner keys`: a block per key, then the number of
// keys. Counts are printed even at zero: zero is a measurement, and with no key
// enrolled the count is the whole answer.
func FormatRunnerKeys(rows []RunnerKeyRow) string {
	var sb strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&sb, "%s label=%s enrolled=%s by=%s binding=%s runners=%d\n",
			KeyPrefix16(r.Key), r.Label, r.EnrolledAt.UTC().Format(time.RFC3339), r.EnrolledBy, BindingLabel(r.Bound), len(r.Runners))
		runnerUserLines(&sb, r.Runners)
	}
	fmt.Fprintf(&sb, "keys=%d\n", len(rows))
	return sb.String()
}

type runnerKeyJSON struct {
	Key        string              `json:"key"`
	Label      string              `json:"label"`
	EnrolledAt string              `json:"enrolled_at"`
	EnrolledBy string              `json:"enrolled_by"`
	Runners    []runnerKeyUserJSON `json:"runners"`
	// BoundTasks is the binding in full; [] when unbound, never absent.
	BoundTasks []string `json:"bound_tasks"`
}

type runnerKeyUserJSON struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}

// FormatRunnerKeysJSON renders `runner keys --json`: JSON Lines, one object per
// key, the key and every runner id in full. runners is [] rather than absent
// for a key nobody uses.
func FormatRunnerKeysJSON(rows []RunnerKeyRow) string {
	var sb strings.Builder
	for _, r := range rows {
		j := runnerKeyJSON{
			Key:        hex.EncodeToString(r.Key),
			Label:      r.Label,
			EnrolledAt: r.EnrolledAt.UTC().Format(time.RFC3339),
			EnrolledBy: r.EnrolledBy,
			Runners:    make([]runnerKeyUserJSON, 0, len(r.Runners)),
			BoundTasks: boundTasksJSON(r.Bound),
		}
		for _, u := range r.Runners {
			j.Runners = append(j.Runners, runnerKeyUserJSON{ID: u.ID.Hex(), Hostname: u.Hostname})
		}
		b, _ := json.Marshal(j)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// FormatRunnerUnenroll renders `runner unenroll`; ok is false for every answer
// but a removal, so the CLI exits non-zero on it.
func FormatRunnerUnenroll(st protocol.RunnerKeyStatus, res RunnerUnenrollResult) (string, bool) {
	var sb strings.Builder
	switch st {
	case protocol.RunnerKeyStatus_Ok:
		fmt.Fprintf(&sb, "unenrolled %s label=%s, closed %d runner(s)\n", KeyPrefix16(res.Key), res.Label, len(res.Runners))
		runnerUserLines(&sb, res.Runners)
		fmt.Fprintf(&sb, "removed %d dial record(s)", len(res.RemovedDials))
		if len(res.RemovedDials) > 0 {
			sb.WriteString(": " + dialIDList(res.RemovedDials))
		}
		sb.WriteString("\n")
		undo := "undo: harness-cli runner enroll " + hex.EncodeToString(res.Key)
		if res.Label != "" {
			undo += " --label " + shellWord(res.Label)
		}
		if len(res.Bound) > 0 {
			undo += " --bind " + BindingIDs(res.Bound)
		}
		sb.WriteString(undo + "\n")
		return sb.String(), true
	case protocol.RunnerKeyStatus_InUse:
		fmt.Fprintf(&sb, "refused: %d runner(s) use %s (pass --force to cut them off):\n", len(res.Runners), KeyPrefix16(res.Key))
		runnerUserLines(&sb, res.Runners)
		return sb.String(), false
	case protocol.RunnerKeyStatus_NotFound:
		return "runner unenroll: no enrolled key has that prefix\n", false
	case protocol.RunnerKeyStatus_AmbiguousPrefix:
		return "runner unenroll: more than one enrolled key has that prefix; give more of it\n", false
	}
	return fmt.Sprintf("runner unenroll: %v\n", st), false
}

// shellWord quotes s for a POSIX shell when it holds anything but plain word
// characters: the undo line is pasted after a mistaken --force, so it must run
// as printed.
func shellWord(s string) string {
	plain := s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.,:/@+=", r))
	}) < 0
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// boundTasksJSON is a binding as JSON: full ids, [] when unbound.
func boundTasksJSON(ids []protocol.TaskID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, hex.EncodeToString(id.Id[:]))
	}
	return out
}
