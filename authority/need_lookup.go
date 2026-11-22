package authority

import (
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The functions here decide a bit from ONE value: a request field, or a value
// the server has just looked up (a forward's direction, a record's retention,
// a schedule's state). The lookup and the order of the check stay in the
// server; which bit the value means is decided here, once.

// ResumeCap is the capability set that satisfies an interactive open. Creating
// a task (zero resumeID) is spawn alone. Resuming is the resume rank, which
// spawn does NOT imply: resume ⊇ resume_exact, checked as any-of like
// AttachModeCap, so a holder of resume may also resume exactly.
func ResumeCap(resumeID protocol.TaskID, exact bool) protocol.Capability {
	switch {
	case resumeID.Id == ([16]byte{}):
		return protocol.Capability_Spawn
	case exact:
		return protocol.Capability_Resume | protocol.Capability_ResumeExact
	default:
		return protocol.Capability_Resume
	}
}

// ForwardCap is the bit a port forward of direction d needs: -L local, -R
// remote, --ip link. An unknown direction is local.
func ForwardCap(d protocol.PortForwardDirection) protocol.Capability {
	switch d {
	case protocol.PortForwardDirection_Remote:
		return protocol.Capability_ForwardRemote
	case protocol.PortForwardDirection_Link:
		return protocol.Capability_ForwardLink
	}
	return protocol.Capability_ForwardLocal
}

// FileTransferCap is the bit a file transfer of direction d needs: reading for
// a pull, writing for everything else (push, delete, mkdir, and any direction
// not known here -- the stronger answer).
func FileTransferCap(d protocol.FileTransferDirection) protocol.Capability {
	switch d {
	case protocol.FileTransferDirection_Pull, protocol.FileTransferDirection_DirPull:
		return protocol.Capability_FileRead
	}
	return protocol.Capability_FileWrite
}

// RecordSwitchCap is the bit switching a session's recording needs: turning it
// on keeps something (record_persist), turning it off deletes sooner
// (record_delete).
func RecordSwitchCap(on bool) protocol.Capability {
	if on {
		return protocol.Capability_RecordPersist
	}
	return protocol.Capability_RecordDelete
}

// RecordKeepCap is the bit changing a record's retention needs, given what it
// is now: a later expiry keeps (record_persist), an earlier one deletes sooner
// (record_delete). Leaving forever is always earlier. A ttl of 0 that is not
// forever expires at the run's end.
func RecordKeepCap(forever bool, ttl time.Duration, curForever bool, curTTL time.Duration) protocol.Capability {
	if !forever && (curForever || ttl < curTTL) {
		return protocol.Capability_RecordDelete
	}
	return protocol.Capability_RecordPersist
}

// RecordOnCap is the bit asking for a session to be recorded needs, wherever
// it is asked: an open with --record, a schedule that records, `record on`.
// Keeping takes the server's disk and pushes older records out under the cap.
const RecordOnCap = protocol.Capability_RecordPersist

// UnbindCap is the bit that unbinding a task from its schedule needs (S-D15):
// a foreground resume or a mode switch of a bound task.
const UnbindCap = protocol.Capability_ScheduleEdit

// SchedulePower is S-D17a for schedule new and edit: besides the schedule bit,
// the power a firing will exercise. Spawn when the schedule is to create its
// task -- a new schedule or a pending_create one -- unless this call binds a
// task, which means nothing will be created. Resume when a task is bound, or
// this call binds one. resume_exact is NOT enough: binding a task, or editing a
// schedule bound to one, chooses the prompt a firing passes.
func SchedulePower(pendingOrNew, bound, binds bool) []Clause {
	var out []Clause
	if pendingOrNew && !binds {
		out = append(out, Clause{AnyOf: protocol.Capability_Spawn, Report: protocol.Capability_Spawn})
	}
	if bound || binds {
		out = append(out, Clause{AnyOf: protocol.Capability_Resume, Report: protocol.Capability_Resume})
	}
	return out
}

// ScheduleRunPower is S-D17a for `schedule run`: the firer picks timing only,
// and the prompt was set by someone who held full resume at bind or edit, so a
// bound schedule takes resume OR resume_exact. A pending_create one needs
// spawn; an unbound one needs nothing (its run records skipped_no_task).
func ScheduleRunPower(state protocol.ScheduleTaskState) []Clause {
	switch state {
	case protocol.ScheduleTaskState_PendingCreate:
		return []Clause{{AnyOf: protocol.Capability_Spawn, Report: protocol.Capability_Spawn}}
	case protocol.ScheduleTaskState_Bound:
		c := ResumeCap(protocol.TaskID{Id: [16]byte{1}}, true)
		return []Clause{{AnyOf: c, Report: c}}
	}
	return nil
}
