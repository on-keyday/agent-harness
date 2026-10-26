package topics

import (
	"fmt"
	"strings"
)

func RunnersStatus() string           { return "runners.status" }
func TasksStatus() string             { return "tasks.status" }
func TaskLog(taskID string) string    { return fmt.Sprintf("task.%s.log", taskID) }
func TaskStatus(taskID string) string { return fmt.Sprintf("task.%s.status", taskID) }
func Notifications() string           { return "notifications" }
func ConnsStatus() string             { return "conns.status" }

// ParseTaskLog is TaskLog's inverse: the task id when topic is a task log.
func ParseTaskLog(topic string) (taskID string, ok bool) {
	id, ok := strings.CutPrefix(topic, "task.")
	if !ok {
		return "", false
	}
	id, ok = strings.CutSuffix(id, ".log")
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// ForwardsStatus carries ForwardStatusEvent: a forward joined the registry,
// left it, or its counters moved. The third is why this topic exists at all —
// a listing without it goes stale in place while bytes cross.
func ForwardsStatus() string { return "forwards.status" }

// ExecsStatus carries ExecStatusEvent: started, stats and ended, the same three
// kinds forwards.status carries.
func ExecsStatus() string { return "execs.status" }
