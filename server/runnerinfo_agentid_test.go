package server

import (
	"testing"

	"github.com/on-keyday/objtrsf/objproto"
)

func TestToRunnerInfoCarriesAgentIdentity(t *testing.T) {
	e := RunnerEntry{
		ID:             "ws:127.0.0.1:1-2",
		Hostname:       "h",
		AgentBin:       "gemini",
		SkillsInjected: true,
		ActiveTasks:    map[string]struct{}{},
		Conn:           &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:1-2")},
	}
	info := toRunnerInfo(e)
	if got := string(info.AgentBin); got != "gemini" {
		t.Errorf("AgentBin=%q want gemini", got)
	}
	if !info.SkillsInjected() {
		t.Error("SkillsInjected=false want true")
	}
}
