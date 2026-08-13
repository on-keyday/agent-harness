package server

import (
	"encoding/hex"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Scope is the server-side form of protocol.TaskScope: which tasks a task's
// capabilities may be pointed at. Capability says what verbs; Scope says what
// targets. The effective set is
//
//	{self} ∪ baseSet(Base) ∪ IDs
//
// so ScopeBase_None is not "nothing" — a task can always reach its own log,
// worktree and session.
//
// IDs are task-id hex, normalised to lower case, sorted and deduped, and
// exactly taskIDHexLen characters. Anything else is dropped on ingest rather
// than carried as a target nothing can ever match.
type Scope struct {
	Base protocol.ScopeBase
	IDs  []string
}

// taskIDHexLen is the hex width of a protocol.TaskID (16 bytes).
const taskIDHexLen = 32

// defaultScope is what a task gets when no scope is requested: self plus
// descendants, which is the visibility rule the server applied before scopes
// existed. It is also the zero value, deliberately — see ScopeBase in the
// schema.
func defaultScope() Scope { return Scope{Base: protocol.ScopeBase_Subtree} }

// scopeBaseRank orders bases by permissiveness. It is NOT the numeric value:
// subtree is 0 so that a zero reads as the default, which puts the numbers out
// of order. An unrecognised base — a byte from a peer built against a newer
// schema — ranks as none, so an unknown widening fails closed.
func scopeBaseRank(b protocol.ScopeBase) int {
	switch b {
	case protocol.ScopeBase_Subtree:
		return 1
	case protocol.ScopeBase_Global:
		return 2
	default: // ScopeBase_None and anything unrecognised
		return 0
	}
}

// minScopeBase returns the less permissive of the two. Ties keep a, so an
// unrecognised base is preserved rather than being rewritten to none — the
// rank already makes it behave as none, and keeping the byte means `ls` shows
// the operator what the task actually asked for.
func minScopeBase(a, b protocol.ScopeBase) protocol.ScopeBase {
	if scopeBaseRank(b) < scopeBaseRank(a) {
		return b
	}
	return a
}

// normalizeScopeIDs lower-cases, validates, sorts and dedupes id hexes.
func normalizeScopeIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if len(id) != taskIDHexLen {
			continue
		}
		if _, err := hex.DecodeString(id); err != nil {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// scopeFromWire converts a decoded TaskScope into the server-side form.
func scopeFromWire(w protocol.TaskScope) Scope {
	ids := make([]string, 0, len(w.Ids))
	for _, id := range w.Ids {
		ids = append(ids, hex.EncodeToString(id.Id[:]))
	}
	return Scope{Base: w.Base, IDs: normalizeScopeIDs(ids)}
}

// toWire converts back for TaskInfo / WhoAmIResponse. Ids that are not valid
// task-id hex are skipped: an unencodable target is a client bug, not a reason
// to fail the whole response.
func (s Scope) toWire() protocol.TaskScope {
	out := protocol.TaskScope{Base: s.Base}
	for _, h := range s.IDs {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != taskIDHexLen/2 {
			continue
		}
		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		out.Ids = append(out.Ids, tid)
	}
	out.IdsLen = uint16(len(out.Ids))
	return out
}

// String renders the scope in the same grammar the --scope flag accepts, so
// what `ls` prints can be pasted back into a spawn.
func (s Scope) String() string {
	base := s.Base.String()
	if len(s.IDs) == 0 {
		return base
	}
	ids := "ids:" + strings.Join(s.IDs, ",")
	if s.Base == protocol.ScopeBase_None {
		// none is the implied base of a bare id list; writing it would be noise.
		return ids
	}
	return base + "+" + ids
}
