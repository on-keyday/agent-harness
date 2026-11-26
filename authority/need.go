package authority

import "github.com/on-keyday/agent-harness/runner/protocol"

// This file is the one answer to "which capability bits does this request
// need" (docs/superpowers/specs/2026-11-22-request-cap-need-design.md). The
// server reads its bits from here -- the central gate and every inline check
// -- and so can anything outside it that has to know the same thing, without
// a copy that drifts.
//
// What it decides is the BITS. Where and in what order the server checks
// them, and how it answers a refusal, stay in the server: several handlers
// check visibility before the bit so a refusal does not reveal that a target
// exists, and that ordering is the handler's.

// Clause is one requirement: any ONE of AnyOf satisfies it. A Need's clauses
// all have to hold -- an AND of ORs -- because a request can need two
// unrelated powers at once: an attached open needs spawn AND exec_control.
type Clause struct {
	AnyOf protocol.Capability
	// Report is what PermissionDenied names when this clause fails. It is not
	// always AnyOf: ListFiles is satisfied by file_read or file_write and has
	// always reported file_read alone.
	Report protocol.Capability
}

// Later is a requirement the request alone cannot decide; the handler decides
// it after a lookup (the forward's direction, the schedule's binding, the
// board's reach of a topic).
type Later struct {
	What string
	// Possible is every bit this requirement can come to need. It is a fixed
	// value per kind and never computed from the request: for board reach that
	// would mean restating agentboard's topic grammar here. Zero means no bit
	// can satisfy it (a reach or grant check that no capability opens).
	Possible protocol.Capability
	// Report is what the server names when it refuses on this requirement with
	// PermissionDenied, and zero when it answers with the kind's own status or
	// with a bit only the lookup decides.
	Report protocol.Capability
}

// NeedClass says what kind of answer a Need is. The zero value is a kind
// nothing here has classified, which TestEveryKindIsClassified refuses.
type NeedClass uint8

const (
	NeedUnclassified NeedClass = iota
	// NeedBits: the request needs the clauses (and the later requirements).
	NeedBits
	// NeedNone: no capability bit; visibility or ownership filters the answer.
	NeedNone
	// NeedIdentity: decided by who is calling (the operator, or a task holding
	// resume over a strict descendant), not by a bit.
	NeedIdentity
	// NeedRetired: a held ordinal with no arm; nothing decodes to it.
	NeedRetired
	// NeedNotRequest: not a kind a client sends (the handshake, a response).
	NeedNotRequest
)

func (c NeedClass) String() string {
	switch c {
	case NeedBits:
		return "bits"
	case NeedNone:
		return "none"
	case NeedIdentity:
		return "identity"
	case NeedRetired:
		return "retired"
	case NeedNotRequest:
		return "not-a-request"
	}
	return "unclassified"
}

// Need is what one request needs.
type Need struct {
	Class NeedClass
	// Clauses are request-determined, in the order the server checks them.
	Clauses []Clause
	Later   []Later
}

// centralCap maps a direction-independent TaskControlKind to the cap the
// server's central gate checks before dispatch. It was server.requiredCap.
// Kinds absent from the map are decided by the request or a lookup (their
// branches in RequestNeed), or need no bit:
//
// OpenFileTransfer / ListFiles are direction-dependent, RegisterPortForward is
// direction-dependent (its gate moved off OpenPortForward, which is now
// unconditionally ForwardLocal); List / ListPortForwards is INFO-scoped
// (visibleToCaller, not a single cap); KillPortForward is direction-dependent AND the
// direction is only known after the registry lookup, so its gate lives inline
// in the dispatch case, after the registry lookup.
//
// ExecRunList is absent because it is INFO-scoped like ListPortForwards, and
// ExecRunKill because its target task is only known after the registry
// lookup, so its gate lives inline in the handler as KillPortForward's does.
//
// GitQuery sits in this map rather than beside the direction-dependent file
// ops because it is read-only by construction — there is no write direction to
// discriminate. It is also the one task-scoped kind that does NOT require
// Running/Detached; see the server's handleGitQuery for why.
//
// OpenInteractive is absent because its bit depends on the request: a zero
// resume_task_id creates a task (spawn), a non-zero one resumes (resume, or
// resume_exact when the exact flag is set).
//
// AttachSession is absent because its cap depends on the requested AttachMode,
// which this map cannot see (AttachModeCap). AwaitIdle is absent because it
// needs NO capability: it reports last_output_at, which `ls` hands to any
// caller that can see the task, so a gate here would only make the
// edge-triggered path cost more authority than polling for the same fact. Its
// one side effect, sink=notify, is a request-determined clause.
var centralCap = map[protocol.TaskControlKind]protocol.Capability{
	protocol.TaskControlKind_Cancel:      protocol.Capability_Cancel,
	protocol.TaskControlKind_SessionMode: protocol.Capability_Cancel,
	// PTY records (pillar A, A1). Reading is exec_view (GD G34); deleting is
	// record_delete. keep and switch need record_persist or record_delete by
	// direction, so their handlers check them. All are scoped in the handler.
	protocol.TaskControlKind_RecordList:   protocol.Capability_ExecView,
	protocol.TaskControlKind_RecordGet:    protocol.Capability_ExecView,
	protocol.TaskControlKind_RecordDelete: protocol.Capability_RecordDelete,
	// Agent transcripts (pillar B, B-D10): transcript_read, scoped in the
	// handler. exec_run on the task only WIDENS a listing to sessions recorded
	// in a shared directory; it is not a requirement.
	protocol.TaskControlKind_TranscriptList: protocol.Capability_TranscriptRead,
	protocol.TaskControlKind_TranscriptGet:  protocol.Capability_TranscriptRead,
	// Schedule management and explicit firing are independent powers.
	// List / show need no bit and are bounded by reach.
	protocol.TaskControlKind_ScheduleCreate: protocol.Capability_ScheduleEdit,
	protocol.TaskControlKind_ScheduleEdit:   protocol.Capability_ScheduleEdit,
	protocol.TaskControlKind_ScheduleDelete: protocol.Capability_ScheduleEdit,
	protocol.TaskControlKind_ScheduleRun:    protocol.Capability_ScheduleRun,
	// Credential tasks (credential-tasks D13, D18): issuing and re-keying are
	// one power; re-key is gated as issue, not as resume.
	protocol.TaskControlKind_CredentialIssue: protocol.Capability_CredentialIssue,
	protocol.TaskControlKind_CredentialRekey: protocol.Capability_CredentialIssue,
	// Server-defined actions (grant conditions spec C13): defining, editing and
	// deleting one. An edit is additionally gated per holder in the handler
	// (C11); action_list is a request-determined clause below.
	protocol.TaskControlKind_ActionPut:    protocol.Capability_ActionAdmin,
	protocol.TaskControlKind_ActionRemove: protocol.Capability_ActionAdmin,
	protocol.TaskControlKind_PruneTasks:      protocol.Capability_Prune,
	protocol.TaskControlKind_Notify:          protocol.Capability_Notify,
	// Emptying another task's progress line: whoever may end the task may
	// empty its status line, which is the weaker act. Target-gated on
	// cancel's scope in handleProgressClear.
	protocol.TaskControlKind_ProgressClear: protocol.Capability_Cancel,
	protocol.TaskControlKind_DialRunner:    protocol.Capability_RunnerAdmin,
	// Which keys may register as runners: enrolling, listing and removing
	// them. Not runner_admin, which is the server's network reach.
	protocol.TaskControlKind_RunnerEnroll:   protocol.Capability_RunnerEnroll,
	protocol.TaskControlKind_RunnerUnenroll: protocol.Capability_RunnerEnroll,
	protocol.TaskControlKind_RunnerKeys:     protocol.Capability_RunnerEnroll,
	// Dial records are the server's network reach made durable, so they take
	// the bit dial_runner takes. Kicking one runner is registry hygiene, the
	// reason runner_enroll exists (spec 2026-11-04 D11).
	protocol.TaskControlKind_RunnerDialAdd:    protocol.Capability_RunnerAdmin,
	protocol.TaskControlKind_RunnerDialList:   protocol.Capability_RunnerAdmin,
	protocol.TaskControlKind_RunnerDialRemove: protocol.Capability_RunnerAdmin,
	protocol.TaskControlKind_RunnerKick:       protocol.Capability_RunnerEnroll,
	protocol.TaskControlKind_BoardTopics:      protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardRead:        protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardPurge:       protocol.Capability_BoardPurge,
	// A task caller is also checked against the board reach in the handler:
	// it may retain only a topic it may publish to.
	protocol.TaskControlKind_BoardRetain: protocol.Capability_BoardPersist,
	// Flush and archive creation are retain's companions: board_persist, and
	// the same publish check for a task caller in the handler.
	protocol.TaskControlKind_BoardFlush:   protocol.Capability_BoardPersist,
	protocol.TaskControlKind_BoardArchive: protocol.Capability_BoardPersist,
	// Listing every archive names topics the caller did not, and reading one
	// on this face includes withdrawn messages: board_topics' and board_read's
	// bit. The agent face's agent_archives / agent_archive_read need none.
	protocol.TaskControlKind_BoardArchives:     protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardArchiveRead:  protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardArchivePurge: protocol.Capability_BoardPurge,
	protocol.TaskControlKind_BoardRetract:      protocol.Capability_BoardPurge,
	// Forgetting a task's contacts removes another task's board state, the
	// same class as withdrawing its message; the handler adds the scope gate.
	protocol.TaskControlKind_BoardContactsClear: protocol.Capability_BoardPurge,
	// Subscribing another task, or making it a member of a topic; the handler
	// adds the scope gate and the reach bounds.
	protocol.TaskControlKind_BoardMembership:  protocol.Capability_BoardMember,
	protocol.TaskControlKind_BoardSubscribers: protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardSend:        protocol.Capability_BoardSend,
	protocol.TaskControlKind_BoardWake:        protocol.Capability_BoardSend,
	protocol.TaskControlKind_GitQuery:         protocol.Capability_FileRead,
	protocol.TaskControlKind_OpenExecRun:      protocol.Capability_ExecRun,
	// OpenForwardTap sits in this map, and NOT inline like KillPortForward's,
	// because it is direction-INDEPENDENT: reading the payload of a -R forward
	// is the same power as reading a -L one, so the bit is known without the
	// registry lookup. The visibility and scope halves still happen in the
	// handler, which is where the target becomes known.
	protocol.TaskControlKind_OpenForwardTap: protocol.Capability_ForwardTap,
	// OpenExecTap: direction-independent like OpenForwardTap — reading an
	// exec's stdin is the same power as reading its stdout — so the bit is
	// known before the registry lookup. Visibility and scope are in the handler.
	protocol.TaskControlKind_OpenExecTap: protocol.Capability_ExecTap,
}

// CentralCap is the bit the server's central gate checks for kind before
// dispatch, and whether it checks one at all.
func CentralCap(kind protocol.TaskControlKind) (protocol.Capability, bool) {
	c, ok := centralCap[kind]
	return c, ok
}

// classOnly are the kinds whose whole answer is their class: no bit at all.
// Each says why it needs none -- an unclassified kind dispatches with whatever
// gate its neighbours happen to have, which is how restore_tasks once shipped
// able to put pruned tasks back with prune revoked.
var classOnly = map[protocol.TaskControlKind]NeedClass{
	// Bounded by task VISIBILITY, not by a bit.
	protocol.TaskControlKind_List:             NeedNone,
	protocol.TaskControlKind_ListConns:        NeedNone,
	protocol.TaskControlKind_ListPortForwards: NeedNone,
	// exec_run_list: gating a fact `ls` already hands out would make the
	// direct path cost more authority than polling for it.
	protocol.TaskControlKind_ExecRunList: NeedNone,
	// The caller's own identity.
	protocol.TaskControlKind_Whoami: NeedNone,
	// trsf_state is bounded by visibility, and its runner branch by the global
	// visibility RANK -- not runner_admin, which also carries the power to make
	// the server dial a new runner and has no business on a read-only
	// diagnostic.
	protocol.TaskControlKind_TrsfState: NeedNone,
	// The agentboard's agent face. Each is keyed to the caller's OWN board
	// state -- its subscriptions (inbox, read, list_retained), its contacts --
	// or, for agent_retract, to its AUTHORSHIP: Board.RetractSeq matches the
	// stored message's sender against the caller's authenticated id, which is
	// why board_retract (the same act without that check) takes board_purge.
	// A board verb that reaches topics the caller need not subscribe to is a
	// board_* kind and does take a bit.
	protocol.TaskControlKind_AgentUnsubscribe:       NeedNone,
	protocol.TaskControlKind_AgentListSubscriptions: NeedNone,
	protocol.TaskControlKind_AgentInbox:             NeedNone,
	protocol.TaskControlKind_AgentInboxAdvance:      NeedNone,
	protocol.TaskControlKind_AgentListRetained:      NeedNone,
	protocol.TaskControlKind_AgentReadSeq:           NeedNone,
	protocol.TaskControlKind_AgentRetract:           NeedNone,
	protocol.TaskControlKind_AgentListContacts:      NeedNone,
	protocol.TaskControlKind_AgentReadPage:          NeedNone,
	// Keyed by a topic or an archive the caller names, refused unless the
	// caller may read the topic: agent_read_page's reasoning, with reach in
	// place of the subscription.
	protocol.TaskControlKind_AgentArchives:    NeedNone,
	protocol.TaskControlKind_AgentArchiveRead: NeedNone,
	// await_idle_list / _kill reveal and end only what the caller armed (the
	// operator: everything), and arming needed no bit either.
	protocol.TaskControlKind_AwaitList: NeedNone,
	protocol.TaskControlKind_AwaitKill: NeedNone,
	// progress_set / lifecycle_set write only the caller's OWN row, resolved
	// from its principal -- whoami's self-introspection reasoning.
	protocol.TaskControlKind_ProgressSet:  NeedNone,
	protocol.TaskControlKind_LifecycleSet: NeedNone,
	// Schedules are listed and shown by reach alone (S-D17).
	protocol.TaskControlKind_ScheduleList: NeedNone,
	protocol.TaskControlKind_ScheduleShow: NeedNone,

	// Retired in A3 (F34): ordinals held, no arm, so a request for either does
	// not decode and is never dispatched. Classified so every ordinal has a
	// class.
	protocol.TaskControlKind_RetiredSubmit:     NeedRetired,
	protocol.TaskControlKind_RetiredGetTaskLog: NeedRetired,

	// The handshake, and a RESPONSE kind that never arrives as a request.
	protocol.TaskControlKind_ClientHello:      NeedNotRequest,
	protocol.TaskControlKind_PermissionDenied: NeedNotRequest,

	// The operator, or a task holding resume over a strict descendant of
	// itself (the server's regrantReaches); SetParent is operator-only.
	protocol.TaskControlKind_SetCaps:    NeedIdentity,
	protocol.TaskControlKind_SetParent:  NeedIdentity,
	protocol.TaskControlKind_TaskRename: NeedIdentity,
}

func same(c protocol.Capability) Clause { return Clause{AnyOf: c, Report: c} }

// The later requirements more than one kind shares.
var (
	scheduleBinding = Later{What: "schedule binding", Possible: UnbindCap, Report: UnbindCap}
	scheduleState   = func(possible protocol.Capability) Later {
		return Later{What: "schedule state", Possible: possible}
	}
	// Board reach is agentboard's (readableFor / publishableFor), and a
	// refusal on it is reported as the bit that most often opens it -- even for
	// a tree.<X> topic that no bit opens (spec §3.4, recorded, not fixed).
	boardReach = func(reported protocol.Capability) Later {
		return Later{What: "board reach", Possible: reported, Report: reported}
	}
)

// RequestNeed is the single answer to "what does this request need". The
// clauses come in the order the server checks them, the central clause first.
// A request whose body is absent is read as the zero body.
func RequestNeed(req *protocol.TaskControlRequest) Need {
	k := req.Header.Kind
	if c, ok := classOnly[k]; ok {
		return Need{Class: c}
	}
	n := Need{Class: NeedBits}
	bit, central := centralCap[k]
	if central {
		n.Clauses = append(n.Clauses, same(bit))
	}
	switch k {
	case protocol.TaskControlKind_AttachSession:
		n.Clauses = append(n.Clauses, same(AttachModeCap(bodyOf(req.Attach()).Mode)))
	case protocol.TaskControlKind_OpenFileTransfer:
		n.Clauses = append(n.Clauses, same(FileTransferCap(bodyOf(req.OpenFileTransfer()).Direction)))
	case protocol.TaskControlKind_ListFiles:
		// Either bit lists a directory; a refusal has always named file_read.
		n.Clauses = append(n.Clauses, Clause{
			AnyOf:  protocol.Capability_FileRead | protocol.Capability_FileWrite,
			Report: protocol.Capability_FileRead,
		})
	case protocol.TaskControlKind_OpenPortForward:
		n.Clauses = append(n.Clauses, same(protocol.Capability_ForwardLocal))
	case protocol.TaskControlKind_ActionList:
		// Who may define actions, and who may run them, may read them (C13).
		n.Clauses = append(n.Clauses, Clause{
			AnyOf:  protocol.Capability_ActionAdmin | protocol.Capability_ExecRun,
			Report: protocol.Capability_ExecRun,
		})
	case protocol.TaskControlKind_RegisterPortForward:
		n.Clauses = append(n.Clauses, same(ForwardCap(bodyOf(req.RegisterPortForward()).Direction)))
	case protocol.TaskControlKind_RecordSwitch:
		rs := bodyOf(req.RecordSwitch())
		n.Clauses = append(n.Clauses, same(RecordSwitchCap(rs.On())))
	case protocol.TaskControlKind_RestoreTasks:
		c := RestoreCap(bodyOf(req.RestoreTasks()).ListOnly)
		if c == 0 {
			return Need{Class: NeedNone}
		}
		n.Clauses = append(n.Clauses, same(c))
	case protocol.TaskControlKind_OpenInteractive:
		oi := bodyOf(req.OpenInteractive())
		n.Clauses = append(n.Clauses, same(ResumeCap(oi.ResumeTaskId, oi.ResumeExact())))
		// An attached open hands the opener the control seat.
		if !oi.Detached() {
			n.Clauses = append(n.Clauses, same(AttachModeCap(protocol.AttachMode_Control)))
		}
		// Refused with RecordNotPermitted, not PermissionDenied.
		if OpenAsksRecord(oi) {
			n.Clauses = append(n.Clauses, Clause{AnyOf: RecordOnCap})
		}
		if oi.ResumeTaskId.Id != ([16]byte{}) {
			n.Later = append(n.Later, scheduleBinding)
		}
	case protocol.TaskControlKind_AwaitIdle:
		return awaitNeed(n, bodyOf(req.AwaitIdle()).Sink)
	case protocol.TaskControlKind_AwaitProgress:
		return awaitNeed(n, bodyOf(req.AwaitProgress()).Sink)
	case protocol.TaskControlKind_SessionMode:
		n.Later = append(n.Later, scheduleBinding)
	case protocol.TaskControlKind_KillPortForward:
		// The bit is ForwardCap of the forward's direction, known after the
		// registry lookup; the refusal names that bit.
		n.Later = append(n.Later, Later{What: "forward direction", Possible: protocol.Capability_ForwardLocal |
			protocol.Capability_ForwardRemote | protocol.Capability_ForwardLink})
	case protocol.TaskControlKind_ExecRunKill:
		// Never PermissionDenied: a missing bit answers NotFound, so a refusal
		// cannot confirm that an exec the caller may not see exists.
		n.Clauses = append(n.Clauses, Clause{AnyOf: protocol.Capability_ExecRun})
	case protocol.TaskControlKind_RecordKeep:
		both := protocol.Capability_RecordPersist | protocol.Capability_RecordDelete
		n.Clauses = append(n.Clauses, Clause{AnyOf: both, Report: protocol.Capability_RecordPersist})
		n.Later = append(n.Later, Later{What: "record retention", Possible: both})
	case protocol.TaskControlKind_ScheduleCreate:
		s := bodyOf(req.ScheduleCreate()).Spec
		n.Clauses = append(n.Clauses, SchedulePower(true, false, SpecBinds(&s))...)
		if SpecAsksRecord(&s) {
			n.Clauses = append(n.Clauses, Clause{AnyOf: RecordOnCap})
		}
	case protocol.TaskControlKind_ScheduleEdit:
		s := bodyOf(req.ScheduleEdit()).Spec
		if SpecAsksRecord(&s) {
			n.Clauses = append(n.Clauses, Clause{AnyOf: RecordOnCap})
		}
		n.Later = append(n.Later, scheduleState(protocol.Capability_Spawn|protocol.Capability_Resume))
	case protocol.TaskControlKind_ScheduleRun:
		n.Later = append(n.Later, scheduleState(protocol.Capability_Spawn|protocol.Capability_Resume|protocol.Capability_ResumeExact))
	case protocol.TaskControlKind_BoardRetain, protocol.TaskControlKind_BoardFlush, protocol.TaskControlKind_BoardArchive:
		// A task caller may persist only a topic it may publish to; refused
		// with NotPublishable, and no bit opens a topic that reach closes.
		n.Later = append(n.Later, Later{What: "board reach"})
	case protocol.TaskControlKind_BoardMembership:
		n.Later = append(n.Later, Later{What: "grant reach"})
	case protocol.TaskControlKind_AgentSend:
		n.Later = append(n.Later, boardReach(protocol.Capability_BoardPeer))
	case protocol.TaskControlKind_AgentSubscribe, protocol.TaskControlKind_AgentWait:
		n.Later = append(n.Later, boardReach(protocol.Capability_BoardObserve))
	default:
		if !central {
			return Need{}
		}
	}
	return n
}

// KindNeed is the need of a kind whose need the request body does not decide,
// for a handler that holds only the decoded body. It panics on a kind whose
// body decides (one variants enumerates more than once): reading the zero body
// there would answer one branch for all of them.
func KindNeed(kind protocol.TaskControlKind) Need {
	if len(variants(kind)) > 1 {
		panic("authority.KindNeed: the body decides " + kind.String() + "'s need; use RequestNeed")
	}
	return RequestNeed(&protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: kind}})
}

// awaitNeed is AwaitIdle's and AwaitProgress's: a reply needs nothing, a
// notification needs notify, a board publish needs the board's reach.
func awaitNeed(n Need, sink protocol.AwaitSink) Need {
	switch sink {
	case protocol.AwaitSink_Notify:
		n.Clauses = append(n.Clauses, same(protocol.Capability_Notify))
	case protocol.AwaitSink_Board:
		n.Later = append(n.Later, boardReach(protocol.Capability_BoardPeer))
	default:
		return Need{Class: NeedNone}
	}
	return n
}

// OpenAsksRecord reports whether an open says "record this session" (the
// --record on of A-D22), which needs RecordOnCap. Off and saying nothing need
// nothing.
func OpenAsksRecord(oi *protocol.OpenInteractiveRequest) bool {
	ext := oi.Ext()
	return oi.ExtPresent() && ext != nil && ext.RecordSet() && ext.Record()
}

// SpecBinds reports whether a schedule spec binds a task (--task).
func SpecBinds(s *protocol.ScheduleSpec) bool { return s.TaskPresent() && s.TaskId() != nil }

// SpecAsksRecord reports whether a schedule spec says "record its sessions",
// which needs RecordOnCap as an open saying it does.
func SpecAsksRecord(s *protocol.ScheduleSpec) bool { return s.RecordSet() && s.Record() }

// bodyOf reads an absent body as the zero one.
func bodyOf[T any](p *T) *T {
	if p == nil {
		return new(T)
	}
	return p
}

// ScopeBit is which bit's scope binds this request's target, given what the
// caller holds -- for the kinds whose REQUEST decides it, and ok=false for every
// other kind rather than a guess. It differs from the clauses where the server
// scopes by something other than the bit it checked: ListFiles is satisfied by
// either file bit and scoped by the one held (file_read first); an attach is
// satisfied by AttachModeCap(mode) and scoped by AttachModeScopeCap(mode); a
// restore's listing needs no bit and is still scoped by prune. A SET of bits is
// returned where the server walks several bits' scopes (a resume: each resume
// bit held; RecordKeep: persist and delete).
//
// Not answered here: a central kind that names a target is scoped by its
// CentralCap; an await is bounded by visibility, not a bit; a schedule that
// binds a task passes two scopes (schedule_edit's reach and resume's); a
// forward or exec kill is scoped through a bit its registry lookup decides; a
// fresh spawn names no target.
func ScopeBit(req *protocol.TaskControlRequest, held protocol.Capability) (protocol.Capability, bool) {
	switch req.Header.Kind {
	case protocol.TaskControlKind_ListFiles:
		if held&protocol.Capability_FileRead != 0 {
			return protocol.Capability_FileRead, true
		}
		return protocol.Capability_FileWrite, true
	case protocol.TaskControlKind_AttachSession:
		return AttachModeScopeCap(bodyOf(req.Attach()).Mode), true
	case protocol.TaskControlKind_OpenFileTransfer:
		return FileTransferCap(bodyOf(req.OpenFileTransfer()).Direction), true
	case protocol.TaskControlKind_OpenPortForward:
		return ForwardCap(protocol.PortForwardDirection_Local), true
	case protocol.TaskControlKind_RegisterPortForward:
		return ForwardCap(bodyOf(req.RegisterPortForward()).Direction), true
	case protocol.TaskControlKind_RecordSwitch:
		return RecordSwitchCap(bodyOf(req.RecordSwitch()).On()), true
	case protocol.TaskControlKind_RestoreTasks:
		return RestoreCap(0), true
	case protocol.TaskControlKind_RecordKeep:
		return protocol.Capability_RecordPersist | protocol.Capability_RecordDelete, true
	case protocol.TaskControlKind_OpenInteractive:
		oi := bodyOf(req.OpenInteractive())
		if oi.ResumeTaskId.Id == ([16]byte{}) {
			return 0, false
		}
		return ResumeCap(oi.ResumeTaskId, oi.ResumeExact()), true
	}
	return 0, false
}

// Possible is, per KIND, every bit some request of that kind could need: the
// union over every request-determined branch and every later requirement. It
// is what a consumer with no request in hand (a tool list) reads. A zero-
// bodied request is not a substitute -- it selects one branch: a zero
// FileTransferDirection is Push, so it would answer file_write alone and hide
// `file pull` from a file_read holder.
//
// It is computed by running RequestNeed over variants(k), so a branch cannot
// be added to RequestNeed and missed here as long as the field it reads is
// enumerated by variants.
func Possible(kind protocol.TaskControlKind) protocol.Capability {
	var out protocol.Capability
	for _, r := range variants(kind) {
		n := RequestNeed(r)
		for _, c := range n.Clauses {
			out |= c.AnyOf
		}
		for _, l := range n.Later {
			out |= l.Possible
		}
	}
	return out
}

// MaySatisfy reports whether SOME request of kind could pass with the bits in
// held: a request whose every clause has a bit in held. A later requirement
// counts as satisfiable (the lookup it waits on may answer either way), and a
// kind that needs no bit, or is decided by identity, always may. It is the tool
// list's rule, so every imprecision in it shows too much rather than hiding a
// tool the caller could use -- a model does not call what it was not shown.
func MaySatisfy(kind protocol.TaskControlKind, held protocol.Capability) bool {
	for _, r := range variants(kind) {
		n := RequestNeed(r)
		if n.Class != NeedBits {
			return true
		}
		ok := true
		for _, c := range n.Clauses {
			if c.AnyOf&held == 0 {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// variants are requests of kind k covering every value of every field
// RequestNeed reads for it: the whole u8 domain of each enum field, so a new
// enum value is covered without an edit here, and both values of each flag.
func variants(k protocol.TaskControlKind) []*protocol.TaskControlRequest {
	mk := func(set func(*protocol.TaskControlRequest)) *protocol.TaskControlRequest {
		r := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: k}}
		set(r)
		return r
	}
	var out []*protocol.TaskControlRequest
	switch k {
	case protocol.TaskControlKind_AttachSession:
		for v := 0; v < 256; v++ {
			out = append(out, mk(func(r *protocol.TaskControlRequest) {
				r.SetAttach(protocol.AttachSessionRequest{Mode: protocol.AttachMode(v)})
			}))
		}
	case protocol.TaskControlKind_OpenFileTransfer:
		for v := 0; v < 256; v++ {
			out = append(out, mk(func(r *protocol.TaskControlRequest) {
				r.SetOpenFileTransfer(protocol.OpenFileTransferRequest{Direction: protocol.FileTransferDirection(v)})
			}))
		}
	case protocol.TaskControlKind_RegisterPortForward:
		for v := 0; v < 256; v++ {
			out = append(out, mk(func(r *protocol.TaskControlRequest) {
				r.SetRegisterPortForward(protocol.RegisterPortForwardRequest{Direction: protocol.PortForwardDirection(v)})
			}))
		}
	case protocol.TaskControlKind_RecordSwitch:
		for _, on := range []bool{false, true} {
			out = append(out, mk(func(r *protocol.TaskControlRequest) {
				var rs protocol.RecordSwitchRequest
				rs.SetOn(on)
				r.SetRecordSwitch(rs)
			}))
		}
	case protocol.TaskControlKind_RestoreTasks:
		for v := 0; v < 256; v++ {
			out = append(out, mk(func(r *protocol.TaskControlRequest) {
				r.SetRestoreTasks(protocol.RestoreTasksRequest{ListOnly: uint8(v)})
			}))
		}
	case protocol.TaskControlKind_AwaitIdle:
		for v := 0; v < 256; v++ {
			out = append(out, mk(func(r *protocol.TaskControlRequest) {
				r.SetAwaitIdle(protocol.AwaitIdleRequest{Sink: protocol.AwaitSink(v)})
			}))
		}
	case protocol.TaskControlKind_AwaitProgress:
		for v := 0; v < 256; v++ {
			out = append(out, mk(func(r *protocol.TaskControlRequest) {
				r.SetAwaitProgress(protocol.AwaitProgressRequest{Sink: protocol.AwaitSink(v)})
			}))
		}
	case protocol.TaskControlKind_OpenInteractive:
		for _, id := range []protocol.TaskID{{}, {Id: [16]byte{1}}} {
			for _, exact := range []bool{false, true} {
				for _, detached := range []bool{false, true} {
					for _, record := range []bool{false, true} {
						out = append(out, mk(func(r *protocol.TaskControlRequest) {
							var oi protocol.OpenInteractiveRequest
							oi.ResumeTaskId = id
							oi.SetResumeExact(exact)
							oi.SetDetached(detached)
							if record {
								var ext protocol.OpenInteractiveExt
								ext.SetRecordSet(true)
								ext.SetRecord(true)
								oi.SetExtPresent(true)
								oi.SetExt(ext)
							}
							r.SetOpenInteractive(oi)
						}))
					}
				}
			}
		}
	case protocol.TaskControlKind_ScheduleCreate, protocol.TaskControlKind_ScheduleEdit:
		for _, binds := range []bool{false, true} {
			for _, record := range []bool{false, true} {
				var s protocol.ScheduleSpec
				if binds {
					s.SetTaskPresent(true)
					s.SetTaskId(protocol.TaskID{Id: [16]byte{1}})
				}
				if record {
					s.SetRecordSet(true)
					s.SetRecord(true)
				}
				out = append(out, mk(func(r *protocol.TaskControlRequest) {
					if k == protocol.TaskControlKind_ScheduleCreate {
						r.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: s})
					} else {
						r.SetScheduleEdit(protocol.ScheduleEditRequest{Spec: s})
					}
				}))
			}
		}
	default:
		out = append(out, mk(func(*protocol.TaskControlRequest) {}))
	}
	return out
}
