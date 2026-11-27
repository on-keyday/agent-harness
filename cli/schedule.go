package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"

	// The zone database, embedded: CLI, TUI and wasm render a schedule's zone
	// from one source whatever the host has (R18; the server embeds it too, S-D3).
	_ "time/tzdata"
)

// Server-kept cron schedules (pillar S,
// docs/superpowers/specs/2026-11-15-pillar-s-schedules-design.md): the client
// methods the CLI, the TUI and the wasm bridge share, the ONE builder from the
// bound flags to the request, and the text and JSON the rows and the detail
// render as.

// --- the request: ScheduleSpecOpts, its one builder, its one encoder ---

// ScheduleSpecOpts is what a schedule new / edit names. Every ScheduleSpec
// field is here with its presence: a zero value means "not named", which on an
// edit leaves the stored value alone (checklist 25, 33). The pointer and
// *Set fields exist where the zero value is itself an answer (--oneshot=false,
// --caps none, --scope subtree).
//
// It is filled by ScheduleSpecOptsFromAction and nowhere else
// (TestScheduleSpecOptsHasOneBuilder), so a field added to the spec cannot be
// set by one surface and missed by another (checklist 28a).
type ScheduleSpecOpts struct {
	Cron   string // "" = not named
	Tz     string
	Repo   string
	TaskID string // hex; --task: bind this task (S-D14)
	Agent  string

	Selector  *protocol.RunnerSelector
	ExtraArgs []string // nil = not named; named ones replace the stored list

	ResumeConversation *bool
	Oneshot            *bool
	Kind               *protocol.TaskKind // --stream: stream, --stream=false: interactive; nil = not named
	Record             *bool              // --record true, --no-record false
	Pty                *bool              // --pty true, --no-pty false

	// The authority block, as OpenInteractiveRequest's: Scope gates Overrides
	// too (ScopePresent), exactly as --scope / --scope-for do.
	Caps         *protocol.Capability
	ScopePresent bool
	Scope        protocol.TaskScope
	Overrides    []protocol.ScopeOverride
	// GrantConditions name overrides by their exact capability list (grant
	// conditions C4); the bodies ride their own stream beside the prompt's.
	GrantConditions []protocol.GrantCondition
	DelegateCaps    *protocol.Capability
	DelegateScope   *protocol.TaskScope
	SpawnDepth      *protocol.SpawnLimit
	MaxDescendants  *protocol.SpawnLimit
	// grantConditionsStream is the stream scheduleBodies named for the
	// conditions; buildScheduleSpec writes it onto the spec.
	grantConditionsStream uint64

	// The run's conditions, each present only when named, so a zero is an
	// answer the server reads (R17): QueueTimeout 0 = none, Quiet / Blocked 0 =
	// the default, TurnEnd unset = any, Deadline 0 refused. The server overlays
	// only the present ones.
	Deadline, Quiet, Blocked, QueueTimeout *time.Duration
	TurnEnd                                *protocol.TurnEndSignal

	Prompt string // "" = not named

	// SizeSet: Rows and Cols are sent. A create always sends the background
	// open's 40x120 default; an edit only when both were named.
	SizeSet    bool
	Rows, Cols uint16
}

// ScheduleSpecOptsFromAction is THE builder: the one place a schedule new /
// edit's parsed flags become ScheduleSpecOpts. The CLI, the TUI and the wasm
// bridge all call it.
func ScheduleSpecOptsFromAction(a verb.ScheduleSpecAction) (ScheduleSpecOpts, error) {
	isNew := a.Sub == verb.SubNew
	o := ScheduleSpecOpts{
		Cron: a.Cron, Tz: a.Tz, Repo: a.Repo, TaskID: a.TaskID, Agent: a.Agent,
		Prompt: a.Prompt, ExtraArgs: nonEmpty(a.ExtraArgs),
		Caps: a.Caps, DelegateCaps: a.DelegateCaps, DelegateScope: a.DelegateScope,
		SpawnDepth: a.SpawnDepth, MaxDescendants: a.MaxDescendants,
	}
	if a.TaskID != "" {
		if _, err := parseTaskIDHex(a.TaskID); err != nil {
			return ScheduleSpecOpts{}, fmt.Errorf("--task: %w", err)
		}
	}
	if err := checkPrompt(a.Prompt); err != nil {
		return ScheduleSpecOpts{}, err
	}
	if len(a.Cron) > protocol.ScheduleCronMax {
		return ScheduleSpecOpts{}, fmt.Errorf("--cron is %d bytes, at most %d", len(a.Cron), protocol.ScheduleCronMax)
	}
	if len(a.Tz) > protocol.ScheduleZoneMax {
		return ScheduleSpecOpts{}, fmt.Errorf("--tz is %d bytes, at most %d", len(a.Tz), protocol.ScheduleZoneMax)
	}
	switch {
	case a.Runner == "any":
		// R19: the edit-only spelling for "back to any runner".
		o.Selector = &protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	case a.Runner != "" || a.Host != "" || a.IP != "":
		sel, err := BuildSelector(SelectorOpts{Runner: a.Runner, Host: a.Host, IP: a.IP})
		if err != nil {
			return ScheduleSpecOpts{}, err
		}
		o.Selector = &sel
	}
	if a.NoAgentArgs {
		o.ExtraArgs = []string{} // present and empty: clears the stored list
	}
	if a.ResumeConversationPresent {
		v := a.ResumeConversation
		o.ResumeConversation = &v
	}
	if a.OneshotPresent || a.StreamPresent {
		k := protocol.TaskKind_Interactive
		switch {
		case a.StreamPresent && a.Stream:
			k = protocol.TaskKind_Stream
		case a.OneshotPresent && a.Oneshot:
			k = protocol.TaskKind_Oneshot
		}
		o.Kind = &k
	}
	switch {
	case a.Record:
		v := true
		o.Record = &v
	case a.NoRecord:
		v := false
		o.Record = &v
	}
	switch {
	case a.Pty:
		v := true
		o.Pty = &v
	case a.NoPty:
		v := false
		o.Pty = &v
	}
	if a.ScopePresent {
		o.ScopePresent = true
		if a.Scope != nil {
			o.Scope = *a.Scope
		}
		o.Overrides = a.Overrides
	}
	o.GrantConditions = a.GrantConditions
	for _, nd := range []struct {
		flag    string
		present bool
		d       time.Duration
	}{{"--deadline", a.DeadlinePresent, a.Deadline}, {"--quiet", a.QuietPresent, a.QuietFor},
		{"--blocked", a.BlockedPresent, a.BlockedFor}, {"--queue-timeout", a.QueueTimeoutPresent, a.QueueTimeout}} {
		if nd.present && nd.d < 0 {
			return ScheduleSpecOpts{}, fmt.Errorf("%s %s: a duration cannot be negative (0 means none)", nd.flag, nd.d)
		}
	}
	if a.DeadlinePresent {
		d := a.Deadline
		o.Deadline = &d
	}
	if a.QuietPresent {
		d := a.QuietFor
		o.Quiet = &d
	}
	if a.BlockedPresent {
		d := a.BlockedFor
		o.Blocked = &d
	}
	if a.QueueTimeoutPresent {
		d := a.QueueTimeout
		o.QueueTimeout = &d
	}
	if a.TurnEndPresent {
		var te protocol.TurnEndSignal // "default" and "" are the unset value: any
		if a.TurnEnd != "" && a.TurnEnd != verb.TurnEndDefault {
			var err error
			if te, err = verb.ParseTurnEnd(a.TurnEnd); err != nil {
				return ScheduleSpecOpts{}, err
			}
		}
		o.TurnEnd = &te
	}
	if !isNew && a.RowsPresent != a.ColsPresent {
		return ScheduleSpecOpts{}, fmt.Errorf("schedule edit: --rows and --cols go together (the size is one value on the wire; one alone would overwrite the other with its default)")
	}
	if isNew || (a.RowsPresent && a.ColsPresent) {
		if a.Rows > 0xffff || a.Cols > 0xffff {
			return ScheduleSpecOpts{}, fmt.Errorf("--rows / --cols: at most 65535")
		}
		o.SizeSet, o.Rows, o.Cols = true, uint16(a.Rows), uint16(a.Cols)
	}
	return o, nil
}

func nonEmpty(l []string) []string {
	if len(l) == 0 {
		return nil
	}
	return l
}

// buildScheduleSpec is the one encoder from ScheduleSpecOpts to the wire spec:
// each presence bit is set exactly when its field was named. promptStreamID is
// the payload stream the prompt rides, used only when a prompt was named.
func buildScheduleSpec(o ScheduleSpecOpts, promptStreamID uint64) protocol.ScheduleSpec {
	var s protocol.ScheduleSpec
	if o.Cron != "" {
		s.SetCronPresent(true)
		s.SetCron([]byte(o.Cron))
	}
	if o.Tz != "" {
		s.SetTzPresent(true)
		s.SetTz([]byte(o.Tz))
	}
	if o.Repo != "" {
		s.SetRepoPresent(true)
		s.SetRepoPath([]byte(o.Repo))
	}
	if o.TaskID != "" {
		if tid, err := parseTaskIDHex(o.TaskID); err == nil { // checked by the builder
			s.SetTaskPresent(true)
			s.SetTaskId(tid)
		}
	}
	if o.Agent != "" {
		s.SetAgentPresent(true)
		s.SetAgentProfile([]byte(o.Agent))
	}
	if o.Selector != nil {
		s.SetSelectorPresent(true)
		s.SetSelector(*o.Selector)
	}
	if o.ExtraArgs != nil {
		s.SetExtraArgsPresent(true)
		s.SetExtraArgs(protocol.AgentArgsFromStrings(o.ExtraArgs))
	}
	if o.ResumeConversation != nil {
		s.SetResumeConversationSet(true)
		s.SetResumeConversation(*o.ResumeConversation)
	}
	if o.Kind != nil {
		s.SetKindPresent(true)
		s.SetKind(*o.Kind)
	} else if o.Oneshot != nil {
		k := protocol.TaskKind_Interactive
		if *o.Oneshot {
			k = protocol.TaskKind_Oneshot
		}
		s.SetKindPresent(true)
		s.SetKind(k)
	}
	if o.Record != nil {
		s.SetRecordSet(true)
		s.SetRecord(*o.Record)
	}
	if o.Pty != nil {
		s.SetPtySet(true)
		s.SetPty(*o.Pty)
	}
	if o.Caps != nil {
		s.SetRequestedCapsPresent(true)
		s.SetRequestedCaps(*o.Caps)
	}
	if o.ScopePresent {
		s.SetScopePresent(true)
		s.SetScope(o.Scope)
		s.SetOverrides(o.Overrides)
	}
	if o.DelegateCaps != nil {
		s.SetDelegateCapsPresent(true)
		s.SetDelegateCaps(*o.DelegateCaps)
	}
	if o.DelegateScope != nil {
		s.SetDelegateScopePresent(true)
		s.SetDelegateScope(*o.DelegateScope)
	}
	if o.SpawnDepth != nil {
		s.SetSpawnDepthPresent(true)
		s.SetSpawnDepth(*o.SpawnDepth)
	}
	if o.MaxDescendants != nil {
		s.SetMaxDescendantsPresent(true)
		s.SetMaxDescendants(*o.MaxDescendants)
	}
	ms := func(d time.Duration) uint64 {
		if d < 0 {
			return 0
		}
		return uint64(d / time.Millisecond)
	}
	if o.Deadline != nil {
		s.SetDeadlinePresent(true)
		s.SetDeadlineMs(ms(*o.Deadline))
	}
	if o.Quiet != nil {
		s.SetQuietPresent(true)
		s.SetQuietMs(ms(*o.Quiet))
	}
	if o.Blocked != nil {
		s.SetBlockedPresent(true)
		s.SetBlockedMs(ms(*o.Blocked))
	}
	if o.QueueTimeout != nil {
		s.SetQueueTimeoutPresent(true)
		s.SetQueueTimeoutMs(ms(*o.QueueTimeout))
	}
	if o.TurnEnd != nil {
		s.SetTurnEndPresent(true)
		s.SetTurnEnd(*o.TurnEnd)
	}
	if o.Prompt != "" {
		s.SetPromptPresent(true)
		s.SetPromptStreamId(promptStreamID)
	}
	if o.SizeSet {
		s.SetSizePresent(true)
		s.SetInitialRows(o.Rows)
		s.SetInitialCols(o.Cols)
	}
	s.GrantConditionsStreamId = o.grantConditionsStream
	return s
}

func scheduleCreateRequest(o ScheduleSpecOpts, promptStreamID uint64) *protocol.TaskControlRequest {
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleCreate}}
	req.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: buildScheduleSpec(o, promptStreamID)})
	return req
}

func scheduleEditRequest(id protocol.ScheduleID, o ScheduleSpecOpts, promptStreamID uint64) *protocol.TaskControlRequest {
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleEdit}}
	req.SetScheduleEdit(protocol.ScheduleEditRequest{Id: id, Spec: buildScheduleSpec(o, promptStreamID)})
	return req
}

// --- statuses ---

// scheduleStatusGloss is what each status means to the person who typed the
// verb, used when the server sent no message of its own. Every ScheduleStatus
// has one (TestEveryScheduleStatusHasAMessage walks the enum).
var scheduleStatusGloss = map[protocol.ScheduleStatus]string{
	protocol.ScheduleStatus_NoSuchSchedule:        "no such schedule, or it is outside your reach",
	protocol.ScheduleStatus_InvalidCron:           "the cron expression is not valid (five fields, or @hourly/@daily/@weekly/@monthly/@yearly; no @every)",
	protocol.ScheduleStatus_InvalidZone:           "unknown time zone (an IANA name such as Asia/Tokyo)",
	protocol.ScheduleStatus_NoCandidate:           "no connected runner serves that repo and runner selector right now",
	protocol.ScheduleStatus_DeadlineRequired:      "a schedule's runs are background runs: state --deadline",
	protocol.ScheduleStatus_PromptRequired:        "a schedule needs a prompt: state --prompt",
	protocol.ScheduleStatus_ProfileRequired:       "a schedule that creates its task needs an agent: state --agent",
	protocol.ScheduleStatus_ProfileUnavailable:    "no candidate runner offers that agent",
	protocol.ScheduleStatus_TaskNotReachable:      "the task is outside your reach, or does not exist",
	protocol.ScheduleStatus_TaskRunningForeground: "the task is running in foreground: end it, or switch it with `session mode <id> background`, first",
	protocol.ScheduleStatus_SelfRefused:           "a schedule's own task cannot edit, run, delete or rebind its schedule",
	protocol.ScheduleStatus_SpawnLimit:            "your spawn limits do not allow another task",
	protocol.ScheduleStatus_PromptTooLarge:        fmt.Sprintf("the prompt is over %d bytes", protocol.PromptMax),
	protocol.ScheduleStatus_InternalError:         "the server failed; see its log",
	protocol.ScheduleStatus_RepoRequired:          "name the repo to create the task in (--repo), or bind a task (--task)",
	protocol.ScheduleStatus_RepoConflict:          "the repo is the bound task's: --repo with --task is refused, and a bound schedule's repo is not edited",
	protocol.ScheduleStatus_ScopeNotPermitted:     "the scope or delegation asked for is outside your reach or your delegation bound",
	protocol.ScheduleStatus_RecordNotPermitted:    "recording a session needs record_persist",
	protocol.ScheduleStatus_TurnEndUnsupported:    "that --turn-end signal is not supported for this run",
	protocol.ScheduleStatus_TaskAlreadyBound:      "that task is already bound to another schedule",
	protocol.ScheduleStatus_NotReady:              "the server is still starting its schedules; try again in a moment",
	protocol.ScheduleStatus_KindUnsupported:       "a schedule runs its task as interactive or stream",
	protocol.ScheduleStatus_OneshotStream:         "--oneshot runs in a PTY; an event-stream (--stream) schedule has none",
	protocol.ScheduleStatus_PtyStream:             "--pty: an event-stream (--stream) schedule has no PTY",
	protocol.ScheduleStatus_BadGrantConditions:    "the server refused the --grant-condition body",
}

// scheduleDetailPty is the detail's PTY word. A stream run has no PTY, and the
// server stores its bit as false (resolveNoPTY) -- read literally, "pty on" --
// so the kind decides first.
func scheduleDetailPty(d protocol.ScheduleDetail, dflt string) string {
	if d.Kind == protocol.TaskKind_Stream {
		return "none"
	}
	return schedTriState(d.PtySet(), d.Pty(), dflt)
}

// ScheduleKindWord is a schedule's task kind as `schedule show` prints it:
// the kind's wire name (interactive / stream).
func ScheduleKindWord(k protocol.TaskKind) string { return EnumWireName(k.String()) }

// scheduleStatusError is the error for a schedule answer: nil for ok,
// otherwise `schedule <verb>: <status>: <the server's message, or the gloss>`.
// Every status is an error and so a non-zero exit code.
func scheduleStatusError(verbName string, st protocol.ScheduleStatus, msg []byte) error {
	if st == protocol.ScheduleStatus_Ok {
		return nil
	}
	what := EnumWireName(st.String())
	switch {
	case len(msg) > 0:
		what += ": " + string(msg)
	case scheduleStatusGloss[st] != "":
		what += ": " + scheduleStatusGloss[st]
	}
	return fmt.Errorf("schedule %s: %s", verbName, what)
}

// --- client methods ---

func scheduleResponseOf(resp *protocol.TaskControlResponse) *protocol.ScheduleResponse {
	switch resp.Kind {
	case protocol.TaskControlKind_ScheduleCreate:
		return resp.ScheduleCreate()
	case protocol.TaskControlKind_ScheduleEdit:
		return resp.ScheduleEdit()
	case protocol.TaskControlKind_ScheduleDelete:
		return resp.ScheduleDelete()
	case protocol.TaskControlKind_ScheduleRun:
		return resp.ScheduleRun()
	case protocol.TaskControlKind_ScheduleList:
		return resp.ScheduleList()
	case protocol.TaskControlKind_ScheduleShow:
		return resp.ScheduleShow()
	}
	return nil
}

// scheduleRoundTrip sends one schedule request and returns the server's
// ScheduleResponse for it (refusals included: the caller reads the status).
func (c *Client) scheduleRoundTrip(ctx context.Context, verbName string, req *protocol.TaskControlRequest) (*protocol.ScheduleResponse, error) {
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	return scheduleAnswer(verbName, req, resp)
}

func scheduleAnswer(verbName string, req *protocol.TaskControlRequest, resp *protocol.TaskControlResponse) (*protocol.ScheduleResponse, error) {
	if resp.Kind != req.Header.Kind {
		return nil, fmt.Errorf("schedule %s: unexpected response kind %v", verbName, resp.Kind)
	}
	r := scheduleResponseOf(resp)
	if r == nil {
		return nil, fmt.Errorf("schedule %s: empty response", verbName)
	}
	return r, nil
}

// scheduleWithPrompt sends a create or an edit. A named prompt rides its own
// payload stream written after the request, as an open's does (A-D17), so a
// request of any prompt size still fits one datagram; grant conditions ride
// another (grant conditions C6).
func (c *Client) scheduleWithPrompt(ctx context.Context, verbName string, o ScheduleSpecOpts, build func(o ScheduleSpecOpts, promptStreamID uint64) *protocol.TaskControlRequest) (*protocol.ScheduleResponse, error) {
	payloads, buildIDs, err := scheduleBodies(o, build)
	if err != nil {
		return nil, err
	}
	if len(payloads) == 0 {
		return c.scheduleRoundTrip(ctx, verbName, buildIDs(nil))
	}
	var sent *protocol.TaskControlRequest
	res, err := c.TaskControlWithPayloads(ctx, func(ids []uint64) (*protocol.TaskControlRequest, error) {
		sent = buildIDs(ids)
		return sent, nil
	}, payloads)
	if err != nil {
		return nil, err
	}
	if res.Err != nil {
		return nil, res.Err
	}
	return scheduleAnswer(verbName, sent, res.Resp)
}

// scheduleBodies is a create's or an edit's body streams — the prompt (if
// any), then the grant conditions (if any) — and the request naming them. The
// overrides the conditions belong to are marked here, so a condition that
// matches no --scope-for is refused before anything is sent.
func scheduleBodies(o ScheduleSpecOpts, build func(o ScheduleSpecOpts, promptStreamID uint64) *protocol.TaskControlRequest) ([][]byte, func([]uint64) *protocol.TaskControlRequest, error) {
	var payloads [][]byte
	if o.Prompt != "" {
		if err := checkPrompt(o.Prompt); err != nil {
			return nil, nil, err
		}
		body := protocol.PromptBody{}
		body.SetPrompt([]byte(o.Prompt))
		enc, err := body.EncodeCopy(nil)
		if err != nil {
			return nil, nil, fmt.Errorf("encode prompt: %w", err)
		}
		payloads = append(payloads, enc)
	}
	if len(o.GrantConditions) != 0 && !o.ScopePresent {
		return nil, nil, fmt.Errorf("--grant-condition attaches to a --scope-for override")
	}
	condBody, overrides, err := grantConditionsBody(o.Overrides, o.GrantConditions)
	if err != nil {
		return nil, nil, err
	}
	o.Overrides = overrides
	if condBody != nil {
		payloads = append(payloads, condBody)
	}
	return payloads, func(ids []uint64) *protocol.TaskControlRequest {
		next := 0
		var promptID uint64
		if o.Prompt != "" {
			promptID = ids[next]
			next++
		}
		if condBody != nil {
			o.grantConditionsStream = ids[next]
		}
		return build(o, promptID)
	}, nil
}

// ScheduleCreate creates a schedule and returns its id (32 hex).
func (c *Client) ScheduleCreate(ctx context.Context, spec ScheduleSpecOpts) (string, error) {
	r, err := c.scheduleWithPrompt(ctx, "new", spec, scheduleCreateRequest)
	if err != nil {
		return "", err
	}
	if err := scheduleStatusError("new", r.Status, r.Message); err != nil {
		return "", err
	}
	return hex.EncodeToString(r.Id.Id[:]), nil
}

// ScheduleEdit changes the future firings of a schedule: only what spec names
// (every field, the conditions included, has its own presence bit, so the
// server overlays just those under the schedule's firing mutex).
func (c *Client) ScheduleEdit(ctx context.Context, id string, spec ScheduleSpecOpts) error {
	sid, err := parseScheduleID(id)
	if err != nil {
		return err
	}
	r, err := c.scheduleWithPrompt(ctx, "edit", spec, func(o ScheduleSpecOpts, s uint64) *protocol.TaskControlRequest { return scheduleEditRequest(sid, o, s) })
	if err != nil {
		return err
	}
	return scheduleStatusError("edit", r.Status, r.Message)
}

func parseScheduleID(s string) (protocol.ScheduleID, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return protocol.ScheduleID{}, fmt.Errorf("schedule id: %w", err)
	}
	if len(raw) != 16 {
		return protocol.ScheduleID{}, fmt.Errorf("schedule id: expected 32 hex chars (16 bytes), got %d bytes", len(raw))
	}
	var id protocol.ScheduleID
	copy(id.Id[:], raw)
	return id, nil
}

func (c *Client) scheduleByID(ctx context.Context, verbName string, kind protocol.TaskControlKind, id string) (*protocol.ScheduleResponse, error) {
	sid, err := parseScheduleID(id)
	if err != nil {
		return nil, err
	}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: kind}}
	body := protocol.ScheduleIDRequest{Id: sid}
	switch kind {
	case protocol.TaskControlKind_ScheduleDelete:
		req.SetScheduleDelete(body)
	case protocol.TaskControlKind_ScheduleRun:
		req.SetScheduleRun(body)
	case protocol.TaskControlKind_ScheduleShow:
		req.SetScheduleShow(body)
	}
	r, err := c.scheduleRoundTrip(ctx, verbName, req)
	if err != nil {
		return nil, err
	}
	if err := scheduleStatusError(verbName, r.Status, r.Message); err != nil {
		return nil, err
	}
	return r, nil
}

// ScheduleDelete deletes a schedule; its task stays (S-D19).
func (c *Client) ScheduleDelete(ctx context.Context, id string) error {
	_, err := c.scheduleByID(ctx, "rm", protocol.TaskControlKind_ScheduleDelete, id)
	return err
}

// ScheduleRun fires a schedule now (S-D13).
func (c *Client) ScheduleRun(ctx context.Context, id string) error {
	_, err := c.scheduleByID(ctx, "run", protocol.TaskControlKind_ScheduleRun, id)
	return err
}

// ScheduleList returns every schedule the caller can reach.
func (c *Client) ScheduleList(ctx context.Context) ([]protocol.ScheduleRow, error) {
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleList}}
	req.SetScheduleList(protocol.ScheduleListRequest{})
	r, err := c.scheduleRoundTrip(ctx, "ls", req)
	if err != nil {
		return nil, err
	}
	if err := scheduleStatusError("ls", r.Status, r.Message); err != nil {
		return nil, err
	}
	var body protocol.ScheduleListBody
	if err := ReadResponseBody(ctx, c.Transport(), r.StreamId, &body); err != nil {
		return nil, fmt.Errorf("schedule ls: %w", err)
	}
	return body.Rows, nil
}

// ScheduleShow returns one schedule's detail.
func (c *Client) ScheduleShow(ctx context.Context, id string) (protocol.ScheduleDetail, error) {
	r, err := c.scheduleByID(ctx, "show", protocol.TaskControlKind_ScheduleShow, id)
	if err != nil {
		return protocol.ScheduleDetail{}, err
	}
	var d protocol.ScheduleDetail
	if err := ReadResponseBody(ctx, c.Transport(), r.StreamId, &d); err != nil {
		return protocol.ScheduleDetail{}, fmt.Errorf("schedule show: %w", err)
	}
	return d, nil
}

// --- the verbs, for a surface holding a *Client ---

// ScheduleSpecWith runs schedule new / edit from a parsed line and prints what
// changed: the target and the change, never a bare "ok" (checklist 29).
func ScheduleSpecWith(ctx context.Context, c *Client, a verb.ScheduleSpecAction, out io.Writer) error {
	o, err := ScheduleSpecOptsFromAction(a)
	if err != nil {
		return fmt.Errorf("schedule %s: %w", a.Sub, err)
	}
	if a.Sub == verb.SubEdit {
		if err := c.ScheduleEdit(ctx, a.ID, o); err != nil {
			return err
		}
		fmt.Fprintf(out, "schedule edit %s: changed %s\n", a.ID, scheduleChangeList(o))
		return nil
	}
	id, err := c.ScheduleCreate(ctx, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "schedule new %s: cron=%q tz=%s\n", id, o.Cron, orServerDefault(o.Tz))
	return nil
}

func orServerDefault(tz string) string {
	if tz == "" {
		return "(server's zone)"
	}
	return tz
}

// scheduleChangeList names what an edit sent, in the order the flags are
// documented; a bare edit sent only the authority refresh.
func scheduleChangeList(o ScheduleSpecOpts) string {
	var ch []string
	add := func(on bool, name string) {
		if on {
			ch = append(ch, name)
		}
	}
	add(o.Cron != "", "cron")
	add(o.Tz != "", "tz")
	add(o.Repo != "", "repo")
	add(o.TaskID != "", "task")
	add(o.Agent != "", "agent")
	add(o.Selector != nil, "runner")
	add(o.ExtraArgs != nil, "agent-arg")
	add(o.ResumeConversation != nil, "resume-conversation")
	add(o.Oneshot != nil || (o.Kind != nil && *o.Kind == protocol.TaskKind_Oneshot), "oneshot")
	add(o.Kind != nil && *o.Kind != protocol.TaskKind_Oneshot, "stream")
	add(o.Record != nil, "record")
	add(o.Pty != nil, "pty")
	add(o.Prompt != "", "prompt")
	add(o.Deadline != nil, "deadline")
	add(o.Quiet != nil, "quiet")
	add(o.Blocked != nil, "blocked")
	add(o.QueueTimeout != nil, "queue-timeout")
	add(o.TurnEnd != nil, "turn-end")
	add(o.SizeSet, "size")
	add(o.Caps != nil, "caps")
	add(o.ScopePresent, "scope")
	add(o.DelegateCaps != nil, "delegate-caps")
	add(o.DelegateScope != nil, "delegate-scope")
	add(o.SpawnDepth != nil, "spawn-depth")
	add(o.MaxDescendants != nil, "max-descendants")
	if len(ch) == 0 {
		return "nothing named (the stored authority is recomputed with you as granter)"
	}
	return strings.Join(ch, ", ") + " (the stored authority is recomputed with you as granter)"
}

// ScheduleIDWith runs schedule run / rm.
func ScheduleIDWith(ctx context.Context, c *Client, a verb.ScheduleIDAction, out io.Writer) error {
	switch a.Sub {
	case verb.SubRun:
		if err := c.ScheduleRun(ctx, a.ID); err != nil {
			return err
		}
		fmt.Fprintf(out, "schedule run %s: fired now (run_now); its next firing is unchanged\n", a.ID)
	case verb.SubRm:
		if err := c.ScheduleDelete(ctx, a.ID); err != nil {
			return err
		}
		fmt.Fprintf(out, "schedule rm %s: deleted; its task stays and a run in progress is not cancelled\n", a.ID)
	default:
		return fmt.Errorf("schedule: unknown sub-verb %q", a.Sub)
	}
	return nil
}

// ScheduleLsWith runs schedule ls.
func ScheduleLsWith(ctx context.Context, c *Client, asJSON bool, out io.Writer) error {
	rows, err := c.ScheduleList(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	if asJSON {
		return ScheduleListJSON(out, rows, now)
	}
	ScheduleListText(out, rows, now)
	return nil
}

// ScheduleListText prints the rows `schedule ls` prints; ScheduleListJSON is
// its --json document. A surface that fetched the rows itself (the WebUI,
// which also draws them) renders through these, not a second copy.
func ScheduleListText(out io.Writer, rows []protocol.ScheduleRow, now time.Time) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "no schedules")
	}
	for _, r := range rows {
		fmt.Fprintln(out, ScheduleRowLine(r, now))
	}
}

func ScheduleListJSON(out io.Writer, rows []protocol.ScheduleRow, now time.Time) error {
	return writeScheduleListJSON(out, rows, now)
}

// ScheduleDetailJSON is `schedule show --json`.
func ScheduleDetailJSON(out io.Writer, d protocol.ScheduleDetail, now time.Time) error {
	return writeScheduleDetailJSON(out, d, now)
}

// ScheduleShowWith runs schedule show.
func ScheduleShowWith(ctx context.Context, c *Client, id string, asJSON bool, out io.Writer) error {
	d, err := c.ScheduleShow(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now()
	if asJSON {
		return writeScheduleDetailJSON(out, d, now)
	}
	_, err = io.WriteString(out, ScheduleDetailText(d, now))
	return err
}

func scheduleDial(ctx context.Context, peerCID objproto.ConnectionID, fn func(*Client) error) error {
	return recordDial(ctx, peerCID, fn)
}

// ScheduleSpecOut, ScheduleIDOut, ScheduleLsOut and ScheduleShowOut are the
// fresh-dial forms for the CLI.
func ScheduleSpecOut(ctx context.Context, peerCID objproto.ConnectionID, a verb.ScheduleSpecAction, out io.Writer) error {
	return scheduleDial(ctx, peerCID, func(c *Client) error { return ScheduleSpecWith(ctx, c, a, out) })
}

func ScheduleIDOut(ctx context.Context, peerCID objproto.ConnectionID, a verb.ScheduleIDAction, out io.Writer) error {
	return scheduleDial(ctx, peerCID, func(c *Client) error { return ScheduleIDWith(ctx, c, a, out) })
}

func ScheduleLsOut(ctx context.Context, peerCID objproto.ConnectionID, asJSON bool, out io.Writer) error {
	return scheduleDial(ctx, peerCID, func(c *Client) error { return ScheduleLsWith(ctx, c, asJSON, out) })
}

func ScheduleShowOut(ctx context.Context, peerCID objproto.ConnectionID, id string, asJSON bool, out io.Writer) error {
	return scheduleDial(ctx, peerCID, func(c *Client) error { return ScheduleShowWith(ctx, c, id, asJSON, out) })
}

// --- rendering ---

// WaitStats is S-D11: over the fired and run_now entries that have BOTH
// queued_at and started_at, the count, the median and the maximum of
// started_at - queued_at. A resumed_by entry never counts (S-D10: its wait is
// not the schedule's), nor does anything else. The median of an even count is
// the mean of the middle two.
func WaitStats(h []protocol.ScheduleHistoryEntry) (n int, median, max time.Duration) {
	var waits []time.Duration
	for _, e := range h {
		if e.Outcome != protocol.ScheduleOutcome_Fired && e.Outcome != protocol.ScheduleOutcome_RunNow {
			continue
		}
		if e.QueuedAt == 0 || e.StartedAt == 0 {
			continue
		}
		waits = append(waits, time.Duration(int64(e.StartedAt)-int64(e.QueuedAt)))
	}
	n = len(waits)
	if n == 0 {
		return 0, 0, 0
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	max = waits[n-1]
	if n%2 == 1 {
		return n, waits[n/2], max
	}
	return n, (waits[n/2-1] + waits[n/2]) / 2, max
}

// scheduleLocation is the zone a schedule's times are shown in. A name this
// host cannot load (a client without a zone database) falls back to UTC, which
// the rendered abbreviation says; the zone's name is always printed beside it.
func scheduleLocation(tz string) *time.Location {
	if loc, err := time.LoadLocation(tz); err == nil && tz != "" {
		return loc
	}
	return time.UTC
}

const (
	schedMinute = "2006-01-02 15:04 MST"
	schedSecond = "2006-01-02 15:04:05 MST"
)

func schedTime(ns uint64, loc *time.Location, layout string) string {
	if ns == 0 {
		return "-"
	}
	return time.Unix(0, int64(ns)).In(loc).Format(layout)
}

// schedBoth is a time in the schedule's zone with the absolute instant beside
// it, `-` when absent.
func schedBoth(ns uint64, loc *time.Location, layout string) string {
	if ns == 0 {
		return "-"
	}
	return schedTime(ns, loc, layout) + " (" + schedInstant(ns) + ")"
}

func schedInstant(ns uint64) string {
	if ns == 0 {
		return "-"
	}
	return time.Unix(0, int64(ns)).UTC().Format(time.RFC3339)
}

// schedNextLocal is the next firing in the schedule's zone, "none" when there
// is none: the one spelling the text row and the JSON share.
func schedNextLocal(ns uint64, loc *time.Location) string {
	if ns == 0 {
		return "none"
	}
	return schedTime(ns, loc, schedMinute)
}

func schedOutcomeWord(o protocol.ScheduleOutcome) string { return EnumWireName(o.String()) }

func schedTaskStateWord(s protocol.ScheduleTaskState) string { return EnumWireName(s.String()) }

func schedAge(ns uint64, now time.Time) string {
	d := now.Sub(time.Unix(0, int64(ns)))
	if d < 0 {
		d = 0
	}
	return AgeStr(uint64(d / time.Millisecond))
}

// ScheduleTaskCell is the task a schedule is bound to, as the row words it:
// <id8>:<status>, created-at-first-firing, or no-task. The TUI table cell and
// the `task=` field of ScheduleRowLine are this one function.
func ScheduleTaskCell(r protocol.ScheduleRow) string {
	switch r.TaskState {
	case protocol.ScheduleTaskState_Bound:
		return hex.EncodeToString(r.TaskId.Id[:4]) + ":" + taskStatusJSON(r.TaskStatus)
	case protocol.ScheduleTaskState_PendingCreate:
		return "created-at-first-firing"
	}
	return "no-task"
}

func schedTaskField(r protocol.ScheduleRow) string { return "task=" + ScheduleTaskCell(r) }

// ScheduleAboutCell is the prompt head as the row shows it: quoted, so a
// control character in it cannot reach the terminal or split a row, and
// (hidden) for a prompt the caller may not read.
func ScheduleAboutCell(r protocol.ScheduleRow) string {
	if r.PromptHidden() {
		return "(hidden)"
	}
	return fmt.Sprintf("%q", string(r.PromptHead))
}

func schedPromptField(r protocol.ScheduleRow) string { return "prompt=" + ScheduleAboutCell(r) }

// ScheduleNextCell is the next firing in the schedule's zone ("none" when
// there is none), ScheduleLastCell the last history entry's outcome and age
// ("none" when it never fired): the spellings of ScheduleRowLine's next= and
// last=.
func ScheduleNextCell(r protocol.ScheduleRow) string {
	return schedNextLocal(r.NextAt, scheduleLocation(string(r.Tz)))
}

func ScheduleLastCell(r protocol.ScheduleRow, now time.Time) string {
	if r.LastPresent() {
		if l := r.Last(); l != nil {
			return schedOutcomeWord(l.Outcome) + " " + schedAge(l.At, now)
		}
	}
	return "none"
}

// ScheduleRowLine is one `schedule ls` row:
//
//	<id>  <cron>  <tz>  next=<time in the zone>  last=<outcome> <age>  task=…  <agent>  <repo>  prompt=…
//
// No field is elided at zero (checklist 31): a schedule that never fired reads
// last=none, one with no next firing next=none. The id is printed in full —
// it is the argument of show, edit, run and rm, and an id8 could not be pasted
// into them (the rule taskIDStr states). The prompt is quoted so a newline in
// it cannot split the row, and a prompt the caller may not read says so.
func ScheduleRowLine(r protocol.ScheduleRow, now time.Time) string {
	return fmt.Sprintf("%s  %s  %s  next=%s  last=%s  %s  %s  %s  %s",
		hex.EncodeToString(r.Id.Id[:]), string(r.Cron), string(r.Tz), ScheduleNextCell(r), ScheduleLastCell(r, now),
		schedTaskField(r), string(r.Agent), string(r.Repo), schedPromptField(r))
}

func schedSelectorLabel(sel protocol.RunnerSelector) string {
	word := EnumWireName(sel.Kind.String())
	switch sel.Kind {
	case protocol.RunnerSelectorKind_ByRunnerId:
		if v := sel.RunnerId(); v != nil {
			return word + " " + v.Hex()
		}
	case protocol.RunnerSelectorKind_ByHostname:
		if v := sel.Hostname(); v != nil {
			return word + " " + string(v.Name)
		}
	case protocol.RunnerSelectorKind_ByIp:
		if v := sel.IpAddr(); v != nil {
			return word + " " + net.IP(v.Addr).String()
		}
	case protocol.RunnerSelectorKind_ByConnId:
		if v := sel.ConnId(); v != nil {
			return word + " " + RunnerCIDStr(*v)
		}
	}
	return word
}

func schedMs(ms uint64) string { return (time.Duration(ms) * time.Millisecond).String() }

func schedConditions(c protocol.BackgroundConditions) string {
	qt := "none"
	if c.QueueTimeoutMs != 0 {
		qt = schedMs(c.QueueTimeoutMs)
	}
	return fmt.Sprintf("deadline=%s turn-end=%s quiet=%s blocked=%s queue-timeout=%s",
		schedMs(c.DeadlineMs), EnumWireName(c.TurnEnd.String()), schedMs(c.QuietMs), schedMs(c.BlockedMs), qt)
}

func schedTriState(set, v bool, unset string) string {
	switch {
	case !set:
		return unset
	case v:
		return "on"
	}
	return "off"
}

func schedByLabel(kind protocol.ClientKind, id protocol.TaskID) string {
	s := originStr(kind)
	if id.Id != ([16]byte{}) {
		s += " " + hex.EncodeToString(id.Id[:])
	}
	return s
}

// ScheduleDetailText is `schedule show`: the definition, the stored authority,
// the history (oldest first) and the wait statistics. Every time is shown in
// the schedule's zone with the absolute instant beside the ones that matter
// (S-D3: every surface shows a firing with its zone); an absent time is `-`,
// never dropped; a history field that exists is always printed.
func ScheduleDetailText(d protocol.ScheduleDetail, now time.Time) string {
	p := ScheduleDetailParts(d, now)
	var b strings.Builder
	for _, l := range p.Head {
		b.WriteString(l + "\n")
	}
	b.WriteString(p.Waits + "\n")
	b.WriteString(p.HistoryTitle + "\n")
	for _, l := range p.History {
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}

// ScheduleDetailPartsOf is ScheduleDetailText cut into the pieces a surface
// with its own layout (the WebUI dialog) places itself: Head is every line
// before the wait statistics, Waits the `waits:` line, HistoryTitle the line
// introducing the history and History each entry without its indent. The text
// form is these parts joined, so a surface cannot render a second wording.
type ScheduleDetailPartsOf struct {
	Head         []string
	Waits        string
	HistoryTitle string
	History      []string
}

// ScheduleDetailParts renders d as its parts; ScheduleDetailText joins them.
func ScheduleDetailParts(d protocol.ScheduleDetail, now time.Time) ScheduleDetailPartsOf {
	r := d.Row
	loc := scheduleLocation(string(r.Tz))
	var head []string
	line := func(format string, a ...any) { head = append(head, fmt.Sprintf(format, a...)) }

	line("id: %s", hex.EncodeToString(r.Id.Id[:]))
	line("cron: %s", string(r.Cron))
	line("zone: %s", string(r.Tz))
	if r.NextAt != 0 {
		line("next: %s (%s)", schedTime(r.NextAt, loc, schedMinute), schedInstant(r.NextAt))
	} else {
		line("next: none")
	}
	switch r.TaskState {
	case protocol.ScheduleTaskState_Bound:
		line("task: %s %s", hex.EncodeToString(r.TaskId.Id[:]), taskStatusJSON(r.TaskStatus))
	case protocol.ScheduleTaskState_PendingCreate:
		line("task: created at first firing")
	default:
		line("task: none (unbound; firings are skipped_no_task until `schedule edit <id> --task` binds one)")
	}
	line("task-state: %s", schedTaskStateWord(r.TaskState))
	line("repo: %s", string(r.Repo))
	line("agent: %s", string(r.Agent))
	line("selector: %s", schedSelectorLabel(d.Selector))
	if r.PromptHidden() {
		line("prompt: (hidden: you could not read the bound task's prompt in `ls`)")
	} else {
		line("prompt: %q", string(d.Prompt))
	}
	line("extra-args: %s", quotedList(d.ExtraArgs.AsStrings()))
	line("resume-conversation: %v", d.ResumeConversation())
	line("kind: %s", ScheduleKindWord(d.Kind))
	line("record: %s", schedTriState(d.RecordSet(), d.Record(), "server default"))
	line("pty: %s", scheduleDetailPty(d, "the mode's default"))
	line("conditions: %s", schedConditions(d.Conditions))
	auth := fmt.Sprintf("caps=%s scope=%s", CapsLabel(d.Caps), ScopeLabel(d.Scope))
	if ov := OverridesLabel(d.Overrides); ov != "" {
		auth += " +" + ov
	}
	if cl := ConditionsLabel(d.Overrides, d.GrantConditions); cl != "" {
		auth += " conditions=" + cl
	}
	if w := ActionOverlapLabel(d.ActionOverlap); w != "" {
		auth += " " + w
	}
	line("authority: %s delegate=%s delegate-scope=%s spawn-depth=%s max-descendants=%s",
		auth, CapsLabel(d.DelegateCaps), ScopeLabel(d.DelegateScope),
		authority.LimitLabel(d.SpawnDepth), authority.LimitLabel(d.MaxDescendants))
	if d.Creator.Id == ([16]byte{}) {
		line("creator: operator")
	} else {
		line("creator: %s", hex.EncodeToString(d.Creator.Id[:]))
	}
	line("created: %s (%s)", schedTime(d.CreatedAt, loc, schedMinute), schedInstant(d.CreatedAt))
	if d.EditedAt == 0 {
		line("edited: never")
	} else {
		line("edited: %s (%s) by %s", schedTime(d.EditedAt, loc, schedMinute), schedInstant(d.EditedAt), schedByLabel(d.EditedByKind, d.EditedBy))
	}
	p := ScheduleDetailPartsOf{Head: head, HistoryTitle: "history: none yet"}
	n, median, max := WaitStats(d.History)
	if n == 0 {
		p.Waits = "waits: n=0 (no fired or run_now entry has both queued and started yet)"
	} else {
		p.Waits = fmt.Sprintf("waits: n=%d median=%s max=%s", n, median.Round(time.Millisecond), max.Round(time.Millisecond))
	}
	if len(d.History) > 0 {
		p.HistoryTitle = fmt.Sprintf("history (%d, oldest first):", len(d.History))
		for _, e := range d.History {
			p.History = append(p.History, scheduleHistoryLine(e, loc))
		}
	}
	return p
}

func quotedList(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	q := make([]string, len(l))
	for i, s := range l {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, " ")
}

func scheduleHistoryLine(e protocol.ScheduleHistoryEntry, loc *time.Location) string {
	end := "-"
	if e.EndedAt != 0 {
		end = "unknown"
		if e.EndReason != protocol.EndReason_None {
			end = EnumWireName(e.EndReason.String())
		}
	}
	s := fmt.Sprintf("%s  %s  due=%s  queued=%s  started=%s  ended=%s  end=%s",
		schedBoth(e.At, loc, schedSecond), schedOutcomeWord(e.Outcome),
		schedBoth(e.DueAt, loc, schedSecond), schedBoth(e.QueuedAt, loc, schedSecond),
		schedBoth(e.StartedAt, loc, schedSecond), schedBoth(e.EndedAt, loc, schedSecond), end)
	if e.Outcome == protocol.ScheduleOutcome_ResumedBy || e.Outcome == protocol.ScheduleOutcome_Unbound {
		by := originStr(e.ByKind)
		if e.By.Id != ([16]byte{}) {
			by += ":" + hex.EncodeToString(e.By.Id[:])
		}
		s += "  by=" + by
	}
	if len(e.Message) > 0 {
		s += fmt.Sprintf("  msg=%q", string(e.Message))
	}
	return s
}

// --- JSON ---

type scheduleHistoryJSON struct {
	Outcome   string `json:"outcome"`
	DueAt     uint64 `json:"due_at"`
	At        uint64 `json:"at"`
	QueuedAt  uint64 `json:"queued_at"`
	StartedAt uint64 `json:"started_at"`
	EndedAt   uint64 `json:"ended_at"`
	EndReason string `json:"end_reason"`
	ByKind    string `json:"by_kind"`
	By        string `json:"by"`
	Message   string `json:"message"`
}

func newScheduleHistoryJSON(e protocol.ScheduleHistoryEntry) scheduleHistoryJSON {
	return scheduleHistoryJSON{
		Outcome: schedOutcomeWord(e.Outcome), DueAt: e.DueAt, At: e.At,
		QueuedAt: e.QueuedAt, StartedAt: e.StartedAt, EndedAt: e.EndedAt,
		EndReason: EnumWireName(e.EndReason.String()), ByKind: EnumWireName(e.ByKind.String()),
		By: taskIDHexOrEmpty(e.By.Id[:]), Message: string(e.Message),
	}
}

type scheduleRowJSON struct {
	ID           string               `json:"id"`
	Cron         string               `json:"cron"`
	Tz           string               `json:"tz"`
	TaskState    string               `json:"task_state"`
	TaskID       string               `json:"task_id"`
	TaskStatus   string               `json:"task_status"`
	NextAt       uint64               `json:"next_at"`
	NextLocal    string               `json:"next_local"`
	Last         *scheduleHistoryJSON `json:"last"`
	Agent        string               `json:"agent"`
	Repo         string               `json:"repo"`
	PromptHead   string               `json:"prompt_head"`
	PromptHidden bool                 `json:"prompt_hidden"`
}

func newScheduleRowJSON(r protocol.ScheduleRow) scheduleRowJSON {
	j := scheduleRowJSON{
		ID: hex.EncodeToString(r.Id.Id[:]), Cron: string(r.Cron), Tz: string(r.Tz),
		TaskState: schedTaskStateWord(r.TaskState), NextAt: r.NextAt,
		NextLocal: schedNextLocal(r.NextAt, scheduleLocation(string(r.Tz))),
		Agent:     string(r.Agent), Repo: string(r.Repo),
		PromptHead: string(r.PromptHead), PromptHidden: r.PromptHidden(),
	}
	if r.TaskState == protocol.ScheduleTaskState_Bound {
		j.TaskID = hex.EncodeToString(r.TaskId.Id[:])
		j.TaskStatus = taskStatusJSON(r.TaskStatus)
	}
	if r.LastPresent() {
		if l := r.Last(); l != nil {
			h := newScheduleHistoryJSON(*l)
			j.Last = &h
		}
	}
	return j
}

func writeScheduleListJSON(w io.Writer, rows []protocol.ScheduleRow, now time.Time) error {
	doc := struct {
		Schedules []scheduleRowJSON `json:"schedules"`
	}{Schedules: []scheduleRowJSON{}}
	for _, r := range rows {
		doc.Schedules = append(doc.Schedules, newScheduleRowJSON(r))
	}
	return json.NewEncoder(w).Encode(doc)
}

type scheduleWaitStatsJSON struct {
	N        int   `json:"n"`
	MedianMs int64 `json:"median_ms"`
	MaxMs    int64 `json:"max_ms"`
}

type scheduleDetailJSON struct {
	scheduleRowJSON
	Prompt             string            `json:"prompt"`
	ExtraArgs          []string          `json:"extra_args"`
	Selector           string            `json:"selector"`
	ResumeConversation bool              `json:"resume_conversation"`
	Kind               string            `json:"kind"`
	Oneshot            bool              `json:"oneshot"`
	Record             string            `json:"record"`
	Pty                string            `json:"pty"`
	Conditions         conditionsJSON    `json:"conditions"`
	Caps               string            `json:"caps"`
	Scope              string            `json:"scope"`
	ScopeByCap         map[string]string `json:"scope_by_cap"`
	ConditionsByCap    map[string]string `json:"conditions_by_cap"`
	// ActionOverlap is the server's action_overlap as bit names, [] when none.
	ActionOverlap  []string              `json:"action_overlap"`
	DelegateCaps   string                `json:"delegate_caps"`
	DelegateScope  string                `json:"delegate_scope"`
	SpawnDepth     *uint32               `json:"spawn_depth"`
	MaxDescendants *uint32               `json:"max_descendants"`
	Creator        string                `json:"creator"`
	CreatedAt      uint64                `json:"created_at"`
	EditedAt       uint64                `json:"edited_at"`
	EditedByKind   string                `json:"edited_by_kind"`
	EditedBy       string                `json:"edited_by"`
	WaitStats      scheduleWaitStatsJSON `json:"wait_stats"`
	History        []scheduleHistoryJSON `json:"history"`
}

func writeScheduleDetailJSON(w io.Writer, d protocol.ScheduleDetail, now time.Time) error {
	n, median, max := WaitStats(d.History)
	j := scheduleDetailJSON{
		scheduleRowJSON: newScheduleRowJSON(d.Row),
		Prompt:          string(d.Prompt), ExtraArgs: append([]string{}, d.ExtraArgs.AsStrings()...),
		Selector:           schedSelectorLabel(d.Selector),
		ResumeConversation: d.ResumeConversation(), Kind: ScheduleKindWord(d.Kind), Oneshot: d.Kind == protocol.TaskKind_Oneshot,
		Record: schedTriState(d.RecordSet(), d.Record(), "default"), Pty: scheduleDetailPty(d, "default"),
		Conditions: conditionsJSON{
			DeadlineMs: d.Conditions.DeadlineMs, TurnEnd: EnumWireName(d.Conditions.TurnEnd.String()),
			QuietMs: d.Conditions.QuietMs, BlockedMs: d.Conditions.BlockedMs, QueueTimeoutMs: d.Conditions.QueueTimeoutMs,
		},
		Caps: CapsLabel(d.Caps), Scope: ScopeLabel(d.Scope),
		ScopeByCap:      ResolvedScopeByCap(d.Caps, d.Scope, d.Overrides),
		ConditionsByCap: ConditionsByCap(d.Caps, d.Overrides, d.GrantConditions),
		ActionOverlap:   ActionOverlapNames(d.ActionOverlap),
		DelegateCaps:    CapsLabel(d.DelegateCaps), DelegateScope: ScopeLabel(d.DelegateScope),
		SpawnDepth: LimitJSON(d.SpawnDepth), MaxDescendants: LimitJSON(d.MaxDescendants),
		Creator:   taskIDHexOrEmpty(d.Creator.Id[:]),
		CreatedAt: d.CreatedAt, EditedAt: d.EditedAt,
		EditedByKind: EnumWireName(d.EditedByKind.String()), EditedBy: taskIDHexOrEmpty(d.EditedBy.Id[:]),
		WaitStats: scheduleWaitStatsJSON{N: n, MedianMs: median.Milliseconds(), MaxMs: max.Milliseconds()},
		History:   []scheduleHistoryJSON{},
	}
	for _, e := range d.History {
		j.History = append(j.History, newScheduleHistoryJSON(e))
	}
	return json.NewEncoder(w).Encode(j)
}
