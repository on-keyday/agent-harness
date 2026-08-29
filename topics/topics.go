package topics

import "fmt"

func RunnersStatus() string           { return "runners.status" }
func TasksStatus() string             { return "tasks.status" }
func TaskLog(taskID string) string    { return fmt.Sprintf("task.%s.log", taskID) }
func TaskStatus(taskID string) string { return fmt.Sprintf("task.%s.status", taskID) }
func Notifications() string           { return "notifications" }
func ConnsStatus() string             { return "conns.status" }

// ForwardsStatus carries ForwardStatusEvent: a forward joined the registry,
// left it, or its counters moved. The third is why this topic exists at all —
// a listing without it goes stale in place while bytes cross.
func ForwardsStatus() string { return "forwards.status" }

// ExecsStatus carries ExecStatusEvent. Its sibling above, minus the stats kind:
// an exec's row does not change while it runs.
func ExecsStatus() string { return "execs.status" }
