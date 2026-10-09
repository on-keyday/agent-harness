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

// ExecsStatus carries ExecStatusEvent: started, stats and ended, the same three
// kinds forwards.status carries.
func ExecsStatus() string { return "execs.status" }
