package server

import (
	"errors"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunConditions are the conditions of a background run (pillar A spec A-D7),
// as the task stores them. Rows/Cols are the PTY size the run is opened at
// (0x0 = no size, as the opener sent it).
type RunConditions struct {
	Deadline, Quiet, Blocked, QueueTimeout time.Duration
	TurnEnd                                protocol.TurnEndSignal
	Rows, Cols                             uint16
}

// RunSpec is what a create or a resume says about the run it starts: its run
// mode and, for background, its conditions. The zero value is a foreground
// run, today's behaviour.
type RunSpec struct {
	Mode protocol.RunMode
	Cond RunConditions
}

const (
	defaultQuiet   = 5 * time.Minute
	defaultBlocked = 5 * time.Minute
)

// WithDefaults fills what the opener left at zero. The deadline has no
// default (G41) and the queue timeout's zero means "none" (G29). The stream
// kind has only the lifecycle leg (F19), so its "any" is lifecycle.
func (c RunConditions) WithDefaults(kind protocol.TaskKind) RunConditions {
	if c.Quiet == 0 {
		c.Quiet = defaultQuiet
	}
	if c.Blocked == 0 {
		c.Blocked = defaultBlocked
	}
	if c.TurnEnd == protocol.TurnEndSignal_Unset {
		c.TurnEnd = protocol.TurnEndSignal_Any
	}
	if kind == protocol.TaskKind_Stream && c.TurnEnd == protocol.TurnEndSignal_Any {
		c.TurnEnd = protocol.TurnEndSignal_Lifecycle
	}
	return c
}

// ConditionsFromWire converts the request block; nil is the zero value.
// Rows/Cols are not on the block: the opener's initial size is.
func ConditionsFromWire(b *protocol.BackgroundConditions) RunConditions {
	if b == nil {
		return RunConditions{}
	}
	return RunConditions{
		Deadline:     time.Duration(b.DeadlineMs) * time.Millisecond,
		Quiet:        time.Duration(b.QuietMs) * time.Millisecond,
		Blocked:      time.Duration(b.BlockedMs) * time.Millisecond,
		QueueTimeout: time.Duration(b.QueueTimeoutMs) * time.Millisecond,
		TurnEnd:      b.TurnEnd,
	}
}

// Wire is the inverse of ConditionsFromWire, for listings.
func (c RunConditions) Wire() protocol.BackgroundConditions {
	return protocol.BackgroundConditions{
		DeadlineMs:     uint64(c.Deadline / time.Millisecond),
		QuietMs:        uint64(c.Quiet / time.Millisecond),
		BlockedMs:      uint64(c.Blocked / time.Millisecond),
		QueueTimeoutMs: uint64(c.QueueTimeout / time.Millisecond),
		TurnEnd:        c.TurnEnd,
	}
}

func (c RunConditions) toWAL(ev *WALEvent) {
	ev.TurnEnd = uint8(c.TurnEnd)
	ev.DeadlineMs = c.Deadline.Milliseconds()
	ev.QuietMs = c.Quiet.Milliseconds()
	ev.BlockedMs = c.Blocked.Milliseconds()
	ev.QueueTimeoutMs = c.QueueTimeout.Milliseconds()
	ev.Rows, ev.Cols = c.Rows, c.Cols
}

func conditionsFromWAL(ev WALEvent) RunConditions {
	return RunConditions{
		TurnEnd:      protocol.TurnEndSignal(ev.TurnEnd),
		Deadline:     time.Duration(ev.DeadlineMs) * time.Millisecond,
		Quiet:        time.Duration(ev.QuietMs) * time.Millisecond,
		Blocked:      time.Duration(ev.BlockedMs) * time.Millisecond,
		QueueTimeout: time.Duration(ev.QueueTimeoutMs) * time.Millisecond,
		Rows:         ev.Rows,
		Cols:         ev.Cols,
	}
}

// Errors SwitchRunMode answers (A-D19).
var (
	ErrSwitchQueued     = errors.New("run mode switch: task is queued")
	ErrSwitchNotActive  = errors.New("run mode switch: task is not running")
	ErrSwitchNotSession = errors.New("run mode switch: not a session task")
	ErrSwitchNotFound   = errors.New("run mode switch: no such task")
)

// DeadlineHit is one background deadline that has passed (A-D13).
type DeadlineHit struct {
	ID         string
	Reason     protocol.EndReason
	SwitchedAt int64 // the run's ModeSwitchedAt when the deadline was read
}

// remainingMs is the time left to an absolute deadline, on the server clock,
// for listings and events (0 when there is none or it has passed).
func remainingMs(at int64) uint64 {
	if at == 0 {
		return 0
	}
	if d := time.Until(time.Unix(0, at)); d > 0 {
		return uint64(d / time.Millisecond)
	}
	return 0
}
