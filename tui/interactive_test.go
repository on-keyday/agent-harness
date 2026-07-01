package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestResumeSelectorOpts(t *testing.T) {
	if got := resumeSelectorOpts(protocol.RunnerID{}); got != (cli.SelectorOpts{}) {
		t.Errorf("zero-value AssignedTo: want Any (empty SelectorOpts), got %+v", got)
	}

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{192, 168, 3, 14})
	rid.Port = 37386
	rid.UniqueNumber = 6360
	got := resumeSelectorOpts(rid)
	if got.Runner == "" {
		t.Fatalf("non-zero AssignedTo: want a Runner pin, got empty SelectorOpts")
	}
	if got.Host != "" || got.IP != "" {
		t.Errorf("expected only Runner set, got %+v", got)
	}
	// Round-trips back through the same selector-building path used by
	// --runner on the CLI (cli/selector.go: buildRunnerIDSelector).
	if _, err := cli.BuildSelector(got); err != nil {
		t.Errorf("BuildSelector(%+v): %v", got, err)
	}
}
