package server

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestResumeFromTerminal verifies the happy path: a terminal task can be
// resumed; the resume re-queues it with the new prompt + extras and the
// returned snapshot reflects the reset state.
func TestResumeFromTerminal(t *testing.T) {
	tasks := NewTaskStore()
	id := tasks.Create("/repo", "old prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "runnerA", protocol.RunnerSelector{}, []string{"--A"}, protocol.Capability_All)
	// Drive task through Assign + Finish to a terminal state.
	tasks.Assign(id, "runnerA", "/wt")
	tasks.Finish(id, 0, nil) // Succeeded

	got, err := tasks.Resume(id, "new prompt", []string{"--B"}, protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, "runnerB", protocol.ClientKind_Unspecified, false, protocol.Capability_None)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.Status != protocol.TaskStatus_Queued {
		t.Errorf("Status: got %v want Queued", got.Status)
	}
	if got.Prompt != "new prompt" {
		t.Errorf("Prompt: got %q", got.Prompt)
	}
	if len(got.ExtraArgs) != 1 || got.ExtraArgs[0] != "--B" {
		t.Errorf("ExtraArgs: got %v", got.ExtraArgs)
	}
	if got.BoundRunnerID != "runnerB" {
		t.Errorf("BoundRunnerID: got %q want runnerB", got.BoundRunnerID)
	}
	if got.AssignedTo != "" {
		t.Errorf("AssignedTo not cleared: %q", got.AssignedTo)
	}
	if got.WorktreeDir != "" {
		t.Errorf("WorktreeDir not cleared: %q", got.WorktreeDir)
	}
	if got.RepoPath != "/repo" {
		t.Errorf("RepoPath should be preserved: got %q", got.RepoPath)
	}
}

// TestResumeRejectsNonTerminal verifies that Resume returns ResumeErrNotTerminal
// for tasks that are still Queued or Running. This is the multi-resume guard:
// a second concurrent Resume call lands here because the first transitioned
// the entry to Queued.
func TestResumeRejectsNonTerminal(t *testing.T) {
	tasks := NewTaskStore()

	t.Run("queued", func(t *testing.T) {
		id := tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
		_, err := tasks.Resume(id, "x", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Unspecified, false, protocol.Capability_None)
		if err != ResumeErrNotTerminal {
			t.Errorf("got %v, want ResumeErrNotTerminal", err)
		}
	})

	t.Run("running", func(t *testing.T) {
		id := tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
		tasks.Assign(id, "r1", "/wt")
		_, err := tasks.Resume(id, "x", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Unspecified, false, protocol.Capability_None)
		if err != ResumeErrNotTerminal {
			t.Errorf("got %v, want ResumeErrNotTerminal", err)
		}
	})
}

func TestResumeRejectsUnknown(t *testing.T) {
	tasks := NewTaskStore()
	_, err := tasks.Resume("00000000000000000000000000000000", "x", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Unspecified, false, protocol.Capability_None)
	if err != ResumeErrNotFound {
		t.Errorf("got %v, want ResumeErrNotFound", err)
	}
}

// TestResumeConcurrentSingleWinner spins up N goroutines that all try to
// Resume the same terminal task. Exactly one should win; the rest should
// see ResumeErrNotTerminal because the winner already flipped status to
// Queued under the lock. This is the explicit "no double-resume" guard the
// user asked for.
func TestResumeConcurrentSingleWinner(t *testing.T) {
	tasks := NewTaskStore()
	id := tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All)
	tasks.Assign(id, "r1", "/wt")
	tasks.Finish(id, 0, nil)

	const N = 16
	var (
		wg          sync.WaitGroup
		successes   int32
		notTerminal int32
		notFound    int32
		other       int32
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := tasks.Resume(id, "x", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Unspecified, false, protocol.Capability_None)
			switch err {
			case nil:
				atomic.AddInt32(&successes, 1)
			case ResumeErrNotTerminal:
				atomic.AddInt32(&notTerminal, 1)
			case ResumeErrNotFound:
				atomic.AddInt32(&notFound, 1)
			default:
				atomic.AddInt32(&other, 1)
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Errorf("expected exactly 1 successful Resume, got %d", successes)
	}
	if notTerminal != N-1 {
		t.Errorf("expected %d ResumeErrNotTerminal, got %d", N-1, notTerminal)
	}
	if notFound != 0 || other != 0 {
		t.Errorf("unexpected outcomes: notFound=%d other=%d", notFound, other)
	}
}

// TestSubmitRequestResumeRoundTrip verifies the wire format carries
// resume_task_id end-to-end and AsStrings() works on the embedded extra args.
func TestSubmitRequestResumeRoundTrip(t *testing.T) {
	orig := protocol.SubmitRequest{
		Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any},
	}
	orig.SetRepoPath([]byte("/r"))
	orig.SetPrompt([]byte("p"))
	orig.ExtraArgs = protocol.ClaudeArgsFromStrings([]string{"--resume", "uuid-1"})
	var resumeID protocol.TaskID
	for i := range resumeID.Id {
		resumeID.Id[i] = byte(i + 1)
	}
	orig.ResumeTaskId = resumeID

	wire := orig.MustAppend(nil)
	var got protocol.SubmitRequest
	if err := got.DecodeExact(wire); err != nil {
		t.Fatalf("DecodeExact: %v", err)
	}
	if got.ResumeTaskId.Id != resumeID.Id {
		t.Errorf("ResumeTaskId mismatch: got %x want %x", got.ResumeTaskId.Id, resumeID.Id)
	}
	args := got.ExtraArgs.AsStrings()
	if len(args) != 2 || args[0] != "--resume" || args[1] != "uuid-1" {
		t.Errorf("ExtraArgs: got %v", args)
	}
}
