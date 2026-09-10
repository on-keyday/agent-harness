package server

import (
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// RunnerEntry holds the current state of a connected runner.
//
// Read methods (Get, List, Candidates) return value snapshots; callers
// may freely read the returned values. All mutations go through the
// Add / Remove / BindTask / UnbindTask / SetLastSeen methods.
//
// Conn is set by the server when registering and is the path through which
// sendAssign reaches the runner. The value-snapshot semantics still hold
// (the field is a copy of an interface value). Conn may be nil if the entry
// was constructed without an active connection (e.g. in tests).
//
// ActiveTasks is a set of task IDs (hex strings) currently bound to this
// runner. len(ActiveTasks) is the current load; capacity is MaxTasks.
type RunnerEntry struct {
	// ID is the CONNECTION this entry describes, and it is the registry's key.
	//
	// A typed ConnectionID rather than its String(), which is what this field
	// held while its own comment read `// = objproto.ConnectionID.String()`.
	// Beside it sits Identity, a protocol.RunnerID — a different thing whose
	// hex is also a string — and the two were interchangeable to the compiler.
	// readopt.go duly passed an identity hex to BindTask, which keys on THIS,
	// so the lookup missed, the bool was discarded, and a re-adopted task was
	// bound to nothing: invisible to failAndRevokeTasksOf, its row stuck
	// non-terminal for good. TaskEntry.AssignedTo was typed for this exact
	// confusion one change earlier; the registry is where it came back.
	//
	// Typed, an identity cannot be passed here at all — there is no conversion
	// from a hex string to a ConnectionID that does not go through a parse
	// that would fail.
	ID objproto.ConnectionID
	// Identity is the runner PROCESS behind this connection, from
	// RunnerHello.runner_id. Unlike ID it survives that process's reconnects,
	// which is the whole reason it exists: a credential keyed on it then
	// outlives a connection. Zero for a runner that predates the field.
	Identity protocol.RunnerID
	Hostname string // from RunnerHello.hostname
	// GOOS is runtime.GOOS on the runner's host, from RunnerHello.goos. Empty
	// for a runner that predates the field — reported as "unknown" rather than
	// guessed, because guessing it is what this exists to stop.
	GOOS         string
	AllowedRoots []string // POSIX '/'-paths, path.Clean'd at Hello receipt (wire-format)
	MaxTasks     int      // from RunnerHello.max_tasks (>=1)
	AgentBin     string   // from RunnerHello.agent_bin (basename of --claude-bin)
	// AgentProfiles is the ordered list of agent profile names advertised in
	// RunnerHello.AgentProfiles. The first entry is this runner's default
	// (see DefaultProfile). Empty for legacy runners that don't advertise any
	// profiles — HasProfile/DefaultProfile fall back to AgentBin in that case.
	AgentProfiles  []string
	SkillsInjected bool                // from RunnerHello.skills_injected
	ActiveTasks    map[string]struct{} // task_id (hex) set; len() = current load
	ConnectedAt    time.Time
	LastSeen       time.Time
	Conn           ConnHandle // set by server.go on registration; nil in zero-value / test stubs

	// Via, when non-nil, is the proxy_runner this runner was registered
	// through via Phase C (--via). nil for Phase A direct and reverse-dial
	// (runner.Connect) registrations. Walking Via.Via.Via... terminates at an
	// entry whose Via is nil (= a hop reachable from server without any proxy).
	Via *RunnerEntry

	// ViaDialAddr = protocol.RunnerIDToConnID(target) captured at Phase C
	// HandleWithVia time. Only Transport + Addr are load-bearing — the ID
	// portion happens to carry admin's UniqueNumber but no consumer reads it.
	// Zero for Phase A direct + reverse-dial registrations.
	//
	// For chained relay setup, this is the addr each upstream hop uses for its
	// SetProxy.allocate when forwarding traffic to this runner.
	ViaDialAddr objproto.ConnectionID
}

// Status derives the wire-visible status from connection + slot occupancy.
// Offline = no Conn; Busy = at capacity; Idle = capacity remains.
func (e *RunnerEntry) Status() protocol.RunnerStatus {
	if e.Conn == nil {
		return protocol.RunnerStatus_Offline
	}
	if len(e.ActiveTasks) >= e.MaxTasks {
		return protocol.RunnerStatus_Busy
	}
	return protocol.RunnerStatus_Idle
}

// HasProfile reports whether name is among the profiles this runner
// advertised at Hello time. Legacy runners that advertised no profiles at
// all are treated as advertising exactly one, implicit profile: AgentBin.
// Matching is EqualAgentProfileName, so a task recorded under a Windows
// spelling ("claude.exe") still matches a runner now advertising "claude".
func (e RunnerEntry) HasProfile(name string) bool {
	if len(e.AgentProfiles) == 0 {
		return protocol.EqualAgentProfileName(name, e.AgentBin)
	}
	for _, p := range e.AgentProfiles {
		if protocol.EqualAgentProfileName(p, name) {
			return true
		}
	}
	return false
}

// DefaultProfile returns the profile to use when a submit/resume request
// doesn't specify one explicitly: the first advertised profile, or AgentBin
// as the legacy fallback when the runner advertised none.
func (e RunnerEntry) DefaultProfile() string {
	if len(e.AgentProfiles) > 0 {
		return e.AgentProfiles[0]
	}
	return e.AgentBin
}

// Registry tracks connected runners. All public methods are concurrency-safe.
//
// Both maps are keyed by the TYPE of the thing they index — a connection by
// its ConnectionID, an identity by its RunnerID. Both are comparable structs,
// so this costs nothing at runtime and buys the one guarantee a pair of
// same-shaped strings cannot give: the compiler refuses the mix-up.
type Registry struct {
	mu      sync.RWMutex
	runners map[objproto.ConnectionID]*RunnerEntry
	// byIdentity maps a runner identity to the connection currently holding
	// it. At most one live connection per identity, and a second Add under the
	// same identity TAKES OVER rather than being refused: that is the reconnect
	// path, the runner re-dialled so the old path is dead by hypothesis, and
	// the runner is the authority on its own liveness. Making the new
	// connection wait for the old to be reaped would cost a full
	// --ping-interval, which is the latency this whole change exists to remove.
	byIdentity map[protocol.RunnerID]objproto.ConnectionID

	OnAdd    func(RunnerEntry)                                // optional; called after Add inserts an entry.
	OnRemove func(id objproto.ConnectionID, snap RunnerEntry) // optional; called after Remove deletes an entry.
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		runners:    make(map[objproto.ConnectionID]*RunnerEntry),
		byIdentity: make(map[protocol.RunnerID]objproto.ConnectionID),
	}
}

// Add inserts or replaces the entry keyed by e.ID and claims e.Identity for it.
//
// Returns the connection that previously held that identity, when there was
// one and it was a different connection; the zero ConnectionID when there was
// not. The caller must tear that connection down: two connections claiming one
// runner would both be dispatched to, and the agents of one of them hold
// tickets the board now keys to the other.
func (r *Registry) Add(e *RunnerEntry) (displaced objproto.ConnectionID) {
	r.mu.Lock()
	// Ensure ActiveTasks is initialized.
	if e.ActiveTasks == nil {
		e.ActiveTasks = make(map[string]struct{})
	}
	r.runners[e.ID] = e
	if !e.Identity.IsZero() {
		if prev, ok := r.byIdentity[e.Identity]; ok && prev != e.ID {
			displaced = prev
		}
		r.byIdentity[e.Identity] = e.ID
	}
	snapshot := *e
	onAdd := r.OnAdd
	r.mu.Unlock()
	if onAdd != nil {
		onAdd(snapshot)
	}
	return displaced
}

// Remove deletes the entry with the given id. No-op if absent.
// The snapshot of the entry at removal time is passed to OnRemove so the
// callback can inspect which tasks were stranded.
func (r *Registry) Remove(id objproto.ConnectionID) {
	r.mu.Lock()
	e, existed := r.runners[id]
	var snap RunnerEntry
	if existed {
		snap = *e
	}
	delete(r.runners, id)
	// Only release the identity if it still points at THIS connection. A
	// takeover repoints it and the displaced connection's teardown arrives
	// afterwards — the normal order, since the runner notices a drop before the
	// server's ping timeout does. Without this check that late teardown would
	// strip the identity from the connection now legitimately holding it, and
	// the symptom would be a runner that reconnects and then cannot be found by
	// identity, intermittently.
	if existed && !e.Identity.IsZero() && r.byIdentity[e.Identity] == id {
		delete(r.byIdentity, e.Identity)
	}
	onRemove := r.OnRemove
	r.mu.Unlock()
	if existed && onRemove != nil {
		onRemove(id, snap)
	}
}

// GetByIdentity returns a value snapshot of whichever connection currently
// holds this runner identity — the counterpart of Get for callers that have a
// task's AssignedTo rather than a connection id, and independent of the
// internal map like Get's return.
func (r *Registry) GetByIdentity(rid protocol.RunnerID) (RunnerEntry, bool) {
	e, ok := r.GetLiveByIdentity(rid)
	if !ok {
		return RunnerEntry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return *e, true
}

// GetLiveByIdentity returns the LIVE entry for an identity. Same caveat as
// GetByConnectionID: the pointer aliases registry state, so it is for callers
// that need to store the entry itself (ResolveVia hands it to entry.Via), not
// for reading fields off it.
func (r *Registry) GetLiveByIdentity(rid protocol.RunnerID) (*RunnerEntry, bool) {
	if rid.IsZero() {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byIdentity[rid]
	if !ok {
		return nil, false
	}
	e, ok := r.runners[id]
	return e, ok
}

// Get returns a value snapshot of the entry for id. The returned value is
// independent of the internal map; callers may read or copy it freely.
func (r *Registry) Get(id objproto.ConnectionID) (RunnerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.runners[id]
	if !ok {
		return RunnerEntry{}, false
	}
	return *e, true
}

// SetLastSeen updates the runner's LastSeen timestamp to ts.
// Returns false if the runner is not registered.
func (r *Registry) SetLastSeen(id objproto.ConnectionID, ts time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return false
	}
	e.LastSeen = ts
	return true
}

// BindTask atomically reserves a task slot on the runner. Returns false if
// the runner is unknown or already at capacity. Caller (dispatcher) must
// call UnbindTask on send failure to roll back the reservation.
func (r *Registry) BindTask(id objproto.ConnectionID, taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return false
	}
	if len(e.ActiveTasks) >= e.MaxTasks {
		return false
	}
	if e.ActiveTasks == nil {
		e.ActiveTasks = make(map[string]struct{})
	}
	e.ActiveTasks[taskID] = struct{}{}
	e.LastSeen = time.Now()
	return true
}

// UnbindTask releases a previously-reserved slot. Idempotent: no error if the
// runner is unknown or did not hold the task. This makes it safe to call
// from both the dispatcher's rollback path and the runner_handler's
// TaskFinished path even if they race.
func (r *Registry) UnbindTask(id objproto.ConnectionID, taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return
	}
	delete(e.ActiveTasks, taskID)
	e.LastSeen = time.Now()
}

// Candidates returns runner snapshots that can serve repo, restricted to
// the most specific tier of matching roots (longest-prefix-match) and
// further filtered by the selector (or any runner if Kind == Any).
//
// Specificity per runner is the length of its longest matching root
// (protocol.MatchLen). Across selector-matched runners, only those whose
// score equals the global maximum survive — so a focused per-repo runner
// shadows a broad fallback runner that would otherwise also match.
//
// The slice is capacity-agnostic: at-capacity runners are still listed so
// callers can detect ambiguity even when matching runners are all busy.
//
// The result is sorted by ConnectedAt asc then ID asc for deterministic
// behavior in tests and dispatch.
func (r *Registry) Candidates(repo string, sel protocol.RunnerSelector) []RunnerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type scored struct {
		entry RunnerEntry
		score int
	}
	var matches []scored
	maxScore := 0
	for _, e := range r.runners {
		if !selectorMatches(sel, e) {
			continue
		}
		score := bestRootScore(e.AllowedRoots, repo)
		if score == 0 {
			continue
		}
		if score > maxScore {
			maxScore = score
		}
		matches = append(matches, scored{entry: *e, score: score})
	}
	var out []RunnerEntry
	for _, m := range matches {
		if m.score == maxScore {
			out = append(out, m.entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectedAt.Equal(out[j].ConnectedAt) {
			// A ConnectionID has no order of its own; the canonical string is
			// what "ID asc" has always meant here, and the tie-break only has
			// to be STABLE.
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].ConnectedAt.Before(out[j].ConnectedAt)
	})
	return out
}

// bestRootScore returns the largest protocol.MatchLen across roots, or 0 if
// none contain repo.
func bestRootScore(roots []string, repo string) int {
	best := 0
	for _, root := range roots {
		if s := protocol.MatchLen(root, repo); s > best {
			best = s
		}
	}
	return best
}

func selectorMatches(sel protocol.RunnerSelector, e *RunnerEntry) bool {
	switch sel.Kind {
	case protocol.RunnerSelectorKind_Any:
		return true
	case protocol.RunnerSelectorKind_ByRunnerId:
		// Identity equality, 16 bytes against 16 bytes. It used to parse the
		// entry's connection id and compare four address fields, because the
		// selector carried an address.
		want := sel.RunnerId()
		return want != nil && !want.IsZero() && *want == e.Identity
	case protocol.RunnerSelectorKind_ByConnId:
		// Pins a CONNECTION, so unlike ByRunnerId it stops matching once that
		// runner reconnects. Exists because an operator can still paste the
		// addr= column. Struct equality now that both sides are the type —
		// this compared two rendered strings while the entry's key was one.
		want := sel.ConnId()
		return want != nil && want.ToObjproto() == e.ID
	case protocol.RunnerSelectorKind_ByHostname:
		h := sel.Hostname()
		return h != nil && string(h.Name) == e.Hostname
	case protocol.RunnerSelectorKind_ByIp:
		ip := sel.IpAddr()
		// Straight off the typed key. This used to re-PARSE the entry's id
		// string back into a ConnectionID to reach the same field
		// (parseConnIDForIP / runnerIDIPMatches, both deleted): work that
		// existed only because the key had been flattened to text.
		return ip != nil && addrBytesEqual(e.ID.Addr.Addr().AsSlice(), ip.Addr)
	}
	return false
}

// addrBytesEqual compares an IP's bytes against the selector's, length
// included — a v4 and a v6 address are not equal because one embeds the other.
func addrBytesEqual(got, want []byte) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// GetByConnectionID returns a pointer to the registered RunnerEntry for cid,
// or nil/false on no match.
//
// Unlike Get (same key, value snapshot) this accessor returns the live pointer
// so the via-relay path can read the live ConnHandle and Addr without an extra
// lookup. The map stores *RunnerEntry, so the returned pointer is the same one
// mutations race against — callers must treat it as read-only.
//
// Used by the dial-runner via-relay path (DialRunnerHandler.ResolveVia) to
// resolve a CLI-supplied via=<cid> against the live registered runners.
func (r *Registry) GetByConnectionID(cid objproto.ConnectionID) (*RunnerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.runners[cid]
	if !ok {
		return nil, false
	}
	return entry, true
}

// List returns value snapshots of all entries in arbitrary order.
// The returned slice is independent of the internal map.
func (r *Registry) List() []RunnerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]RunnerEntry, 0, len(r.runners))
	for _, e := range r.runners {
		result = append(result, *e)
	}
	return result
}
