package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// ActivityBusyThreshold aliases the wire-shared busy/idle cut so existing
// cli callers keep compiling; see protocol.ActivityBusyThreshold for the
// basis and the server-side counterpart (the task_activity event watcher).
const ActivityBusyThreshold = protocol.ActivityBusyThreshold

// ActivityStr renders the busy/idle badge for a live interactive session
// from the server-computed idle age (TaskInfo.output_idle_ms; caller must
// have checked last_output_at > 0). Shared by the CLI `ls` renderer and the
// TUI task table. Takes the wire value, NOT a locally-derived age: client
// and server run on different hosts and clock skew would distort a local
// now()-timestamp derivation.
func ActivityStr(outputIdleMs uint64) string {
	idle := time.Duration(outputIdleMs) * time.Millisecond
	if idle < ActivityBusyThreshold {
		return "busy"
	}
	if idle >= time.Minute {
		return fmt.Sprintf("idle:%dm", int(idle/time.Minute))
	}
	return fmt.Sprintf("idle:%ds", int(idle/time.Second))
}

// Snapshot queries the server for all runners + recent tasks and returns the
// decoded ListResultBody. The wire response carries only a stream id; the
// body is read from the trsf send-stream the server opens (so the payload
// fits within UDP path MTU regardless of how many tasks the server holds).
//
// Both the human-readable List and the TUI/webui code paths share this
// helper so the RoundTripTaskControl + stream-decode logic exists in exactly
// one place.
func (c *Client) Snapshot(ctx context.Context) (*protocol.ListResultBody, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
	req.SetList(protocol.ListQuery{Query: nil})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.List()
	if lr == nil {
		return nil, fmt.Errorf("expected List response, got kind=%v", resp.Kind)
	}
	if lr.StreamId == 0 {
		return nil, fmt.Errorf("server returned no stream id (could not allocate)")
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(lr.StreamId))
	if st == nil {
		return nil, fmt.Errorf("list stream %d not visible after response", lr.StreamId)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("list stream read: %w", err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			break
		}
	}
	body := &protocol.ListResultBody{}
	if err := body.DecodeExact(raw); err != nil {
		return nil, fmt.Errorf("decode ListResultBody (%d bytes): %w", len(raw), err)
	}
	return body, nil
}

// List queries the server for all runners + recent tasks and writes a human-
// readable summary to out. Method form: callable repeatedly without re-dialing.
func (c *Client) List(ctx context.Context, out io.Writer) error {
	lr, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	renderList(lr, out)
	return nil
}

// ListJSON is the --json counterpart of List: it snapshots the server and writes
// a single JSON object ({"runners":[...],"tasks":[...]}) to out. A single object
// (rather than JSON Lines) keeps the two heterogeneous lists in one jq-navigable
// document, and empty lists render as [] (not null) so `.runners | length` never
// errors.
func (c *Client) ListJSON(ctx context.Context, out io.Writer) error {
	lr, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	renderListJSON(lr, out)
	return nil
}

// SessionListJSON snapshots the server and writes one JSON object per line
// (JSON Lines) for each interactive session — the `session ls` output. Each
// row shares `ls --json`'s task vocabulary plus is_attached/ring_buffer_bytes.
func (c *Client) SessionListJSON(ctx context.Context, out io.Writer) error {
	lr, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	renderSessionsJSON(lr, out)
	return nil
}

// renderList writes a human-readable summary of a ListResult to out.
// Extracted for testability: tests can construct a ListResult directly without
// a live server and call renderList to verify the rendered columns.
func renderList(lr *protocol.ListResultBody, out io.Writer) {
	fmt.Fprintln(out, "RUNNERS")
	if len(lr.Runners) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, r := range lr.Runners {
		roots := make([]string, len(r.AllowedRoots))
		for i, ar := range r.AllowedRoots {
			roots[i] = string(ar.Path)
		}
		fmt.Fprintf(out, "  %s  host=%s  tasks=%d/%d  %s  roots=%s  id=%s\n",
			runnerStatusStr(r.Status),
			string(r.Hostname),
			len(r.ActiveTasks),
			r.MaxTasks,
			agentProfilesStr(r.AgentProfiles, string(r.AgentBin), r.SkillsInjected()),
			strings.Join(roots, ","),
			protocol.RunnerIDToConnID(r.Id).String(),
		)
	}

	// Index runners by ConnID string so each task can show its runner's agent.
	runnerByID := make(map[string]protocol.RunnerInfo, len(lr.Runners))
	for _, r := range lr.Runners {
		runnerByID[protocol.RunnerIDToConnID(r.Id).String()] = r
	}
	fmt.Fprintln(out, "TASKS")
	if len(lr.Tasks) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, t := range lr.Tasks {
		// Prefer the task's own resolved agent profile (which can differ from
		// its runner's default AgentBin on a multi-profile runner or after a
		// cross-agent resume); fall back to the runner descriptor for tasks
		// predating the field.
		agent := ""
		if len(t.AgentProfile) > 0 {
			agent = "  agent=" + string(t.AgentProfile)
		} else if r, ok := runnerByID[protocol.RunnerIDToConnID(t.AssignedTo).String()]; ok {
			agent = "  " + agentStr(string(r.AgentBin), r.SkillsInjected())
		}
		// exit= / err= render only when meaningful so the common rows stay
		// short: exit= for a finished task with a non-zero code, err= for a
		// server- or runner-recorded failure reason (e.g. runner_disconnected
		// — which marks a resumable task, not a dead one).
		suffix := ""
		if t.EndedAt > 0 && t.ExitCode != 0 {
			suffix += fmt.Sprintf("  exit=%d", t.ExitCode)
		}
		if len(t.ErrorMessage) > 0 {
			suffix += fmt.Sprintf("  err=%q", string(t.ErrorMessage))
		}
		resumedBy := ""
		if t.ResumedByKind != protocol.ClientKind_Unspecified {
			resumedBy = "  resumed_by=" + originStr(t.ResumedByKind)
		}
		act := ""
		if t.LastOutputAt > 0 {
			act = "  act=" + ActivityStr(t.OutputIdleMs)
		}
		createdBy := ""
		if t.CreatorTaskId.Id != ([16]byte{}) {
			createdBy = "  by=" + hex.EncodeToString(t.CreatorTaskId.Id[:])[:8]
		}
		caps := "  caps=" + CapsLabel(t.Capabilities)
		fmt.Fprintf(out, "  %s  %s  %s  repo=%s  from=%s%s%s%s%s%s  prompt=%q%s\n",
			taskIDStr(t.Id.Id[:]),
			taskStatusStr(t.Status),
			taskKindStr(t.Kind),
			string(t.RepoPath),
			originStr(t.OriginKind),
			agent,
			resumedBy,
			act,
			createdBy,
			caps,
			string(t.Prompt),
			suffix,
		)
	}
}

// runnerJSON is the single source of truth for the JSON shape of a runner row
// in `ls --json`. A struct (not map[string]any) gives deterministic field
// order. Fields mirror the text renderer's columns but as scriptable values:
// agents is the profile list (not the joined "+skills" display string) and
// skills_injected is broken out as its own bool.
type runnerJSON struct {
	Id             string   `json:"id"`
	Status         string   `json:"status"`
	Hostname       string   `json:"hostname"`
	ActiveTasks    int      `json:"active_tasks"`
	MaxTasks       uint16   `json:"max_tasks"`
	Agents         []string `json:"agents"`
	SkillsInjected bool     `json:"skills_injected"`
	Roots          []string `json:"roots"`
}

// taskJSON is the JSON shape of a task row in `ls --json`. Fields mirror the
// text renderer, plus the raw wire timestamps/idle counters (created_at,
// started_at, ended_at, last_output_at, output_idle_ms) that scripts want but
// the compact text line omits. Presence-gated text columns (agent, resumed_by,
// activity, created_by, error) are omitempty so a JSON row stays as small as
// its text counterpart when they don't apply.
type taskJSON struct {
	Id           string `json:"id"`
	Status       string `json:"status"`
	Kind         string `json:"kind"`
	Repo         string `json:"repo"`
	From         string `json:"from"`
	Agent        string `json:"agent,omitempty"`
	ResumedBy    string `json:"resumed_by,omitempty"`
	Activity     string `json:"activity,omitempty"`
	Caps         string `json:"caps"`
	CreatedBy    string `json:"created_by,omitempty"`
	Prompt       string `json:"prompt"`
	ExitCode     int32  `json:"exit_code"`
	ErrorMessage string `json:"error_message,omitempty"`
	CreatedAt    uint64 `json:"created_at"`
	StartedAt    uint64 `json:"started_at"`
	EndedAt      uint64 `json:"ended_at"`
	LastOutputAt uint64 `json:"last_output_at"`
	OutputIdleMs uint64 `json:"output_idle_ms"`
}

// listJSON is the top-level `ls --json` document.
type listJSON struct {
	Runners []runnerJSON `json:"runners"`
	Tasks   []taskJSON   `json:"tasks"`
}

// renderListJSON writes a ListResult as a single JSON object to out. Shares the
// same field derivations (agent profiles, caps label, activity, creator id) as
// renderList so the two views can never disagree. Extracted (like renderList)
// so tests can drive it without a live server.
func renderListJSON(lr *protocol.ListResultBody, out io.Writer) {
	// Non-nil zero-length slices so empty sections encode as [] not null.
	doc := listJSON{Runners: []runnerJSON{}, Tasks: []taskJSON{}}

	runnerByID := make(map[string]protocol.RunnerInfo, len(lr.Runners))
	for _, r := range lr.Runners {
		runnerByID[protocol.RunnerIDToConnID(r.Id).String()] = r
		roots := make([]string, len(r.AllowedRoots))
		for i, ar := range r.AllowedRoots {
			roots[i] = string(ar.Path)
		}
		doc.Runners = append(doc.Runners, runnerJSON{
			Id:             protocol.RunnerIDToConnID(r.Id).String(),
			Status:         runnerStatusJSON(r.Status),
			Hostname:       string(r.Hostname),
			ActiveTasks:    len(r.ActiveTasks),
			MaxTasks:       r.MaxTasks,
			Agents:         agentProfileNames(r.AgentProfiles, string(r.AgentBin)),
			SkillsInjected: r.SkillsInjected(),
			Roots:          roots,
		})
	}

	for i := range lr.Tasks {
		doc.Tasks = append(doc.Tasks, newTaskJSON(&lr.Tasks[i], runnerByID))
	}

	enc := json.NewEncoder(out)
	_ = enc.Encode(doc)
}

// newTaskJSON builds the shared per-task JSON view. Single source of truth for
// the task vocabulary of BOTH `ls --json` and `session ls`, so the overlapping
// fields can never drift in name, casing, or derivation. runnerByID resolves
// the task's runner for the agent-bin fallback (a task with no own profile
// shows its runner's default bin); pass nil to skip that fallback.
func newTaskJSON(t *protocol.TaskInfo, runnerByID map[string]protocol.RunnerInfo) taskJSON {
	// agent: prefer the task's own resolved profile, else its runner's default
	// bin — the same precedence renderList uses for the text column.
	agent := string(t.AgentProfile)
	if agent == "" && runnerByID != nil {
		if r, ok := runnerByID[protocol.RunnerIDToConnID(t.AssignedTo).String()]; ok {
			agent = string(r.AgentBin)
		}
	}
	resumedBy := ""
	if t.ResumedByKind != protocol.ClientKind_Unspecified {
		resumedBy = originStr(t.ResumedByKind)
	}
	activity := ""
	if t.LastOutputAt > 0 {
		activity = ActivityStr(t.OutputIdleMs)
	}
	return taskJSON{
		Id:           taskIDHexOrEmpty(t.Id.Id[:]),
		Status:       taskStatusJSON(t.Status),
		Kind:         taskKindJSON(t.Kind),
		Repo:         string(t.RepoPath),
		From:         originStr(t.OriginKind),
		Agent:        agent,
		ResumedBy:    resumedBy,
		Activity:     activity,
		Caps:         CapsLabel(t.Capabilities),
		CreatedBy:    taskIDHexOrEmpty(t.CreatorTaskId.Id[:]),
		Prompt:       string(t.Prompt),
		ExitCode:     t.ExitCode,
		ErrorMessage: string(t.ErrorMessage),
		CreatedAt:    t.CreatedAt,
		StartedAt:    t.StartedAt,
		EndedAt:      t.EndedAt,
		LastOutputAt: t.LastOutputAt,
		OutputIdleMs: t.OutputIdleMs,
	}
}

// sessionJSON is one `session ls` row: the shared taskJSON view (embedded, so
// its fields promote to top level and stay byte-identical to `ls --json`) plus
// the two session-only fields no other view carries — attach state and replay
// ring size. After this, `session ls` differs from `ls --json` ONLY by the
// interactive filter and these two extra fields.
type sessionJSON struct {
	taskJSON
	IsAttached      bool   `json:"is_attached"`
	RingBufferBytes uint64 `json:"ring_buffer_bytes"`
}

// renderSessionsJSON writes one JSON object per line (JSON Lines) for each
// interactive session in lr. Extracted (like renderListJSON) so tests drive it
// without a live server. JSON Lines — not a single object — because a session
// list is commonly piped one-per-line (xargs/while-read) to attach/kill.
func renderSessionsJSON(lr *protocol.ListResultBody, out io.Writer) {
	runnerByID := make(map[string]protocol.RunnerInfo, len(lr.Runners))
	for _, r := range lr.Runners {
		runnerByID[protocol.RunnerIDToConnID(r.Id).String()] = r
	}
	enc := json.NewEncoder(out)
	for i := range lr.Tasks {
		t := &lr.Tasks[i]
		if t.Kind != protocol.TaskKind_Interactive {
			continue
		}
		_ = enc.Encode(sessionJSON{
			taskJSON:        newTaskJSON(t, runnerByID),
			IsAttached:      t.IsAttached(),
			RingBufferBytes: t.RingBufferBytes,
		})
	}
}

// taskIDHexOrEmpty returns the full hex encoding of a task id, or "" when every
// byte is zero. The JSON counterpart of taskIDStr's "-" sentinel: an empty
// string is the natural "absent" value for a scripting consumer (matches the
// whoami --json convention).
func taskIDHexOrEmpty(b []byte) string {
	if s := taskIDStr(b); s != "-" {
		return s
	}
	return ""
}

// runnerStatusJSON renders RunnerStatus as a lowercase scripting token, the
// trimmed counterpart of the fixed-width runnerStatusStr.
func runnerStatusJSON(s protocol.RunnerStatus) string {
	switch s {
	case protocol.RunnerStatus_Idle:
		return "idle"
	case protocol.RunnerStatus_Busy:
		return "busy"
	default:
		return "offline"
	}
}

// taskStatusJSON renders TaskStatus as a lowercase scripting token, the trimmed
// counterpart of the fixed-width taskStatusStr.
func taskStatusJSON(s protocol.TaskStatus) string {
	return strings.ToLower(strings.TrimSpace(taskStatusStr(s)))
}

// taskKindJSON renders TaskKind as a lowercase scripting token (oneshot /
// interactive / "?"), the trimmed counterpart of taskKindStr.
func taskKindJSON(k protocol.TaskKind) string {
	return strings.TrimSpace(taskKindStr(k))
}

// agentProfileNames returns a runner's advertised agent profile names, falling
// back to [bin] when the runner advertised none (legacy). Empty bin yields an
// empty slice. Shared derivation behind agentProfilesStr's display string and
// runnerJSON.Agents so the text and JSON views agree.
func agentProfileNames(profiles []protocol.AgentProfileName, bin string) []string {
	if len(profiles) == 0 {
		if bin == "" {
			return []string{}
		}
		return []string{bin}
	}
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = string(p.Name)
	}
	return names
}

// agentStr renders a peer's agent descriptor for the ls output: the agent
// binary basename, plus "+skills" when the runner injects the harness skill.
// Empty bin renders as "?".
func agentStr(bin string, injected bool) string {
	if bin == "" {
		bin = "?"
	}
	if injected {
		return "agent=" + bin + "+skills"
	}
	return "agent=" + bin
}

// agentProfilesStr renders a runner's agent descriptor extended to its full
// advertised profile set: a multi-profile runner shows
// "agent=claude,codex" (or "+skills") instead of just its process-level
// AgentBin. A legacy runner that advertised no AgentProfiles falls back to
// agentStr(bin, injected), unchanged. Mirrors the TUI's
// agentProfilesDescriptor / WebUI picker so all three UIs agree.
func agentProfilesStr(profiles []protocol.AgentProfileName, bin string, injected bool) string {
	if len(profiles) == 0 {
		return agentStr(bin, injected)
	}
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = string(p.Name)
	}
	desc := "agent=" + strings.Join(names, ",")
	if injected {
		desc += "+skills"
	}
	return desc
}

// originStr formats a ClientKind for the `from=` column. Unspecified renders
// as "-" so a row visibly shows "no recorded origin" rather than the
// confusingly literal "unspecified" / "Unspecified" enum name.
func originStr(k protocol.ClientKind) string {
	if k == protocol.ClientKind_Unspecified {
		return "-"
	}
	return strings.ToLower(k.String())
}

// List (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).List instead.
func List(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.List(ctx, out)
}

// ListJSON is the package-level --json wrapper: opens a fresh Client, writes the
// snapshot as one JSON object, and closes. Mirrors List for short-lived CLI use.
func ListJSON(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.ListJSON(ctx, out)
}

// SessionListJSON is the package-level wrapper for `session ls`: opens a fresh
// Client, writes interactive sessions as JSON Lines, and closes.
func SessionListJSON(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.SessionListJSON(ctx, out)
}

func runnerStatusStr(s protocol.RunnerStatus) string {
	switch s {
	case protocol.RunnerStatus_Idle:
		return "Idle   "
	case protocol.RunnerStatus_Busy:
		return "Busy   "
	default:
		return "Offline"
	}
}

// taskKindStr renders TaskKind as a fixed-width column. Vocabulary matches
// the TUI detail popup (tui/detail.go taskKindStr): oneshot / interactive.
// An interactive row explains its empty prompt= by itself; a oneshot row
// with an empty prompt really was submitted with one.
func taskKindStr(k protocol.TaskKind) string {
	switch k {
	case protocol.TaskKind_Oneshot:
		return "oneshot    "
	case protocol.TaskKind_Interactive:
		return "interactive"
	}
	return "?          "
}

func taskStatusStr(s protocol.TaskStatus) string {
	switch s {
	case protocol.TaskStatus_Queued:
		return "Queued   "
	case protocol.TaskStatus_Running:
		return "Running  "
	case protocol.TaskStatus_Succeeded:
		return "Succeeded"
	case protocol.TaskStatus_Failed:
		return "Failed   "
	case protocol.TaskStatus_Cancelled:
		return "Cancelled"
	case protocol.TaskStatus_Detached:
		return "Detached "
	}
	return "?"
}

// taskIDStr returns the full hex encoding of b, or "-" if every byte is zero.
// Full length is required so the printed value can be copy-pasted directly
// into harness-cli subcommands (cancel / logs / file push / file pull / ...).
func taskIDStr(b []byte) string {
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "-"
	}
	const tab = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, v := range b {
		out = append(out, tab[v>>4], tab[v&0xf])
	}
	return string(out)
}
