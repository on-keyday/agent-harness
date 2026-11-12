package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func TestRegistryAddFindRemove(t *testing.T) {
	r := NewRegistry()
	now := time.Now()

	r.Add(&RunnerEntry{
		ID:               tcid("A"),
		Hostname:         "hostA",
		AllowedRoots:     []string{"/x"},
		MaxTasks:         1,
		ActiveTasks:      map[string]struct{}{},
		ConnectedAt:      now,
		LastTaskActivity: now,
	})

	entry, ok := r.Get(tcid("A"))
	if !ok {
		t.Fatal("expected Get(\"A\") ok=true, got false")
	}
	if len(entry.AllowedRoots) == 0 || entry.AllowedRoots[0] != "/x" {
		t.Fatalf("expected AllowedRoots[\"/x\"], got %v", entry.AllowedRoots)
	}

	r.Remove(tcid("A"))

	_, ok = r.Get(tcid("A"))
	if ok {
		t.Fatal("expected Get(\"A\") ok=false after Remove, got true")
	}
}

func TestRegistryNoteTaskActivity(t *testing.T) {
	r := NewRegistry()
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)

	r.Add(&RunnerEntry{
		ID:               tcid("A"),
		Hostname:         "hostA",
		AllowedRoots:     []string{"/x"},
		MaxTasks:         1,
		ActiveTasks:      map[string]struct{}{},
		ConnectedAt:      t0,
		LastTaskActivity: t0,
	})

	ok := r.NoteTaskActivity(tcid("A"), t1)
	if !ok {
		t.Fatal("expected NoteTaskActivity to return true for registered runner, got false")
	}

	entry, _ := r.Get(tcid("A"))
	if !entry.LastTaskActivity.Equal(t1) {
		t.Fatalf("expected LastTaskActivity=%v after NoteTaskActivity, got %v", t1, entry.LastTaskActivity)
	}

	// Returns false for unknown runner.
	if r.NoteTaskActivity(tcid("nonexistent"), t1) {
		t.Fatal("expected NoteTaskActivity to return false for unknown runner, got true")
	}
}

func TestRegistryReadIsSnapshot(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunnerEntry{
		ID:           tcid("A"),
		Hostname:     "hostA",
		AllowedRoots: []string{"/original"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Now(),
	})

	got, ok := r.Get(tcid("A"))
	if !ok {
		t.Fatal("expected Get ok=true")
	}

	// Mutate the returned value snapshot; the registry must not be affected.
	got.Hostname = "poison"

	second, _ := r.Get(tcid("A"))
	if second.Hostname != "hostA" {
		t.Fatalf("registry was poisoned by mutating returned snapshot: got Hostname=%q, want \"hostA\"", second.Hostname)
	}
}

func TestRegistryBindTaskAtCapacity(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: tcid("A"), Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now,
	})
	if !r.BindTask(tcid("A"), "t1") {
		t.Fatal("expected first BindTask to succeed")
	}
	if r.BindTask(tcid("A"), "t2") {
		t.Fatal("expected second BindTask to fail at capacity")
	}
	r.UnbindTask(tcid("A"), "t1")
	if !r.BindTask(tcid("A"), "t2") {
		t.Fatal("expected BindTask to succeed after UnbindTask")
	}
}

func TestRegistryUnbindTaskIdempotent(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: tcid("A"), Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 2,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now,
	})
	r.UnbindTask(tcid("A"), "absent") // double-release safe
	r.BindTask(tcid("A"), "t1")
	r.UnbindTask(tcid("A"), "t1")
	r.UnbindTask(tcid("A"), "t1") // idempotent on already-unbound
}

func TestRegistryBindTaskRace(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: tcid("A"), Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now,
	})
	const N = 64
	results := make(chan bool, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- r.BindTask(tcid("A"), fmt.Sprintf("t%d", i))
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for ok := range results {
		if ok {
			successes++
		}
	}
	if successes != 4 {
		t.Fatalf("expected exactly 4 successful binds (MaxTasks), got %d", successes)
	}
}

// TestRegistryStatusMethod verifies the Status() method derives status from connection + capacity.
func TestRegistryStatusMethod(t *testing.T) {
	now := time.Now()

	// No conn = Offline
	e := &RunnerEntry{
		ID: tcid("A"), Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now,
		Conn: nil,
	}
	if s := e.Status(); s.String() != "Offline" {
		t.Fatalf("expected Offline when Conn==nil, got %v", s)
	}

	// With a mock conn and no active tasks = Idle
	e.Conn = &fakeConn{}
	e.MaxTasks = 2
	if s := e.Status(); s.String() != "Idle" {
		t.Fatalf("expected Idle, got %v", s)
	}

	// Fill to capacity = Busy
	e.ActiveTasks["t1"] = struct{}{}
	e.ActiveTasks["t2"] = struct{}{}
	if s := e.Status(); s.String() != "Busy" {
		t.Fatalf("expected Busy at capacity, got %v", s)
	}
}

// Roots match exactly (G9): a runner serves a repo only when one of its roots
// IS that repo. A root that merely contains the repo does not.
func TestRegistryCandidatesExactMatch(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: tcid("A"), Identity: testRunnerID("A"), Hostname: "h1", AllowedRoots: []string{"/home/u/ws", "/home/u/ws/repo1"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now})
	r.Add(&RunnerEntry{ID: tcid("B"), Identity: testRunnerID("B"), Hostname: "h2", AllowedRoots: []string{"/srv/ws"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now})

	any := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	if cs := r.Candidates("/home/u/ws/repo1", any); len(cs) != 1 || cs[0].ID != tcid("A") {
		t.Fatalf("expected only A, got %v", cs)
	}
	if cs := r.Candidates("/home/u/ws/other", any); len(cs) != 0 {
		t.Fatalf("a repo below a root must have no candidate, got %v", cs)
	}
	if cs := r.Candidates("/srv/ws/foo", any); len(cs) != 0 {
		t.Fatalf("a repo below a root must have no candidate, got %v", cs)
	}
}

// The silent fallback G9 removes: with the repo's own runner absent, a runner
// rooted at a parent directory must not become its candidate.
func TestRegistryCandidatesParentRootDoesNotServe(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: tcid("broad"), Identity: testRunnerID("broad"), Hostname: "h", AllowedRoots: []string{"/a"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: stubConn{}})
	if cs := r.Candidates("/a/b", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}); len(cs) != 0 {
		t.Fatalf("parent-rooted runner must not serve /a/b, got %v", cs)
	}
}

func TestRegistryCandidatesAmbiguous(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: tcid("A"), Identity: testRunnerID("A"), Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now})
	r.Add(&RunnerEntry{ID: tcid("B"), Identity: testRunnerID("B"), Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now})
	cs := r.Candidates("/shared", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	if len(cs) != 2 {
		t.Fatalf("expected 2 candidates (ambiguous), got %d", len(cs))
	}
}

func TestRegistryCandidatesCapacityAgnostic(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: tcid("A"), Identity: testRunnerID("A"), Hostname: "h1", AllowedRoots: []string{"/x/repo"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{"existing": {}}, ConnectedAt: now, LastTaskActivity: now})
	cs := r.Candidates("/x/repo", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	if len(cs) != 1 {
		t.Fatalf("Candidates must include at-capacity runners (caller filters), got %d", len(cs))
	}
}

func TestRegistryOnRemovePassesSnapshot(t *testing.T) {
	r := NewRegistry()
	var got RunnerEntry
	r.OnRemove = func(id objproto.ConnectionID, snap RunnerEntry) {
		got = snap
	}
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: tcid("A"), Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 2,
		ActiveTasks: map[string]struct{}{"t1": {}, "t2": {}}, ConnectedAt: now, LastTaskActivity: now,
	})
	r.Remove(tcid("A"))
	if got.ID != tcid("A") || len(got.ActiveTasks) != 2 {
		t.Fatalf("snapshot lost ActiveTasks: %+v", got)
	}
}

func TestRegistryCandidatesSelectorByHostname(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: tcid("A"), Identity: testRunnerID("A"), Hostname: "gmkhost", AllowedRoots: []string{"/x/repo"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now})
	r.Add(&RunnerEntry{ID: tcid("B"), Identity: testRunnerID("B"), Hostname: "raspi", AllowedRoots: []string{"/x/repo"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now})

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	h := protocol.Hostname{}
	h.SetName([]byte("gmkhost"))
	sel.SetHostname(h)

	cs := r.Candidates("/x/repo", sel)
	if len(cs) != 1 || cs[0].ID != tcid("A") {
		t.Fatalf("expected only A (gmkhost), got %v", cs)
	}
}

// TestRegistry_PhaseAEntry_NoVia verifies that a Phase A (direct-dial)
// registration leaves Via nil and ViaDialAddr at the zero value.
func TestRegistry_PhaseAEntry_NoVia(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID:               tcid("ws:127.0.0.1:8540-1"),
		Hostname:         "direct-runner",
		AllowedRoots:     []string{"/x"},
		MaxTasks:         1,
		ActiveTasks:      map[string]struct{}{},
		ConnectedAt:      now,
		LastTaskActivity: now,
		// Via and ViaDialAddr intentionally left zero (Phase A direct).
	})
	entry, ok := r.Get(tcid("ws:127.0.0.1:8540-1"))
	if !ok {
		t.Fatal("expected Get ok=true")
	}
	if entry.Via != nil {
		t.Errorf("Phase A entry should have Via=nil, got %+v", entry.Via)
	}
	var zeroConnID objproto.ConnectionID
	if entry.ViaDialAddr != zeroConnID {
		t.Errorf("Phase A entry should have ViaDialAddr zero, got %v", entry.ViaDialAddr)
	}
}

// TestRegistry_PhaseCEntry_ViaPopulated verifies that a Phase C (via-relay)
// registration stores the Via pointer and ViaDialAddr correctly and that
// walking Via reaches the parent entry.
func TestRegistry_PhaseCEntry_ViaPopulated(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	proxyEntry := &RunnerEntry{
		ID:               tcid("ws:127.0.0.1:8541-1"),
		Hostname:         "proxy-runner",
		AllowedRoots:     []string{"/x"},
		MaxTasks:         1,
		ActiveTasks:      map[string]struct{}{},
		ConnectedAt:      now,
		LastTaskActivity: now,
	}
	r.Add(proxyEntry)

	// Use ParseConnectionID for a valid CID value representing L's LISTEN_ADDR.
	parsedViaAddr, err := objproto.ParseConnectionID("ws:10.0.0.1:9000-42", 0)
	if err != nil {
		t.Fatalf("ParseConnectionID: %v", err)
	}

	childEntry := &RunnerEntry{
		ID:               tcid("ws:10.0.0.1:9000-42"),
		Hostname:         "child-runner",
		AllowedRoots:     []string{"/x"},
		MaxTasks:         1,
		ActiveTasks:      map[string]struct{}{},
		ConnectedAt:      now,
		LastTaskActivity: now,
		Via:              proxyEntry,
		ViaDialAddr:      parsedViaAddr,
	}
	r.Add(childEntry)

	got, ok := r.Get(tcid("ws:10.0.0.1:9000-42"))
	if !ok {
		t.Fatal("expected Get ok=true for child entry")
	}
	if got.Via == nil {
		t.Fatal("expected Via non-nil for Phase C entry")
	}
	if got.Via.ID != tcid("ws:127.0.0.1:8541-1") {
		t.Errorf("Via.ID: got %q, want %q", got.Via.ID, "ws:127.0.0.1:8541-1")
	}
	if got.ViaDialAddr != parsedViaAddr {
		t.Errorf("ViaDialAddr: got %v, want %v", got.ViaDialAddr, parsedViaAddr)
	}
	// Walk terminates: proxy's Via is nil.
	if got.Via.Via != nil {
		t.Errorf("proxy entry Via should be nil, got %+v", got.Via.Via)
	}
}

// TestRegistry_ViaWalk_TerminatesAtNil verifies that walking a 3-hop chain
// (L.Via=P, P.Via=Q, Q.Via=nil) terminates at the nil sentinel.
func TestRegistry_ViaWalk_TerminatesAtNil(t *testing.T) {
	now := time.Now()
	makeEntry := func(id string) *RunnerEntry {
		return &RunnerEntry{
			ID: tcid(id), Hostname: id, AllowedRoots: []string{"/x"},
			MaxTasks: 1, ActiveTasks: map[string]struct{}{},
			ConnectedAt: now, LastTaskActivity: now,
		}
	}
	q := makeEntry("Q")
	p := makeEntry("P")
	p.Via = q
	l := makeEntry("L")
	l.Via = p

	// Walk from L following Via pointers; collect visited IDs.
	var visited []objproto.ConnectionID
	cur := l
	for cur != nil {
		visited = append(visited, cur.ID)
		cur = cur.Via
	}

	want := []objproto.ConnectionID{tcid("L"), tcid("P"), tcid("Q")}
	if len(visited) != len(want) {
		t.Fatalf("walk length: got %d (%v), want %d (%v)", len(visited), visited, len(want), want)
	}
	for i, id := range want {
		if visited[i] != id {
			t.Errorf("walk[%d]: got %v, want %v", i, visited[i], id)
		}
	}
}

func TestHasProfileAcrossExeSuffix(t *testing.T) {
	e := RunnerEntry{AgentProfiles: []string{"claude"}}
	if !e.HasProfile("claude.exe") {
		t.Error("recorded claude.exe must match advertised claude")
	}
	e = RunnerEntry{AgentProfiles: []string{"claude.exe"}}
	if !e.HasProfile("claude") {
		t.Error("recorded claude must match advertised claude.exe")
	}
	if e.HasProfile("codex") {
		t.Error("distinct profile must not match")
	}
	// Legacy runner: implicit AgentBin profile follows the same equivalence.
	legacy := RunnerEntry{AgentBin: "claude.exe"}
	if !legacy.HasProfile("claude") {
		t.Error("legacy AgentBin claude.exe must match claude")
	}
}
