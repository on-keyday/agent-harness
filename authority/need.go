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
	// NeedRetired: answered with an error or nothing, whoever asks.
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
// (visibleToCaller, not a single cap); GetTaskLog is retired (A3) and answers
// "no log" to everyone; KillPortForward is direction-dependent AND the
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
// resume_exact when the exact flag is set). Submit is absent because it is
// retired (A3): it is answered with an error and creates nothing.
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
var classOnly = map[protocol.TaskControlKind]NeedClass{
	protocol.TaskControlKind_List:                   NeedNone,
	protocol.TaskControlKind_ListConns:              NeedNone,
	protocol.TaskControlKind_Whoami:                 NeedNone,
	protocol.TaskControlKind_ListPortForwards:       NeedNone,
	protocol.TaskControlKind_ExecRunList:            NeedNone,
	protocol.TaskControlKind_TrsfState:              NeedNone, // a global visibility rank, not a bit
	protocol.TaskControlKind_AgentUnsubscribe:       NeedNone,
	protocol.TaskControlKind_AgentListSubscriptions: NeedNone,
	protocol.TaskControlKind_AgentInbox:             NeedNone,
	protocol.TaskControlKind_AgentInboxAdvance:      NeedNone,
	protocol.TaskControlKind_AgentListRetained:      NeedNone,
	protocol.TaskControlKind_AgentReadSeq:           NeedNone,
	protocol.TaskControlKind_AgentRetract:           NeedNone,
	protocol.TaskControlKind_AwaitList:              NeedNone,
	protocol.TaskControlKind_AwaitKill:              NeedNone,
	protocol.TaskControlKind_AgentListContacts:      NeedNone,
	protocol.TaskControlKind_ProgressSet:            NeedNone,
	protocol.TaskControlKind_LifecycleSet:           NeedNone,
	protocol.TaskControlKind_AgentReadPage:          NeedNone,
	protocol.TaskControlKind_AgentArchives:          NeedNone,
	protocol.TaskControlKind_AgentArchiveRead:       NeedNone,
	protocol.TaskControlKind_ScheduleList:           NeedNone,
	protocol.TaskControlKind_ScheduleShow:           NeedNone,

	protocol.TaskControlKind_Submit:     NeedRetired,
	protocol.TaskControlKind_GetTaskLog: NeedRetired,

	protocol.TaskControlKind_ClientHello:      NeedNotRequest,
	protocol.TaskControlKind_PermissionDenied: NeedNotRequest,

	// The operator, or a task holding resume over a strict descendant of
	// itself (the server's regrantReaches); SetParent is operator-only.
	protocol.TaskControlKind_SetCaps:    NeedIdentity,
	protocol.TaskControlKind_SetParent:  NeedIdentity,
	protocol.TaskControlKind_TaskRename: NeedIdentity,
}

// requestKinds are the kinds outside the central map whose bits the request
// or a lookup decides. Filled in by kindBranches.
var requestKinds = map[protocol.TaskControlKind]bool{
	protocol.TaskControlKind_AttachSession:       true,
	protocol.TaskControlKind_OpenFileTransfer:    true,
	protocol.TaskControlKind_ListFiles:           true,
	protocol.TaskControlKind_OpenPortForward:     true,
	protocol.TaskControlKind_RegisterPortForward: true,
	protocol.TaskControlKind_RecordSwitch:        true,
	protocol.TaskControlKind_RestoreTasks:        true,
	protocol.TaskControlKind_OpenInteractive:     true,
	protocol.TaskControlKind_AwaitIdle:           true,
	protocol.TaskControlKind_AwaitProgress:       true,
	protocol.TaskControlKind_KillPortForward:     true,
	protocol.TaskControlKind_ExecRunKill:         true,
	protocol.TaskControlKind_RecordKeep:          true,
	protocol.TaskControlKind_AgentSend:           true,
	protocol.TaskControlKind_AgentSubscribe:      true,
	protocol.TaskControlKind_AgentWait:           true,
}

// RequestNeed is the single answer to "what does this request need". The
// clauses come in the order the server checks them, the central clause first.
func RequestNeed(req *protocol.TaskControlRequest) Need {
	k := req.Header.Kind
	if c, ok := classOnly[k]; ok {
		return Need{Class: c}
	}
	n := Need{Class: NeedBits}
	bit, central := centralCap[k]
	if central {
		n.Clauses = append(n.Clauses, Clause{AnyOf: bit, Report: bit})
	}
	if !central && !requestKinds[k] {
		return Need{}
	}
	return n
}
