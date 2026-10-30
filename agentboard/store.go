package agentboard

import (
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Retention is how long a retained topic is kept. The period counts from the
// topic's last publish, or from SetAt when nothing was published since — the
// TTL rule every topic already has, with the topic's own period instead of
// the board's. See docs/superpowers/specs/2026-10-30-agentboard-retention-design.md.
type Retention struct {
	TTL     time.Duration // ignored when Forever
	Forever bool
	SetBy   protocol.TaskID // the task that set it; zero = the operator
	SetAt   time.Time
}

// StoredTopic is one retained topic as the store holds it at load time.
type StoredTopic struct {
	Name      string
	Retention Retention
	Ring      []RetainedMessage // the newest ringN live messages, ascending seq
	Retracted []RetainedMessage // the newest ringN withdrawn messages, ascending seq
	Shown     map[protocol.TaskID]uint64
	Members   []protocol.TaskID // the tasks that joined it; the last record per task wins
	// MaxSeq is the highest seq the log has ever held — messages, marks, and
	// the floor a purge rewrite records — so a purge never lowers the seq a
	// restart seeds from.
	MaxSeq uint64
	// LastPublishedAt is the newest publish the log has seen, withdrawn
	// messages included, or the time of a whole-topic purge: what the
	// topic's period counts from after a restart.
	LastPublishedAt time.Time
}

// Store persists retained topics. The board mirrors into it only the topics
// that carry a Retention; every other topic stays in memory alone.
type Store interface {
	// Load returns every retained topic. A record that cannot be read is
	// skipped and logged; it never discards the rest of its log.
	Load(ringN int) ([]StoredTopic, error)
	// SetRetention records topic's retention, creating its log if absent.
	// Append, Retract and SetShown write nothing for a topic without one, so
	// a mirror racing a Drop cannot leave a log behind.
	SetRetention(topic string, r Retention) error
	// Drop removes topic's log and its retention.
	Drop(topic string) error
	// Truncate empties topic's log and keeps its retention.
	Truncate(topic string) error
	Append(topic string, m RetainedMessage) error
	// Retract records that m — carrying RetractedAt / RetractedBy /
	// RetractedByTask — was withdrawn. The bytes stay.
	Retract(topic string, m RetainedMessage) error
	// Remove rewrites topic's log without seq, so its bytes are gone. An
	// Append of seq that arrives afterwards — a purge racing the publish's
	// mirror — is dropped.
	Remove(topic string, seq uint64) (bool, error)
	SetShown(topic string, task protocol.TaskID, seq uint64) error
	// SetMember records that task joined (on) or left topic. Truncate keeps
	// the memberships: a purge empties the content, not the participants.
	SetMember(topic string, task protocol.TaskID, on bool) error
	// Page returns up to limit messages, live and withdrawn, with seq below
	// beforeSeq (beforeSeq 0 = from the newest), newest first.
	Page(topic string, beforeSeq uint64, limit int) ([]RetainedMessage, error)
	// Bytes is the size of topic's log, buffered records included.
	Bytes(topic string) int64
	// Close flushes and closes every log.
	Close() error
}
