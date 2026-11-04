package verb

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRunnerDialAddNeedsKey(t *testing.T) {
	if _, err := ParseCmdRunnerDialAdd(CLI, []string{"ws:127.0.0.1:8540-*"}, nil); err == nil || !strings.Contains(err.Error(), "--key") {
		t.Fatalf("no --key: %v, want an error naming --key", err)
	}
	a, err := ParseCmdRunnerDialAdd(CLI, []string{"ws:a.example:8540-*,udp:a.example:8541-*", "--key", "3f9c01ab", "--label", "aws"}, nil)
	if err != nil || a.Candidates != "ws:a.example:8540-*,udp:a.example:8541-*" || a.Key != "3f9c01ab" || a.Label != "aws" {
		t.Fatalf("parsed %+v %v", a, err)
	}
}

func TestRunnerKickParses(t *testing.T) {
	a, err := ParseCmdRunnerKick(CLI, []string{"83e97d9c", "--force"}, nil)
	if err != nil || a.RunnerID != "83e97d9c" || !a.Force {
		t.Fatalf("parsed %+v %v", a, err)
	}
}

func TestCapDescriptionNamesDialAndKick(t *testing.T) {
	admin := CapDescription(protocol.Capability_RunnerAdmin)
	for _, v := range []string{"runner dial add", "runner dial ls", "runner dial rm"} {
		if !strings.Contains(admin, v) {
			t.Errorf("runner_admin description %q does not name %q", admin, v)
		}
	}
	if enroll := CapDescription(protocol.Capability_RunnerEnroll); !strings.Contains(enroll, "runner kick") {
		t.Errorf("runner_enroll description %q does not name runner kick", enroll)
	}
}
