package topics

import "fmt"

func RunnersStatus() string             { return "runners.status" }
func TasksStatus() string               { return "tasks.status" }
func TaskLog(taskID string) string      { return fmt.Sprintf("task.%s.log", taskID) }
func TaskStatus(taskID string) string   { return fmt.Sprintf("task.%s.status", taskID) }
