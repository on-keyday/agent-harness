package runner

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func mustParseCID(t *testing.T, s string) objproto.ConnectionID {
	t.Helper()
	cid, err := objproto.ParseConnectionID(s, 0)
	if err != nil {
		t.Fatal(err)
	}
	return cid
}

func envMap(env []string) map[string]string {
	out := make(map[string]string)
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 {
			out[e[:i]] = e[i+1:]
		}
	}
	return out
}

func TestBuildAgentEnv_AllFields(t *testing.T) {
	var taskID protocol.TaskID
	copy(taskID.Id[:], []byte{0xde, 0xad, 0xbe, 0xef})
	var ticket [16]byte
	copy(ticket[:], []byte{0xfe, 0xed, 0xfa, 0xce})

	spec := AgentEnvSpec{
		ServerCID:  mustParseCID(t, "ws:127.0.0.1:8539-12345"),
		RunnerID:   mustParseCID(t, "ws:1.2.3.4:9999-42"),
		TaskID:     taskID,
		RepoPath:   "/home/u/repo",
		Hostname:   "dev-pi-01",
		WSPath:     "/ws",
		AuthTicket: ticket,
	}
	env := BuildAgentEnv(spec)
	want := map[string]string{
		"HARNESS_SERVER_CID":  spec.ServerCID.String(),
		"HARNESS_RUNNER_ID":   spec.RunnerID.String(),
		"HARNESS_TASK_ID":     hex.EncodeToString(taskID.Id[:]),
		"HARNESS_REPO_PATH":   "/home/u/repo",
		"HARNESS_HOSTNAME":    "dev-pi-01",
		"HARNESS_WS_PATH":     "/ws",
		"HARNESS_AUTH_TICKET": hex.EncodeToString(ticket[:]),
	}
	got := envMap(env)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestBuildAgentEnv_OmitsEmptyHostname(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
		WSPath:    "/ws",
	}
	env := BuildAgentEnv(spec)
	for _, e := range env {
		if strings.HasPrefix(e, "HARNESS_HOSTNAME=") {
			t.Errorf("hostname should be omitted when empty, got %q", e)
		}
	}
}
