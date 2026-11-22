package server

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The six schedule verbs (pillar S, "Verbs", S-D12..S-D20): create, edit,
// delete, run, list, show. The firing machinery is schedule_fire.go; this file
// is what a caller may ask of it.
//
// Lock order for the verbs that change a schedule, outermost first:
//
//	per-schedule firing mutex  -> bindMu -> ScheduleStore.mu -> TaskStore (read)
//
// edit, delete and the S-D15 unbinding take the firing mutex of their schedule
// around the store change, so they serialize with a firing of that schedule
// (an edit out of pending_create cannot race an in-flight creation).

// FireVeto is a refusal a FireCheck gives: a ScheduleResponse, or the bit
// whose absence is answered as the permission denial.
type FireVeto struct {
	Refusal *protocol.ScheduleResponse
	Deny    protocol.Capability
}

// FireCheck is made by a verb that fires a schedule (`schedule run`). The
// firing calls it inside its decision write, under the firing mutex, after the
// pending creation has settled, so it sees the state the firing acts on.
type FireCheck func(Schedule) *FireVeto

// ScheduleOps is what the handler needs from the server's schedule machinery.
// Function fields, as the rest of TaskHandler's reach into Server is: the
// handler holds no *Server by design.
type ScheduleOps struct {
	Store *ScheduleStore
	// Fire is Server.fireScheduleChecked. Run answers it only after Ready:
	// start-up's Settle holds no firing mutex, so a firing must not overlap it.
	// A non-nil check is evaluated under the firing mutex against the schedule
	// as the firing will act on it (S-D17a); a veto records nothing.
	Fire func(id string, due time.Time, manual bool, check FireCheck) (protocol.ScheduleOutcome, *FireVeto)
	// Poke re-arms the timer loop (Server.pokeSchedules).
	Poke func()
	// Lock is the schedule's firing mutex (scheduleRuntime.firingMutex).
	Lock  func(id string) *sync.Mutex
	Ready func() bool
	Now   func() time.Time
	// OnBound publishes a task status event for a task whose schedule binding
	// changed (bound, unbound, its schedule deleted), so live surfaces gain or
	// drop its schedule_id. Wired to a task event by Server.New; nil in tests.
	OnBound func(taskHex string)

	// bindMu makes "this task is bound to no other schedule" and the write that
	// binds it one step: the store assumes one schedule per task.
	bindMu sync.Mutex
}

func (o *ScheduleOps) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *ScheduleOps) poke() {
	if o.Poke != nil {
		o.Poke()
	}
}

func (o *ScheduleOps) withFiringMutex(id string, f func()) {
	if o.Lock == nil {
		f()
		return
	}
	m := o.Lock(id)
	m.Lock()
	defer m.Unlock()
	f()
}

func scheduleRefusal(st protocol.ScheduleStatus, format string, args ...any) protocol.ScheduleResponse {
	r := protocol.ScheduleResponse{Status: st}
	if format != "" {
		r.SetMessage([]byte(fmt.Sprintf(format, args...)))
	}
	return r
}

func scheduleIDFromHex(idHex string) protocol.ScheduleID {
	var id protocol.ScheduleID
	raw, _ := hex.DecodeString(idHex)
	copy(id.Id[:], raw)
	return id
}

// handleSchedule dispatches the six kinds. create and edit read their prompt
// from the payload stream the spec names (off the receive loop, as an open's
// is: the stream's frames arrive on the loop this runs on); the bit was
// checked centrally (authority.CentralCap).
func (h *TaskHandler) handleSchedule(cid string, conn ConnHandle, req *protocol.TaskControlRequest) {
	kind := req.Header.Kind
	// deny answers a missing bit as the task-control permission denial (S-D17a),
	// the shape authority.CentralCap's own denial has.
	deny := func(c protocol.Capability) {
		h.denyTaskControl(conn, kind, req.Header.RequestId, c)
	}
	send := func(r protocol.ScheduleResponse) {
		resp := protocol.TaskControlResponse{Kind: kind, RequestId: req.Header.RequestId}
		switch kind {
		case protocol.TaskControlKind_ScheduleCreate:
			resp.SetScheduleCreate(r)
		case protocol.TaskControlKind_ScheduleEdit:
			resp.SetScheduleEdit(r)
		case protocol.TaskControlKind_ScheduleDelete:
			resp.SetScheduleDelete(r)
		case protocol.TaskControlKind_ScheduleRun:
			resp.SetScheduleRun(r)
		case protocol.TaskControlKind_ScheduleList:
			resp.SetScheduleList(r)
		case protocol.TaskControlKind_ScheduleShow:
			resp.SetScheduleShow(r)
		}
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	if h.Sched == nil || h.Sched.Store == nil {
		if id := payloadStreamOf(req); id != 0 {
			go discardPayloadStream(conn, id)
		}
		send(scheduleRefusal(protocol.ScheduleStatus_InternalError, "this server keeps no schedules"))
		return
	}
	switch kind {
	case protocol.TaskControlKind_ScheduleCreate:
		cr := req.ScheduleCreate()
		if cr == nil {
			send(scheduleRefusal(protocol.ScheduleStatus_InternalError, "empty request"))
			return
		}
		h.withSpecPrompt(conn, &cr.Spec, send, func(spec *protocol.ScheduleSpec, prompt string) {
			r, c := h.scheduleCreate(cid, spec, prompt)
			if c != 0 {
				deny(c)
				return
			}
			send(r)
		})
	case protocol.TaskControlKind_ScheduleEdit:
		er := req.ScheduleEdit()
		if er == nil {
			send(scheduleRefusal(protocol.ScheduleStatus_InternalError, "empty request"))
			return
		}
		id := er.Id
		h.withSpecPrompt(conn, &er.Spec, send, func(spec *protocol.ScheduleSpec, prompt string) {
			r, c := h.scheduleEdit(cid, id, spec, prompt)
			if c != 0 {
				deny(c)
				return
			}
			send(r)
		})
	case protocol.TaskControlKind_ScheduleDelete:
		r := req.ScheduleDelete()
		if r == nil {
			send(scheduleRefusal(protocol.ScheduleStatus_InternalError, "empty request"))
			return
		}
		send(h.scheduleDelete(cid, r.Id))
	case protocol.TaskControlKind_ScheduleRun:
		r := req.ScheduleRun()
		if r == nil {
			send(scheduleRefusal(protocol.ScheduleStatus_InternalError, "empty request"))
			return
		}
		resp, c := h.scheduleRun(cid, r.Id)
		if c != 0 {
			deny(c)
			return
		}
		send(resp)
	case protocol.TaskControlKind_ScheduleList:
		body := h.scheduleListBody(cid)
		respondWithBody(conn, "schedule list", &body, func(streamID uint64) {
			send(protocol.ScheduleResponse{Status: protocol.ScheduleStatus_Ok, StreamId: streamID})
		})
	case protocol.TaskControlKind_ScheduleShow:
		r := req.ScheduleShow()
		if r == nil {
			send(scheduleRefusal(protocol.ScheduleStatus_InternalError, "empty request"))
			return
		}
		detail, refusal := h.scheduleDetail(cid, r.Id)
		if refusal != nil {
			send(*refusal)
			return
		}
		respondWithBody(conn, "schedule detail", detail, func(streamID uint64) {
			resp := protocol.ScheduleResponse{Status: protocol.ScheduleStatus_Ok, StreamId: streamID, Id: r.Id}
			send(resp)
		})
	}
}

// withSpecPrompt runs then with the spec and its prompt. A spec that names a
// prompt stream has it read first, on its own goroutine, with the open's cap
// (readOpenPrompt); a refusal is answered, nothing is created or changed, and
// the stream is cancelled (Review Focus 5).
func (h *TaskHandler) withSpecPrompt(conn ConnHandle, spec *protocol.ScheduleSpec, send func(protocol.ScheduleResponse), then func(*protocol.ScheduleSpec, string)) {
	if !spec.PromptPresent() {
		then(spec, "")
		return
	}
	// The decoded frame is borrowed; a re-decoded copy owns its bytes.
	var own protocol.ScheduleSpec
	if err := own.DecodeExactCopy(spec.MustAppend(nil)); err != nil {
		if id := spec.PromptStreamId(); id != nil {
			go discardPayloadStream(conn, *id)
		}
		send(scheduleRefusal(protocol.ScheduleStatus_InternalError, "spec: %v", err))
		return
	}
	go func() {
		var streamID uint64
		if id := own.PromptStreamId(); id != nil {
			streamID = *id
		}
		prompt, st, msg := readOpenPrompt(conn, streamID)
		switch st {
		case protocol.OpenInteractiveStatus_Ok:
			then(&own, prompt)
		case protocol.OpenInteractiveStatus_PromptTooLarge:
			send(scheduleRefusal(protocol.ScheduleStatus_PromptTooLarge, "%s", msg))
		default:
			send(scheduleRefusal(protocol.ScheduleStatus_InternalError, "%s", msg))
		}
	}()
}

// --- identity and reach ---

func (h *TaskHandler) principalHex(cid string) string {
	p := h.lookupPrincipal(cid)
	if p.Id == ([16]byte{}) {
		return ""
	}
	return hex.EncodeToString(p.Id[:])
}

// scheduleTarget is the task whose reach decides who may act on sc (S-D17): its
// bound task, or while it has none its creator task ("" = the operator).
func scheduleTarget(sc Schedule) string {
	if sc.State == protocol.ScheduleTaskState_Bound {
		return sc.TaskID
	}
	return sc.Creator
}

// scheduleReach reports management reach through schedule_edit's own scope.
// Reading also admits a schedule_run holder's run reach (scheduleReadReach).
// A schedule with no target is the operator's.
func (h *TaskHandler) scheduleReach(cid string, sc Schedule) bool {
	target := scheduleTarget(sc)
	if target == "" {
		return h.principalHex(cid) == ""
	}
	return h.inScope(cid, requiredCentral(protocol.TaskControlKind_ScheduleEdit), target)
}

// scheduleRunReach uses the run bit's own scope independently of management
// reach. No target means the schedule belongs to the operator.
func (h *TaskHandler) scheduleRunReach(cid string, sc Schedule) bool {
	target := scheduleTarget(sc)
	if target == "" {
		return h.principalHex(cid) == ""
	}
	return h.inScope(cid, requiredCentral(protocol.TaskControlKind_ScheduleRun), target)
}

// scheduleReadReach preserves reads through management reach without requiring
// a capability, and lets run-only callers discover schedules they can fire.
// An absent run bit must not widen an existing read restriction through its
// default scope. This predicate is never used to authorize mutations.
func (h *TaskHandler) scheduleReadReach(cid string, sc Schedule) bool {
	return h.scheduleReach(cid, sc) ||
		(hasCap(h.callerCaps(cid), requiredCentral(protocol.TaskControlKind_ScheduleRun)) && h.scheduleRunReach(cid, sc))
}

// scheduleOwnTask is S-D20: the caller is the task bound to sc.
func (h *TaskHandler) scheduleOwnTask(cid string, sc Schedule) bool {
	self := h.principalHex(cid)
	return self != "" && sc.State == protocol.ScheduleTaskState_Bound && sc.TaskID == self
}

// scheduleFor is the schedule a verb names, once the caller may know it
// exists: an unknown id and one out of reach answer alike (never
// permission_denied, so ids are not probed).
func (h *TaskHandler) scheduleFor(cid string, id protocol.ScheduleID) (Schedule, *protocol.ScheduleResponse) {
	sc, ok := h.Sched.Store.Get(hex.EncodeToString(id.Id[:]))
	if !ok || !h.scheduleReach(cid, sc) {
		r := scheduleRefusal(protocol.ScheduleStatus_NoSuchSchedule, "")
		return Schedule{}, &r
	}
	return sc, nil
}

// --- create and edit ---

// scheduleDraft is a definition being built from a spec, and what the caller
// must do once it is stored.
type scheduleDraft struct {
	sc         Schedule
	bindTask   string // a task to bind: its mode is written background after the store change
	timeChange bool   // cron or zone changed: next_at is recomputed
}

func scheduleStatusFromOpen(st protocol.OpenInteractiveStatus) protocol.ScheduleStatus {
	switch st {
	case protocol.OpenInteractiveStatus_ProfileRequired:
		return protocol.ScheduleStatus_ProfileRequired
	case protocol.OpenInteractiveStatus_DeadlineRequired:
		return protocol.ScheduleStatus_DeadlineRequired
	case protocol.OpenInteractiveStatus_PromptRequired:
		return protocol.ScheduleStatus_PromptRequired
	case protocol.OpenInteractiveStatus_ProfileUnavailable:
		return protocol.ScheduleStatus_ProfileUnavailable
	case protocol.OpenInteractiveStatus_TurnEndUnsupported:
		return protocol.ScheduleStatus_TurnEndUnsupported
	}
	return protocol.ScheduleStatus_InternalError
}

// scheduleGrantRefusal is every grantFor refusal: an id outside the granter's
// reach or what it may delegate, an inconsistent scope.
func scheduleGrantRefusal(err error) protocol.ScheduleResponse {
	return scheduleRefusal(protocol.ScheduleStatus_ScopeNotPermitted, "%s", err.Error())
}

// specGrantRequest is the authority half of a spec, as grantFor reads an
// open's: a half the spec does not carry is zero with its presence bit off.
func specGrantRequest(spec *protocol.ScheduleSpec) grantRequest {
	var r grantRequest
	if spec.RequestedCapsPresent() && spec.RequestedCaps() != nil {
		r.Caps, r.CapsPresent = *spec.RequestedCaps(), true
	}
	if spec.ScopePresent() && spec.Scope() != nil {
		var ov []protocol.ScopeOverride
		if o := spec.Overrides(); o != nil {
			ov = *o
		}
		r.Scope, r.ScopePresent = authority.ScopeFromWire(*spec.Scope(), ov), true
	}
	if spec.DelegateCapsPresent() && spec.DelegateCaps() != nil {
		r.DelegateCaps, r.DelegateCapsPresent = *spec.DelegateCaps(), true
	}
	if spec.DelegateScopePresent() && spec.DelegateScope() != nil {
		r.DelegateScope, r.DelegateScopePresent = authority.ScopeFromWire(*spec.DelegateScope(), nil), true
	}
	if spec.SpawnDepthPresent() && spec.SpawnDepth() != nil {
		r.SpawnDepth, r.SpawnDepthPresent = *spec.SpawnDepth(), true
	}
	if spec.MaxDescendantsPresent() && spec.MaxDescendants() != nil {
		r.MaxDescendants, r.MaxDescendantsPresent = *spec.MaxDescendants(), true
	}
	return r
}

// grantRequestOver is the request an edit makes: the stored grant, every half
// present, with the halves the spec names replaced. An edit always recomputes
// the grant with the editor as granter (S-D16), so the stored one is the
// request, not the result.
func grantRequestOver(stored authority.Grant, spec *protocol.ScheduleSpec) grantRequest {
	r := grantRequest{
		Caps: stored.Caps, Scope: stored.Scope, DelegateCaps: stored.DelegateCaps, DelegateScope: stored.DelegateScope,
		SpawnDepth: stored.SpawnDepth, MaxDescendants: stored.MaxDescendants,
		CapsPresent: true, ScopePresent: true, DelegateCapsPresent: true, DelegateScopePresent: true,
		SpawnDepthPresent: true, MaxDescendantsPresent: true,
	}
	n := specGrantRequest(spec)
	if n.CapsPresent {
		r.Caps = n.Caps
	}
	if n.ScopePresent {
		r.Scope = n.Scope
	}
	if n.DelegateCapsPresent {
		r.DelegateCaps = n.DelegateCaps
	}
	if n.DelegateScopePresent {
		r.DelegateScope = n.DelegateScope
	}
	if n.SpawnDepthPresent {
		r.SpawnDepth = n.SpawnDepth
	}
	if n.MaxDescendantsPresent {
		r.MaxDescendants = n.MaxDescendants
	}
	return r
}

// storedGrant is the grant a schedule stores for r written onto task idHex
// ("" = the task it will create). Every half of r is present (a create over a
// task builds it with grantRequestOver, as an edit does), so the whole grant
// is bounded by the granter and a firing writes it whole.
func (h *TaskHandler) storedGrant(g *authority.Granter, r grantRequest, idHex string) (authority.Grant, error) {
	u, err := h.grantFor(g, r, idHex)
	if err != nil {
		return authority.Grant{}, err
	}
	return u.To, nil
}

// scheduleBinding is S-D14: the task a schedule is to be bound to must exist,
// be within the caller's reach, be bound to no other schedule, and not be
// running in foreground. own is the schedule being edited ("" on a create).
func (h *TaskHandler) scheduleBinding(cid string, taskHex, own string) (TaskEntry, *protocol.ScheduleResponse) {
	fail := func(st protocol.ScheduleStatus, format string, args ...any) (TaskEntry, *protocol.ScheduleResponse) {
		r := scheduleRefusal(st, format, args...)
		return TaskEntry{}, &r
	}
	t, ok := h.Tasks.Get(taskHex)
	if !ok || !h.inScope(cid, requiredCentral(protocol.TaskControlKind_ScheduleEdit), taskHex) {
		return fail(protocol.ScheduleStatus_TaskNotReachable, "no task %s within your reach", short(taskHex))
	}
	// A firing resumes its task onto a runner; a credential task has none and
	// is never resumed (credential-tasks D18).
	if t.Kind == protocol.TaskKind_Credential {
		return fail(protocol.ScheduleStatus_KindUnsupported, "task %s is a credential task: a schedule cannot run it", short(taskHex))
	}
	if other, bound := h.Sched.Store.BoundTo(taskHex); bound && other != own {
		return fail(protocol.ScheduleStatus_TaskAlreadyBound, "task %s is already bound to schedule %s", short(taskHex), short(other))
	}
	if t.RunMode == protocol.RunMode_Foreground && !scheduleTerminal(t.Status) {
		return fail(protocol.ScheduleStatus_TaskRunningForeground,
			"task %s is running in foreground: end it, or switch it with `session mode %s background`, first", short(taskHex), short(taskHex))
	}
	return t, nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// applySpec builds the definition a create or an edit stores: base with the
// spec's named fields applied, every S-D12 check made over the result. create
// is true for a new schedule; base then holds only its defaults.
func (h *TaskHandler) applySpec(cid string, base Schedule, spec *protocol.ScheduleSpec, prompt string, create bool, now time.Time) (scheduleDraft, *protocol.ScheduleResponse) {
	d := scheduleDraft{sc: base.clone()}
	sc := &d.sc
	refuse := func(st protocol.ScheduleStatus, format string, args ...any) (scheduleDraft, *protocol.ScheduleResponse) {
		r := scheduleRefusal(st, format, args...)
		return scheduleDraft{}, &r
	}
	callerCaps := h.callerCaps(cid)

	// Time (S-D1..S-D3): the expression and the zone, bounded before parsed.
	switch {
	case spec.CronPresent() && spec.Cron() != nil:
		expr := string(*spec.Cron())
		if len(expr) > protocol.ScheduleCronMax {
			return refuse(protocol.ScheduleStatus_InvalidCron, "cron expression is %d bytes (at most %d)", len(expr), protocol.ScheduleCronMax)
		}
		if _, err := parseCron(expr); err != nil {
			return refuse(protocol.ScheduleStatus_InvalidCron, "%v", err)
		}
		d.timeChange = d.timeChange || expr != sc.Cron
		sc.Cron = expr
	case create:
		return refuse(protocol.ScheduleStatus_InvalidCron, "a schedule needs --cron")
	}
	switch {
	case spec.TzPresent() && spec.Tz() != nil:
		name := string(*spec.Tz())
		if len(name) > protocol.ScheduleZoneMax {
			return refuse(protocol.ScheduleStatus_InvalidZone, "time zone name is %d bytes (at most %d)", len(name), protocol.ScheduleZoneMax)
		}
		_, zone, err := loadZone(name)
		if err != nil {
			return refuse(protocol.ScheduleStatus_InvalidZone, "%v", err)
		}
		d.timeChange = d.timeChange || zone != sc.TZ
		sc.TZ = zone
	case create:
		_, zone, err := loadZone("")
		if err != nil {
			return refuse(protocol.ScheduleStatus_InvalidZone, "%v", err)
		}
		sc.TZ = zone
	}

	// The task (S-D14): bound before any other check, because the repo, the
	// default agent and the run's size all follow from it.
	if spec.TaskPresent() && spec.TaskId() != nil {
		if spec.RepoPresent() {
			return refuse(protocol.ScheduleStatus_RepoConflict, "--repo with --task is refused: the repo is the task's")
		}
		taskHex := hex.EncodeToString(spec.TaskId().Id[:])
		own := ""
		if !create {
			own = sc.ID
		}
		t, refusal := h.scheduleBinding(cid, taskHex, own)
		if refusal != nil {
			return scheduleDraft{}, refusal
		}
		sc.State, sc.TaskID, sc.Repo = protocol.ScheduleTaskState_Bound, taskHex, t.RepoPath
		d.bindTask = taskHex
		if create && !spec.AgentPresent() {
			sc.Agent = t.AgentProfile // a resume stays on the task's agent
		}
	} else if spec.RepoPresent() && spec.RepoPath() != nil {
		if !create && sc.State == protocol.ScheduleTaskState_Bound {
			return refuse(protocol.ScheduleStatus_RepoConflict, "the repo is the bound task's; unbind the task to change it")
		}
		sc.Repo = path.Clean(string(*spec.RepoPath()))
	}
	if create && sc.State != protocol.ScheduleTaskState_Bound {
		if !spec.RepoPresent() || sc.Repo == "" || sc.Repo == "." {
			return refuse(protocol.ScheduleStatus_RepoRequired, "a schedule needs --repo or --task")
		}
		sc.State = protocol.ScheduleTaskState_PendingCreate
	}

	// The run.
	if spec.AgentPresent() && spec.AgentProfile() != nil {
		sc.Agent = string(*spec.AgentProfile())
	}
	if spec.SelectorPresent() && spec.Selector() != nil {
		sc.Selector = cloneSelector(*spec.Selector())
	}
	if spec.ExtraArgsPresent() && spec.ExtraArgs() != nil {
		sc.ExtraArgs = spec.ExtraArgs().AsStrings()
	}
	if spec.ResumeConversationSet() {
		sc.ResumeConversation = spec.ResumeConversation()
	}
	sc.Run.Mode = protocol.RunMode_Background
	// The kind (2026-11-17-schedule-stream-kind-design.md, K1-K6): set before
	// the conditions, whose defaults depend on it.
	kindChanged := false
	prevKind := sc.runKind()
	switch {
	case spec.KindPresent():
		k := protocol.TaskKind(255)
		if v := spec.Kind(); v != nil {
			k = *v
		}
		if k != protocol.TaskKind_Interactive && k != protocol.TaskKind_Stream {
			return refuse(protocol.ScheduleStatus_KindUnsupported, "kind %s: a schedule runs interactive or stream", k)
		}
		kindChanged = create || k != prevKind
		sc.Kind = k
	case create:
		sc.Kind = protocol.TaskKind_Interactive
	}
	if spec.OneshotSet() {
		sc.Run.Oneshot = spec.Oneshot()
	}
	switch {
	case spec.PtySet():
		sc.Run.NoPTY = !spec.Pty()
	case create || spec.OneshotSet():
		sc.Run.NoPTY = sc.Run.Oneshot // the mode's default (A-D33)
	}
	switch {
	case spec.RecordSet():
		sc.Run.Record = spec.Record()
		// Saying on needs record_persist, within the bit's scope over a bound
		// task (the open's rule); off, and saying nothing, need nothing.
		if sc.Run.Record {
			if !hasCap(callerCaps, authority.RecordOnCap) {
				return refuse(protocol.ScheduleStatus_RecordNotPermitted, "recording a session needs record_persist")
			}
			if sc.State == protocol.ScheduleTaskState_Bound && !h.inScope(cid, authority.RecordOnCap, sc.TaskID) {
				return refuse(protocol.ScheduleStatus_RecordNotPermitted, "recording a session needs record_persist within reach of task %s", short(sc.TaskID))
			}
		}
	case create:
		sc.Run.Record = h.RecordDefault
	}
	// The conditions (R17): each has its own presence bit, and only the present
	// ones overlay the stored block, here under the schedule's firing mutex (an
	// edit holds it) so two edits of different fields both survive. A present
	// zero is an answer: queue_timeout 0 = none; quiet / blocked 0 and an unset
	// turn_end = the defaults (WithDefaults); deadline 0 is refused below
	// (G41). Size lives beside them and is untouched.
	// A kind switch takes the stored turn-end back to unset when it is only the
	// old kind's default (any / lifecycle), so the new kind's default applies
	// and a switch back restores the old one. A turn-end the edit names, or one
	// that differs from the old default, is kept and judged below.
	if kindChanged && !create && !spec.TurnEndPresent() &&
		sc.Run.Cond.TurnEnd == (RunConditions{}).WithDefaults(prevKind).TurnEnd {
		sc.Run.Cond.TurnEnd = protocol.TurnEndSignal_Unset
	}
	if spec.DeadlinePresent() || spec.QuietPresent() || spec.BlockedPresent() || spec.QueueTimeoutPresent() || spec.TurnEndPresent() {
		cond := sc.Run.Cond
		ms := func(p *uint64) time.Duration {
			if p == nil {
				return 0
			}
			return time.Duration(*p) * time.Millisecond
		}
		if spec.DeadlinePresent() {
			cond.Deadline = ms(spec.DeadlineMs())
		}
		if spec.QuietPresent() {
			cond.Quiet = ms(spec.QuietMs())
		}
		if spec.BlockedPresent() {
			cond.Blocked = ms(spec.BlockedMs())
		}
		if spec.QueueTimeoutPresent() {
			cond.QueueTimeout = ms(spec.QueueTimeoutMs())
		}
		if spec.TurnEndPresent() {
			cond.TurnEnd = protocol.TurnEndSignal_Unset
			if v := spec.TurnEnd(); v != nil {
				cond.TurnEnd = *v
			}
		}
		sc.Run.Cond = cond.WithDefaults(sc.runKind())
	} else if kindChanged {
		sc.Run.Cond = sc.Run.Cond.WithDefaults(sc.runKind())
	}
	if spec.SizePresent() && spec.InitialRows() != nil && spec.InitialCols() != nil {
		r, c := *spec.InitialRows(), *spec.InitialCols()
		if r == 0 || c == 0 {
			r, c = 0, 0
		}
		sc.Run.Cond.Rows, sc.Run.Cond.Cols = r, c
	}
	if create || spec.PromptPresent() {
		sc.Prompt = prompt
	}

	// The open's stream rules (task_handler.go, OneshotStream / PtyStream).
	// --oneshot is judged on the schedule as it would be stored, so an edit
	// naming only the kind meets a stored --oneshot. --pty is judged on this
	// request only: the store keeps no "pty was named" bit, so a stored PTY is
	// indistinguishable from the interactive default; it is kept as it is (a
	// switch back restores it) and the firing runs a stream with no PTY.
	if sc.runKind() == protocol.TaskKind_Stream {
		if sc.Run.Oneshot {
			return refuse(protocol.ScheduleStatus_OneshotStream, "--oneshot runs in a PTY; an event-stream session has none")
		}
		if spec.PtySet() && spec.Pty() {
			return refuse(protocol.ScheduleStatus_PtyStream, "--pty: an event-stream session has no PTY")
		}
	}

	// S-D12: the checks a background open makes, through the same function.
	wc := sc.Run.Cond.Wire()
	if st, msg := h.checkBackgroundRun(sc.runKind(), sc.Agent, &wc, sc.Prompt, sc.Repo, sc.Selector); st != protocol.OpenInteractiveStatus_Ok {
		return refuse(scheduleStatusFromOpen(st), "%s", msg)
	}
	if create && len(h.Registry.Candidates(sc.Repo, sc.Selector)) == 0 {
		return refuse(protocol.ScheduleStatus_NoCandidate, "no connected runner serves %s", sc.Repo)
	}
	return d, nil
}

// scheduleSpecTask is the task a spec binds, "" when it names none.
func scheduleSpecTask(spec *protocol.ScheduleSpec) string {
	if spec.TaskPresent() && spec.TaskId() != nil {
		return hex.EncodeToString(spec.TaskId().Id[:])
	}
	return ""
}

// schedulePower is S-D17a for new and edit: besides the schedule bit, a verb
// needs the power it exercises. Each need is a bit (a missing one is returned
// as the capability to deny with) and, for resume, a target inside that bit's
// scope (a target outside answers task_not_reachable). resume_exact is not
// enough here: binding a task or editing a schedule bound to one chooses the
// prompt a firing passes. It runs after reach and self (S-D20), so a missing
// bit never tells a caller anything about a schedule it cannot see.
//
//   - spawn: the schedule is to create its task (no --task on a create, or a
//     stored pending_create schedule).
//   - resume over T: a bind of T, or a stored schedule bound to T.
//
// An unbound schedule needs neither: an edit that binds nothing changes
// nothing that runs until a later bind (which is itself checked here), and a
// run of it only records skipped_no_task. `schedule run` has its own rule,
// scheduleRunPower: the firer picks timing only, and the prompt was set by
// someone who held full resume over T at bind (below) or edit.
func (h *TaskHandler) schedulePower(cid string, cur *Schedule, bindTask string) (protocol.Capability, *protocol.ScheduleResponse) {
	callerCaps := h.callerCaps(cid)
	// A bind means nothing will be created, so a bind needs no spawn even on a
	// pending_create schedule (authority.SchedulePower decides which).
	pendingOrNew := cur == nil || cur.State == protocol.ScheduleTaskState_PendingCreate
	bound := cur != nil && cur.State == protocol.ScheduleTaskState_Bound
	for _, c := range authority.SchedulePower(pendingOrNew, bound, bindTask != "") {
		if !hasAnyCap(callerCaps, c.AnyOf) {
			return c.Report, nil
		}
	}
	// The task bound or being bound is reached through the bit that binding
	// needs: the one clause a bound schedule carries.
	resumeBit := authority.SchedulePower(false, true, false)[0].AnyOf
	var over []string
	if bound {
		over = append(over, cur.TaskID)
	}
	if bindTask != "" {
		over = append(over, bindTask)
	}
	for _, t := range over {
		if !h.inScope(cid, resumeBit, t) {
			r := scheduleRefusal(protocol.ScheduleStatus_TaskNotReachable, "no task %s within your reach", short(t))
			return 0, &r
		}
	}
	return 0, nil
}

// scheduleRunPower is S-D17a for `schedule run` (amended 2026-11-22): a
// pending_create schedule needs spawn, an unbound one needs nothing, and one
// bound to T needs resume OR resume_exact with T inside the scope of a bit the
// caller holds (resumeTargetAllowed). A missing bit is returned as the mask a
// resume of that exactness needs; a target out of scope answers
// task_not_reachable.
func (h *TaskHandler) scheduleRunPower(cid string, sc *Schedule) (protocol.Capability, *protocol.ScheduleResponse) {
	callerCaps := h.callerCaps(cid)
	for _, c := range authority.ScheduleRunPower(sc.State) {
		if !hasAnyCap(callerCaps, c.AnyOf) {
			return c.Report, nil
		}
	}
	if sc.State == protocol.ScheduleTaskState_Bound {
		if !h.resumeTargetAllowed(cid, true, sc.TaskID) {
			r := scheduleRefusal(protocol.ScheduleStatus_TaskNotReachable, "no task %s within your reach", short(sc.TaskID))
			return 0, &r
		}
	}
	return 0, nil
}

func (h *TaskHandler) scheduleCreate(cid string, spec *protocol.ScheduleSpec, prompt string) (protocol.ScheduleResponse, protocol.Capability) {
	if c, refusal := h.schedulePower(cid, nil, scheduleSpecTask(spec)); c != 0 || refusal != nil {
		if refusal != nil {
			return *refusal, 0
		}
		return protocol.ScheduleResponse{}, c
	}
	return h.scheduleCreateChecked(cid, spec, prompt), 0
}

func (h *TaskHandler) scheduleCreateChecked(cid string, spec *protocol.ScheduleSpec, prompt string) protocol.ScheduleResponse {
	ops := h.Sched
	now := ops.now()
	d, refusal := h.applySpec(cid, Schedule{}, spec, prompt, true, now)
	if refusal != nil {
		return *refusal
	}
	sc := d.sc
	self := h.principalHex(cid)

	// The spawn rule (S-D12): a creation by a task is checked as a dry run
	// against its creator. A bound task is not created, so no limit applies.
	if d.bindTask == "" {
		if err := h.Tasks.CheckSpawnLimits(taskIDFromHex(self)); err != nil {
			return scheduleRefusal(protocol.ScheduleStatus_SpawnLimit, "%s", err.Error())
		}
	}
	// Authority (S-D16): as for a new child of the creator, or as a resume over
	// the bound task.
	g := h.granter(cid, h.callerCaps(cid))
	req := specGrantRequest(spec)
	if d.bindTask != "" {
		// Every half present (R15): the task's own grant is only the starting
		// point, so the stored grant is bounded by the creator.
		rec, _ := h.Tasks.Get(d.bindTask)
		req = grantRequestOver(rec.Grant(), spec)
	}
	grant, err := h.storedGrant(g, req, d.bindTask)
	if err != nil {
		return scheduleGrantRefusal(err)
	}
	sc.Grant = grant
	sc.ID = newTaskID()
	sc.Creator = self
	sc.CreatedAt = now
	next, err := nextFor(&sc, now)
	if err != nil || next.IsZero() {
		return nextForRefusal(err)
	}
	sc.NextAt = next

	ops.bindMu.Lock()
	if d.bindTask != "" {
		// Re-checked under the lock that makes the binding.
		if other, bound := ops.Store.BoundTo(d.bindTask); bound {
			ops.bindMu.Unlock()
			return scheduleRefusal(protocol.ScheduleStatus_TaskAlreadyBound, "task %s is already bound to schedule %s", short(d.bindTask), short(other))
		}
	}
	// The mode is written first, under TaskStore.mu: a task a concurrent
	// foreground resume took is refused, and then no schedule is stored bound
	// to it.
	if refusal := h.writeBindingMode(d.bindTask, sc.Run.Cond, now); refusal != nil {
		ops.bindMu.Unlock()
		return *refusal
	}
	err = ops.Store.Put(sc)
	ops.bindMu.Unlock()
	if err != nil {
		slog.Error("schedule create: store write failed", "err", err)
		return scheduleRefusal(protocol.ScheduleStatus_InternalError, "could not save the schedule: %v", err)
	}
	ops.boundChanged(d.bindTask)
	ops.poke()
	r := protocol.ScheduleResponse{Status: protocol.ScheduleStatus_Ok, Id: scheduleIDFromHex(sc.ID)}
	return r
}

// writeBindingMode is S-D14's "binding writes the task's mode to background",
// through the task mode write (task_mode_changed). A task already in
// background is left alone; one running in foreground is refused.
func (h *TaskHandler) writeBindingMode(taskHex string, cond RunConditions, now time.Time) *protocol.ScheduleResponse {
	if taskHex == "" {
		return nil
	}
	err := h.Tasks.BindBackground(taskHex, cond, now.UnixNano())
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrBindForeground):
		r := scheduleRefusal(protocol.ScheduleStatus_TaskRunningForeground, "task %s is running in foreground", short(taskHex))
		return &r
	}
	r := scheduleRefusal(protocol.ScheduleStatus_InternalError, "writing the task's mode: %v", err)
	return &r
}

// nextForRefusal answers a nextFor that failed or found no firing: a zone that
// no longer loads is invalid_zone (naming it, via err), anything else is the
// five-year answer.
func nextForRefusal(err error) protocol.ScheduleResponse {
	if errors.Is(err, errZoneLoad) {
		return scheduleRefusal(protocol.ScheduleStatus_InvalidZone, "%v", err)
	}
	return scheduleRefusal(protocol.ScheduleStatus_InvalidCron, "no firing within five years")
}

func (o *ScheduleOps) boundChanged(taskHex string) {
	if taskHex != "" && o.OnBound != nil {
		o.OnBound(taskHex)
	}
}

func (h *TaskHandler) scheduleEdit(cid string, id protocol.ScheduleID, spec *protocol.ScheduleSpec, prompt string) (resp protocol.ScheduleResponse, deny protocol.Capability) {
	ops := h.Sched
	idHex := hex.EncodeToString(id.Id[:])
	ops.withFiringMutex(idHex, func() {
		resp, deny = h.scheduleEditLocked(cid, id, idHex, spec, prompt)
	})
	if deny == 0 && resp.Status == protocol.ScheduleStatus_Ok {
		ops.poke()
	}
	return resp, deny
}

func (h *TaskHandler) scheduleEditLocked(cid string, id protocol.ScheduleID, idHex string, spec *protocol.ScheduleSpec, prompt string) (protocol.ScheduleResponse, protocol.Capability) {
	cur, refusal := h.scheduleFor(cid, id)
	if refusal != nil {
		return *refusal, 0
	}
	if h.scheduleOwnTask(cid, cur) {
		return scheduleRefusal(protocol.ScheduleStatus_SelfRefused, "a schedule's own task cannot edit it"), 0
	}
	if c, refusal := h.schedulePower(cid, &cur, scheduleSpecTask(spec)); c != 0 || refusal != nil {
		if refusal != nil {
			return *refusal, 0
		}
		return protocol.ScheduleResponse{}, c
	}
	return h.scheduleEditChecked(cid, id, idHex, spec, prompt, cur), 0
}

func (h *TaskHandler) scheduleEditChecked(cid string, id protocol.ScheduleID, idHex string, spec *protocol.ScheduleSpec, prompt string, cur Schedule) protocol.ScheduleResponse {
	ops := h.Sched
	now := ops.now()
	d, refusal := h.applySpec(cid, cur, spec, prompt, false, now)
	if refusal != nil {
		return *refusal
	}
	next := d.sc
	// Every edit recomputes the authority with the editor as granter (S-D16),
	// flags or not: the stored grant is the request, bounded by the editor.
	g := h.granter(cid, h.callerCaps(cid))
	boundHex := ""
	if next.State == protocol.ScheduleTaskState_Bound {
		boundHex = next.TaskID // the task the grant is written over: the new one on a rebind
	}
	grant, err := h.storedGrant(g, grantRequestOver(cur.Grant, spec), boundHex)
	if err != nil {
		return scheduleGrantRefusal(err)
	}
	next.Grant = grant
	if d.timeChange {
		n, err := nextFor(&next, now)
		if err != nil || n.IsZero() {
			return nextForRefusal(err)
		}
		next.NextAt = n
	}
	next.EditedAt, next.EditedBy, next.EditedByKind = now, h.principalHex(cid), h.lookupClientKind(cid)

	apply := func(sc *Schedule) error {
		sc.Cron, sc.TZ, sc.Repo, sc.Agent, sc.Selector = next.Cron, next.TZ, next.Repo, next.Agent, next.Selector
		sc.Prompt, sc.ExtraArgs, sc.ResumeConversation = next.Prompt, next.ExtraArgs, next.ResumeConversation
		sc.Kind, sc.Run, sc.Grant = next.Kind, next.Run, next.Grant
		sc.EditedAt, sc.EditedBy, sc.EditedByKind = next.EditedAt, next.EditedBy, next.EditedByKind
		if d.timeChange {
			sc.NextAt = next.NextAt
		}
		if d.bindTask != "" {
			sc.State, sc.TaskID = protocol.ScheduleTaskState_Bound, d.bindTask
		}
		return nil
	}
	var err2 error
	if d.bindTask != "" {
		ops.bindMu.Lock()
		if other, bound := ops.Store.BoundTo(d.bindTask); bound && other != idHex {
			ops.bindMu.Unlock()
			return scheduleRefusal(protocol.ScheduleStatus_TaskAlreadyBound, "task %s is already bound to schedule %s", short(d.bindTask), short(other))
		}
		if refusal := h.writeBindingMode(d.bindTask, next.Run.Cond, now); refusal != nil {
			ops.bindMu.Unlock()
			return *refusal
		}
		err2 = ops.Store.Update(idHex, apply)
		ops.bindMu.Unlock()
	} else {
		err2 = ops.Store.Update(idHex, apply)
	}
	switch {
	case errors.Is(err2, ErrScheduleNotFound):
		return scheduleRefusal(protocol.ScheduleStatus_NoSuchSchedule, "")
	case err2 != nil:
		slog.Error("schedule edit: store write failed", "schedule", idHex, "err", err2)
		return scheduleRefusal(protocol.ScheduleStatus_InternalError, "could not save the schedule: %v", err2)
	}
	if d.bindTask != "" {
		if cur.State == protocol.ScheduleTaskState_Bound && cur.TaskID != d.bindTask {
			ops.boundChanged(cur.TaskID) // the old task loses its schedule id
		}
		ops.boundChanged(d.bindTask)
	}
	return protocol.ScheduleResponse{Status: protocol.ScheduleStatus_Ok, Id: id}
}

// --- delete and run ---

func (h *TaskHandler) scheduleDelete(cid string, id protocol.ScheduleID) (resp protocol.ScheduleResponse) {
	ops := h.Sched
	idHex := hex.EncodeToString(id.Id[:])
	ops.withFiringMutex(idHex, func() {
		sc, refusal := h.scheduleFor(cid, id)
		if refusal != nil {
			resp = *refusal
			return
		}
		if h.scheduleOwnTask(cid, sc) {
			resp = scheduleRefusal(protocol.ScheduleStatus_SelfRefused, "a schedule's own task cannot delete it")
			return
		}
		// The task and a run in progress are untouched (S-D19).
		switch err := ops.Store.Delete(idHex); {
		case errors.Is(err, ErrScheduleNotFound):
			resp = scheduleRefusal(protocol.ScheduleStatus_NoSuchSchedule, "")
		case err != nil:
			slog.Error("schedule delete: store write failed", "schedule", idHex, "err", err)
			resp = scheduleRefusal(protocol.ScheduleStatus_InternalError, "could not save: %v", err)
		default:
			resp = protocol.ScheduleResponse{Status: protocol.ScheduleStatus_Ok, Id: id}
			if sc.State == protocol.ScheduleTaskState_Bound {
				ops.boundChanged(sc.TaskID)
			}
		}
	})
	if resp.Status == protocol.ScheduleStatus_Ok {
		ops.poke()
	}
	return resp
}

// scheduleRun fires the schedule now (S-D13). It pokes nothing: a manual
// firing does not move next_at. fireSchedule returns the zero outcome for a
// schedule that vanished or whose intent could not be saved; the first is
// answered no_such_schedule before it is called, so the zero outcome means
// the second.
func (h *TaskHandler) scheduleRun(cid string, id protocol.ScheduleID) (protocol.ScheduleResponse, protocol.Capability) {
	ops := h.Sched
	idHex := hex.EncodeToString(id.Id[:])
	check := h.scheduleRunCheck(cid)
	veto := func(v *FireVeto) (protocol.ScheduleResponse, protocol.Capability) {
		if v.Refusal != nil {
			return *v.Refusal, 0
		}
		return protocol.ScheduleResponse{}, v.Deny
	}
	// An early answer from a lock-free read, so a refusal comes before
	// NotReady; the authoritative check is the one the firing makes under its
	// mutex, against the state it acts on.
	sc, ok := ops.Store.Get(idHex)
	if !ok {
		return scheduleRefusal(protocol.ScheduleStatus_NoSuchSchedule, ""), 0
	}
	if v := check(sc); v != nil {
		return veto(v)
	}
	if ops.Fire == nil || ops.Ready == nil || !ops.Ready() {
		return scheduleRefusal(protocol.ScheduleStatus_NotReady, "the server is still starting its schedules; try again"), 0
	}
	outcome, v := ops.Fire(idHex, ops.now(), true, check)
	if v != nil {
		return veto(v)
	}
	if outcome == protocol.ScheduleOutcome_Pending {
		return scheduleRefusal(protocol.ScheduleStatus_InternalError, "the firing could not be recorded; nothing was started"), 0
	}
	msg := scheduleOutcomeName(outcome)
	if after, ok := ops.Store.Get(idHex); ok {
		for i := len(after.History) - 1; i >= 0; i-- {
			if e := after.History[i]; e.Outcome == outcome && e.Message != "" {
				msg += ": " + e.Message
				break
			}
		}
	}
	return scheduleOK(id, msg), 0
}

// scheduleRunCheck is `schedule run`'s FireCheck: reach (no_such_schedule),
// the schedule's own task (S-D20), then what S-D17a needs for a run
// (scheduleRunPower), in that order.
func (h *TaskHandler) scheduleRunCheck(cid string) FireCheck {
	return func(sc Schedule) *FireVeto {
		if !h.scheduleRunReach(cid, sc) {
			r := scheduleRefusal(protocol.ScheduleStatus_NoSuchSchedule, "")
			return &FireVeto{Refusal: &r}
		}
		if h.scheduleOwnTask(cid, sc) {
			r := scheduleRefusal(protocol.ScheduleStatus_SelfRefused, "a schedule's own task cannot run it")
			return &FireVeto{Refusal: &r}
		}
		if c, refusal := h.scheduleRunPower(cid, &sc); c != 0 || refusal != nil {
			return &FireVeto{Refusal: refusal, Deny: c}
		}
		return nil
	}
}

// scheduleOutcomeName is the outcome as the spec and the history write it.
func scheduleOutcomeName(o protocol.ScheduleOutcome) string {
	switch o {
	case protocol.ScheduleOutcome_Pending:
		return "pending"
	case protocol.ScheduleOutcome_Lost:
		return "lost"
	case protocol.ScheduleOutcome_Fired:
		return "fired"
	case protocol.ScheduleOutcome_RunNow:
		return "run_now"
	case protocol.ScheduleOutcome_SkippedBusy:
		return "skipped_busy"
	case protocol.ScheduleOutcome_SkippedNoTask:
		return "skipped_no_task"
	case protocol.ScheduleOutcome_SkippedTaskGone:
		return "skipped_task_gone"
	case protocol.ScheduleOutcome_SkippedRefused:
		return "skipped_refused"
	case protocol.ScheduleOutcome_ResumedBy:
		return "resumed_by"
	case protocol.ScheduleOutcome_Unbound:
		return "unbound"
	}
	return o.String()
}

func scheduleOK(id protocol.ScheduleID, msg string) protocol.ScheduleResponse {
	r := protocol.ScheduleResponse{Status: protocol.ScheduleStatus_Ok, Id: id}
	r.SetMessage([]byte(msg))
	return r
}

// --- list and show ---

func unixNano(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano())
}

// promptHead is the first ScheduleHeadMax bytes of p, cut on a UTF-8 boundary.
func promptHead(p string) []byte {
	if len(p) <= protocol.ScheduleHeadMax {
		return []byte(p)
	}
	cut := protocol.ScheduleHeadMax
	for cut > 0 && !utf8.RuneStart(p[cut]) {
		cut--
	}
	return []byte(p[:cut])
}

func historyEntryWire(e HistoryEntry) protocol.ScheduleHistoryEntry {
	w := protocol.ScheduleHistoryEntry{
		Outcome: e.Outcome, DueAt: unixNano(e.DueAt), At: unixNano(e.At),
		QueuedAt: unixNano(e.QueuedAt), StartedAt: unixNano(e.StartedAt), EndedAt: unixNano(e.EndedAt),
		EndReason: e.EndReason, ByKind: e.ByKind, By: taskIDFromHex(e.By),
	}
	w.SetMessage([]byte(e.Message))
	return w
}

// scheduleRow is one `schedule ls` line. The prompt is shown only where the
// caller could read the target's prompt in `ls` (S-D17): its visible set.
func (h *TaskHandler) scheduleRow(sc Schedule, visAll bool, visible map[string]bool) protocol.ScheduleRow {
	row := protocol.ScheduleRow{Id: scheduleIDFromHex(sc.ID), TaskState: sc.State, NextAt: unixNano(sc.NextAt)}
	if sc.State == protocol.ScheduleTaskState_Bound {
		row.TaskId = taskIDFromHex(sc.TaskID)
		if t, ok := h.Tasks.Get(sc.TaskID); ok {
			row.TaskStatus = t.Status
		}
	}
	row.SetCron([]byte(sc.Cron))
	row.SetTz([]byte(sc.TZ))
	row.SetAgent([]byte(sc.Agent))
	row.SetRepo([]byte(sc.Repo))
	if target := scheduleTarget(sc); target != "" && !visAll && !visible[target] {
		row.SetPromptHidden(true)
	} else {
		row.SetPromptHead(promptHead(sc.Prompt))
	}
	if n := len(sc.History); n > 0 {
		row.SetLastPresent(true)
		row.SetLast(historyEntryWire(sc.History[n-1]))
	}
	return row
}

func (h *TaskHandler) scheduleListBody(cid string) protocol.ScheduleListBody {
	visAll, visible := h.visibleToCaller(cid)
	var rows []protocol.ScheduleRow
	for _, sc := range h.Sched.Store.List() {
		if h.scheduleReadReach(cid, sc) {
			rows = append(rows, h.scheduleRow(sc, visAll, visible))
		}
	}
	var body protocol.ScheduleListBody
	body.SetRows(rows)
	return body
}

func (h *TaskHandler) scheduleDetail(cid string, id protocol.ScheduleID) (*protocol.ScheduleDetail, *protocol.ScheduleResponse) {
	sc, ok := h.Sched.Store.Get(hex.EncodeToString(id.Id[:]))
	if !ok || !h.scheduleReadReach(cid, sc) {
		r := scheduleRefusal(protocol.ScheduleStatus_NoSuchSchedule, "")
		return nil, &r
	}
	visAll, visible := h.visibleToCaller(cid)
	d := &protocol.ScheduleDetail{
		Row:        h.scheduleRow(sc, visAll, visible),
		ExtraArgs:  protocol.AgentArgsFromStrings(sc.ExtraArgs),
		Selector:   cloneSelector(sc.Selector),
		Conditions: sc.Run.Cond.Wire(),
		Caps:       sc.Grant.Caps, DelegateCaps: sc.Grant.DelegateCaps,
		Scope: sc.Grant.Scope.ToWire(), DelegateScope: sc.Grant.DelegateScope.ToWire(),
		SpawnDepth: sc.Grant.SpawnDepth, MaxDescendants: sc.Grant.MaxDescendants,
		Creator: taskIDFromHex(sc.Creator), CreatedAt: unixNano(sc.CreatedAt), EditedAt: unixNano(sc.EditedAt),
		EditedByKind: sc.EditedByKind, EditedBy: taskIDFromHex(sc.EditedBy),
	}
	d.SetResumeConversation(sc.ResumeConversation)
	d.SetOneshot(sc.Run.Oneshot)
	d.Kind = sc.runKind()
	d.SetRecordSet(true)
	d.SetRecord(sc.Run.Record)
	d.SetPtySet(true)
	d.SetPty(!sc.Run.NoPTY)
	if !d.Row.PromptHidden() {
		d.SetPrompt([]byte(sc.Prompt))
	}
	d.SetOverrides(sc.Grant.Scope.OverridesToWire())
	hist := make([]protocol.ScheduleHistoryEntry, 0, len(sc.History))
	for _, e := range sc.History {
		hist = append(hist, historyEntryWire(e))
	}
	d.SetHistory(hist)
	return d, nil
}

// --- unbinding (S-D15) ---

// unbindSchedule is the S-D15 effect: the task bound to a schedule goes back
// to a plain background task, the schedule keeps its definition and shows no
// task, and `unbound` (who, when) joins its history. It holds the schedule's
// firing mutex, so a firing in flight is not overlapped.
func (h *TaskHandler) unbindSchedule(taskHex string, by protocol.ClientKind, actor protocol.TaskID) {
	ops := h.Sched
	if ops == nil || ops.Store == nil {
		return
	}
	id, ok := ops.Store.BoundTo(taskHex)
	if !ok {
		return
	}
	at := ops.now()
	var by16 string
	if actor.Id != ([16]byte{}) {
		by16 = hex.EncodeToString(actor.Id[:])
	}
	ops.withFiringMutex(id, func() {
		_, err := ops.Store.UpdateForTask(taskHex, func(sc *Schedule) bool {
			if sc.State != protocol.ScheduleTaskState_Bound || sc.TaskID != taskHex {
				return false
			}
			sc.State, sc.TaskID = protocol.ScheduleTaskState_Unbound, ""
			sc.appendHistory(HistoryEntry{Outcome: protocol.ScheduleOutcome_Unbound, At: at, TaskID: taskHex, ByKind: by, By: by16})
			return true
		})
		if err != nil {
			slog.Error("schedule unbind: store write failed", "schedule", id, "task", taskHex, "err", err)
			return
		}
		ops.boundChanged(taskHex)
	})
}

// boundScheduleOf is the schedule a task is bound to, "" for none.
func (h *TaskHandler) boundScheduleOf(taskHex string) string {
	if h.Sched == nil || h.Sched.Store == nil {
		return ""
	}
	id, _ := h.Sched.Store.BoundTo(taskHex)
	return id
}

// openUnbinds reports whether an open is a foreground resume of a task bound to
// a schedule, which S-D15 gives the schedule bit as well.
func (h *TaskHandler) openUnbinds(oi *protocol.OpenInteractiveRequest) (taskHex string, ok bool) {
	if isZeroTaskID(oi.ResumeTaskId) {
		return "", false
	}
	taskHex = hex.EncodeToString(oi.ResumeTaskId.Id[:])
	if h.resolveRunMode(oi, taskHex) != protocol.RunMode_Foreground || h.boundScheduleOf(taskHex) == "" {
		return "", false
	}
	return taskHex, true
}

// unbindRefusal is what S-D15 and S-D20 add to a foreground resume or a mode
// switch of a bound task beyond the bit (checked by the caller): the task must
// be within the caller's schedule reach, and may not be the caller itself.
func (h *TaskHandler) unbindRefusal(cid, taskHex string) (reach, self bool) {
	reach = h.inScope(cid, authority.UnbindCap, taskHex)
	self = h.principalHex(cid) == taskHex
	return reach, self
}

// BindBackground writes background as the run mode of a task a schedule is
// bound to (S-D14), through the same record as a mode switch
// (task_mode_changed). SwitchRunMode refuses a terminal task, which is the
// only kind that can be in another mode here: binding refuses a task running
// in foreground, so an active task is already background and needs no write.
func (s *TaskStore) BindBackground(id string, cond RunConditions, now int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tasks[id]
	if !ok {
		return ErrSwitchNotFound
	}
	if e.RunMode == protocol.RunMode_Background {
		return nil
	}
	if !scheduleTerminal(e.Status) {
		return ErrBindForeground // it would become a background run with no deadline
	}
	e.RunMode = protocol.RunMode_Background
	e.ModeSwitchedAt = now
	e.Cond = cond
	e.DeadlineAt = 0
	if s.wal != nil {
		ev := WALEvent{Type: "task_mode_changed", TaskID: id, RunMode: uint8(e.RunMode), DeadlineNs: 0, SwitchedAtNs: now, Ts: now}
		e.Cond.toWAL(&ev)
		if err := s.wal.Write(ev); err != nil {
			slog.Error("WAL write failed", "op", "task_mode_changed", "task_id", id, "err", err)
		}
	}
	return nil
}

// ErrBindForeground is BindBackground refusing a task that is running in
// foreground: it would become a background run with no deadline.
var ErrBindForeground = errors.New("task is running in foreground")
