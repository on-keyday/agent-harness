package server

import (
	"encoding/hex"
	"log/slog"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// D16 says re-adoption re-binds capacity. It did not: the call passed the
// runner's IDENTITY hex where Registry.BindTask wants the CONNECTION id it
// keys `runners` by, so the lookup missed, BindTask returned false, and the
// return was discarded. Every other call site passes `runner.ID`; this one was
// the odd one out, and it compiled because the registry API takes a bare
// string — the same identity-vs-connection-id confusion TaskEntry's own
// comment warns about beside AssignedTo / BoundRunnerID.
//
// The consequence is not the capacity. A task absent from ActiveTasks is
// invisible to failAndRevokeTasksOf, so when that runner's connection later
// drops NOTHING fails the task and the row stays non-terminal forever. Found
// on the live fleet 2026-09-10: one interactive task sat `Detached` with its
// runner identity registered nowhere, while a sibling on the same runner that
// had never been re-adopted was correctly `Failed err="runner_disconnected"`.
//
// Nothing caught it because no readopt test ever put a runner in the registry:
// they all build `NewRegistry()` empty, so BindTask missed there too and the
// assertions were about the store.
func readoptFixture(t *testing.T) (srv *Server, reg *Registry, store *TaskStore, identity protocol.RunnerID, cid objproto.ConnectionID, taskID string) {
	t.Helper()
	store, _ = storeWithWAL(t)
	reg = NewRegistry()
	srv = &Server{tasks: store, registry: reg, cfg: Config{Logger: slog.Default(), DataDir: t.TempDir()}}
	identity.Id[0] = 0xaa
	cid = buildTestCID("ws:127.0.0.1:8539-77")
	reg.Add(&RunnerEntry{
		ID: cid, Identity: identity, Hostname: "h", MaxTasks: 4,
		ActiveTasks: map[string]struct{}{},
		ConnectedAt: time.Unix(1, 0), LastSeen: time.Unix(1, 0),
	})
	taskID = runningTask(t, store, identity)
	return srv, reg, store, identity, cid, taskID
}

func heldReportFor(t *testing.T, store *TaskStore, identity protocol.RunnerID, taskID string) protocol.HeldTasksReport {
	t.Helper()
	var hold protocol.HoldID
	hold.Id[0] = 0xc0
	if err := store.MarkHold(taskID, identity.Hex(), hex.EncodeToString(hold.Id[:]),
		time.Now().Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskID)
	copy(tid.Id[:], raw)
	rep := protocol.HeldTasksReport{HoldId: hold}
	rep.SetTasks([]protocol.HeldTask{{TaskId: tid, Ticket: [16]byte{1}}})
	return rep
}

func TestReadoptBindsTheTaskToTheRunnersConnection(t *testing.T) {
	srv, reg, store, identity, cid, taskID := readoptFixture(t)
	report := heldReportFor(t, store, identity, taskID)

	res := srv.readoptHeldTasks(identity, report)
	if len(res.Accepted) != 1 {
		t.Fatalf("accepted %d, want 1", len(res.Accepted))
	}

	e, ok := reg.Get(cid)
	if !ok {
		t.Fatal("the runner left the registry")
	}
	if _, bound := e.ActiveTasks[taskID]; !bound {
		t.Errorf("re-adopted task is not in ActiveTasks (%v) — it is then invisible to "+
			"failAndRevokeTasksOf, so its row survives the runner going away", e.ActiveTasks)
	}
}

// The consequence, through the REAL sweep rather than a re-statement of it:
// once bound, the disconnect path fails the task like any other.
func TestReadoptedTaskIsSweptWhenItsRunnerGoesAway(t *testing.T) {
	srv, reg, store, identity, cid, taskID := readoptFixture(t)
	report := heldReportFor(t, store, identity, taskID)
	if res := srv.readoptHeldTasks(identity, report); len(res.Accepted) != 1 {
		t.Fatalf("accepted %d, want 1", len(res.Accepted))
	}

	snap, ok := reg.Get(cid)
	if !ok {
		t.Fatal("the runner left the registry")
	}
	srv.failAndRevokeTasksOf(cid, snap)

	got, _ := store.Get(taskID)
	if got.Status != protocol.TaskStatus_Failed {
		t.Errorf("status = %v, want Failed — a re-adopted task whose runner disappears "+
			"must not stay non-terminal", got.Status)
	}
	if string(got.ErrorMsg) != "runner_disconnected" {
		t.Errorf("reason = %q, want runner_disconnected", got.ErrorMsg)
	}
}
