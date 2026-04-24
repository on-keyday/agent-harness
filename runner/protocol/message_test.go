package protocol

import (
	"bytes"
	"testing"
)

func TestRunnerHelloRoundTrip(t *testing.T) {
	const wantVersion = uint8(42)
	wantRepoPath := []byte("/home/runner/repos/myproject")

	orig := RunnerHello{
		Version: wantVersion,
	}
	if !orig.SetRepoPath(wantRepoPath) {
		t.Fatal("SetRepoPath returned false unexpectedly")
	}

	buf := orig.MustAppend(nil)

	var decoded RunnerHello
	remain, err := decoded.Decode(buf)
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	if len(remain) != 0 {
		t.Fatalf("expected no remaining bytes after Decode, got %d", len(remain))
	}

	if decoded.Version != wantVersion {
		t.Errorf("Version: got %d, want %d", decoded.Version, wantVersion)
	}
	if !bytes.Equal(decoded.RepoPath, wantRepoPath) {
		t.Errorf("RepoPath: got %q, want %q", decoded.RepoPath, wantRepoPath)
	}
	if decoded.RepoPathLen != uint16(len(wantRepoPath)) {
		t.Errorf("RepoPathLen: got %d, want %d", decoded.RepoPathLen, len(wantRepoPath))
	}
}

func TestTaskInfoRoundTrip(t *testing.T) {
	wantRepoPath := []byte("/srv/repos/agent-harness")
	wantWorktreeDir := []byte("/srv/worktrees/task-001")
	wantPrompt := []byte("Implement the feature described in issue #42.")
	const wantStatus = TaskStatus_Running
	const wantCreatedAt = uint64(1234567890)
	const wantStartedAt = uint64(1700000001)
	const wantEndedAt = uint64(1700000099)
	const wantExitCode = int32(-1)

	var wantID TaskID
	wantID.Id[0] = 0xAB
	wantID.Id[1] = 0xCD
	wantID.Id[15] = 0xEF

	// AssignedTo (RunnerID) requires IpAddrLen to be 4 (IPv4) or 16 (IPv6);
	// the zero value triggers an assertion failure, so we populate it minimally.
	assignedTo := RunnerID{}
	assignedTo.SetIpAddr([]byte{127, 0, 0, 1})

	orig := TaskInfo{
		Id:         wantID,
		Status:     wantStatus,
		CreatedAt:  wantCreatedAt,
		StartedAt:  wantStartedAt,
		EndedAt:    wantEndedAt,
		ExitCode:   wantExitCode,
		AssignedTo: assignedTo,
	}
	if !orig.SetRepoPath(wantRepoPath) {
		t.Fatal("SetRepoPath returned false unexpectedly")
	}
	if !orig.SetWorktreeDir(wantWorktreeDir) {
		t.Fatal("SetWorktreeDir returned false unexpectedly")
	}
	if !orig.SetPrompt(wantPrompt) {
		t.Fatal("SetPrompt returned false unexpectedly")
	}

	buf := orig.MustAppend(nil)

	var decoded TaskInfo
	remain, err := decoded.Decode(buf)
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	if len(remain) != 0 {
		t.Fatalf("expected no remaining bytes after Decode, got %d", len(remain))
	}

	// Verify Id (the whole array must match)
	if decoded.Id != wantID {
		t.Errorf("Id: got %v, want %v", decoded.Id, wantID)
	}

	if decoded.Status != wantStatus {
		t.Errorf("Status: got %d, want %d", decoded.Status, wantStatus)
	}

	if !bytes.Equal(decoded.RepoPath, wantRepoPath) {
		t.Errorf("RepoPath: got %q, want %q", decoded.RepoPath, wantRepoPath)
	}
	if decoded.RepoPathLen != uint16(len(wantRepoPath)) {
		t.Errorf("RepoPathLen: got %d, want %d", decoded.RepoPathLen, len(wantRepoPath))
	}
	if !bytes.Equal(decoded.WorktreeDir, wantWorktreeDir) {
		t.Errorf("WorktreeDir: got %q, want %q", decoded.WorktreeDir, wantWorktreeDir)
	}
	if decoded.WorktreeDirLen != uint16(len(wantWorktreeDir)) {
		t.Errorf("WorktreeDirLen: got %d, want %d", decoded.WorktreeDirLen, len(wantWorktreeDir))
	}
	if !bytes.Equal(decoded.Prompt, wantPrompt) {
		t.Errorf("Prompt: got %q, want %q", decoded.Prompt, wantPrompt)
	}
	if decoded.PromptLen != uint32(len(wantPrompt)) {
		t.Errorf("PromptLen: got %d, want %d", decoded.PromptLen, len(wantPrompt))
	}

	if decoded.CreatedAt != wantCreatedAt {
		t.Errorf("CreatedAt: got %d, want %d", decoded.CreatedAt, wantCreatedAt)
	}
	if decoded.StartedAt != wantStartedAt {
		t.Errorf("StartedAt: got %d, want %d", decoded.StartedAt, wantStartedAt)
	}
	if decoded.EndedAt != wantEndedAt {
		t.Errorf("EndedAt: got %d, want %d", decoded.EndedAt, wantEndedAt)
	}
	if decoded.ExitCode != wantExitCode {
		t.Errorf("ExitCode: got %d, want %d", decoded.ExitCode, wantExitCode)
	}

	if !bytes.Equal(decoded.AssignedTo.IpAddr, []byte{127, 0, 0, 1}) {
		t.Errorf("AssignedTo.IpAddr: got %v, want %v", decoded.AssignedTo.IpAddr, []byte{127, 0, 0, 1})
	}
}
