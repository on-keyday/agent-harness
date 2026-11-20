package verb

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestScheduleCapabilityNames(t *testing.T) {
	for name, want := range map[string]protocol.Capability{
		"schedule_edit":              protocol.Capability_ScheduleEdit,
		"schedule_run":               protocol.Capability_ScheduleRun,
		"schedule_edit,schedule_run": protocol.Capability_ScheduleEdit | protocol.Capability_ScheduleRun,
	} {
		got, err := ParseCaps(name)
		if err != nil || got != want {
			t.Fatalf("ParseCaps(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := ParseCaps("schedule"); err == nil {
		t.Fatal("obsolete schedule capability name was accepted")
	}
}
