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
