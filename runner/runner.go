package runner

import (
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type RunnerID string // = objproto.ConnectionID の文字列表現
type TaskID string   // ULID, server 発番

type Runner struct {
	ID          RunnerID
	RepoPath    string // 不変 (runner 起動時に宣言)
	Status      protocol.RunnerStatus
	CurrentTask TaskID // Busy のとき非空
	ConnectedAt time.Time
	LastSeen    time.Time
}

type Task struct {
	ID          TaskID
	RepoPath    string // runner マッチング用
	Prompt      string
	Status      protocol.TaskStatus
	AssignedTo  RunnerID // Running 以降で set
	WorktreeDir string   // runner からの絶対 path
	CreatedAt   time.Time
	StartedAt   *time.Time
	EndedAt     *time.Time
	ExitCode    *int
	DiffSHA     string // §9 の扱い次第で空のまま
}
