package runner

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

type mockSender struct {
	mu        sync.Mutex
	sent      [][]byte
	publishes []publishedMsg
}

type publishedMsg struct {
	topic string
	data  []byte
}

func (m *mockSender) Send(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, append([]byte{}, data...))
	return nil
}
func (m *mockSender) ID() objproto.ConnectionID { return objproto.ConnectionID{} }
func (m *mockSender) Publish(topic string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishes = append(m.publishes, publishedMsg{topic, append([]byte{}, data...)})
	return nil
}

func TestHandleAssignSuccessSequence(t *testing.T) {
	repo := initRepo(t)
	ms := &mockSender{}
	fakePath, _ := filepath.Abs("../testdata/fake-claude.sh") // relative from runner/
	s := &Session{
		RepoPath:  repo,
		ClaudeBin: fakePath,
		Timeout:   5 * time.Second,
		Sender:    ms,
		Now:       time.Now,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xAB
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("hello"),
	}
	s.handleAssign(context.Background(), req)

	// Should have sent at least 3 control messages: Accepted, Started, Finished.
	if len(ms.sent) < 3 {
		t.Fatalf("expected ≥3 messages, got %d", len(ms.sent))
	}

	// First should be TaskAccepted.
	accepted := decodeRunnerMsg(t, ms.sent[0])
	if accepted.Kind != protocol.RunnerMessageType_TaskAccepted {
		t.Fatalf("first msg kind: %v", accepted.Kind)
	}

	// Last should be TaskFinished with exit_code=0.
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last msg kind: %v", last.Kind)
	}
	tf := last.TaskFinished()
	if tf == nil || tf.ExitCode != 0 {
		t.Fatalf("finished: %+v", tf)
	}

	// Some TaskStarted in between.
	foundStarted := false
	for _, m := range ms.sent[1 : len(ms.sent)-1] {
		if decodeRunnerMsg(t, m).Kind == protocol.RunnerMessageType_TaskStarted {
			foundStarted = true
			break
		}
	}
	if !foundStarted {
		t.Fatalf("missing TaskStarted in: %v", ms.sent)
	}

	// Logs published to the right topic.
	expectedTopic := topics.TaskLog("ab" + strings.Repeat("00", 15))
	if len(ms.publishes) == 0 {
		t.Fatalf("expected log publishes, got none")
	}
	for _, p := range ms.publishes {
		if p.topic != expectedTopic {
			t.Fatalf("unexpected topic %q (want %q)", p.topic, expectedTopic)
		}
	}
	// Some chunk should contain the prompt echo.
	var combined []byte
	for _, p := range ms.publishes {
		combined = append(combined, p.data...)
	}
	if !strings.Contains(string(combined), "hello") {
		t.Fatalf("prompt not echoed in logs: %q", combined)
	}
}

func TestHandleAssignWorktreeFailureReportsFinished(t *testing.T) {
	// Use a non-existent repo path; WorktreeManager.Create will fail.
	ms := &mockSender{}
	s := &Session{
		RepoPath:  "/no/such/repo",
		ClaudeBin: "/bin/true",
		Timeout:   1 * time.Second,
		Sender:    ms,
		Now:       time.Now,
	}
	req := &protocol.AssignTask{TaskId: protocol.TaskID{}, Prompt: []byte("x")}
	s.handleAssign(context.Background(), req)

	// Should have sent: TaskAccepted, then TaskFinished with error info.
	if len(ms.sent) < 2 {
		t.Fatalf("expected ≥2 messages, got %d", len(ms.sent))
	}
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last kind: %v", last.Kind)
	}
	tf := last.TaskFinished()
	if tf == nil || tf.ExitCode == 0 {
		t.Fatalf("expected non-zero exit on worktree failure, got %+v", tf)
	}
	if !bytes.Contains(tf.DiffInfo, []byte("worktree_error")) {
		t.Fatalf("expected 'worktree_error' in DiffInfo, got %q", tf.DiffInfo)
	}
}

// decodeRunnerMsg parses the wire-prefixed RunnerControl payload from a Sender.Send call.
func decodeRunnerMsg(t *testing.T, raw []byte) *protocol.RunnerMessage {
	t.Helper()
	if len(raw) == 0 || raw[0] != byte(wire.ApplicationPayloadKind_RunnerControl) {
		t.Fatalf("expected RunnerControl prefix byte, got %v", raw)
	}
	msg := &protocol.RunnerMessage{}
	if _, err := msg.Decode(raw[1:]); err != nil {
		t.Fatalf("decode runner msg: %v", err)
	}
	return msg
}
