package topics

import (
	"testing"
)

func TestRunnersStatus(t *testing.T) {
	got := RunnersStatus()
	want := "runners.status"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTasksStatus(t *testing.T) {
	got := TasksStatus()
	want := "tasks.status"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTaskLog(t *testing.T) {
	got := TaskLog("01HAA")
	want := "task.01HAA.log"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTaskStatus(t *testing.T) {
	got := TaskStatus("01HAA")
	want := "task.01HAA.status"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseTaskLog(t *testing.T) {
	if id, ok := ParseTaskLog(TaskLog("01HAA")); !ok || id != "01HAA" {
		t.Fatalf("ParseTaskLog(TaskLog) = %q, %v", id, ok)
	}
	for _, topic := range []string{TaskStatus("01HAA"), TasksStatus(), "task..log", "task.01HAA.log.x", "xtask.01HAA.log"} {
		if id, ok := ParseTaskLog(topic); ok {
			t.Errorf("ParseTaskLog(%q) = %q, want not a task log", topic, id)
		}
	}
}
