//go:build !js

package cli_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Driven through RunBoardSubcmd, the argv parser the binary uses.
func TestBoardMembershipCLI(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	newTask := func() string {
		id := srv.Tasks().Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "bash")
		srv.Board().Attach(protocol.RunnerID{}, hexTask(t, id), "h", "bash")
		return id
	}
	task, other := newTask(), newTask()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(sub string, args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := cli.RunBoardSubcmd(ctx, peerCID, sub, args, &out)
		return out.String(), err
	}
	subsLine := func() string {
		t.Helper()
		out, err := run("subscribers")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, task) {
				return l
			}
		}
		t.Fatalf("no subscribers line for %s in:\n%s", task, out)
		return ""
	}

	out, err := run("join", task, "grp")
	if err != nil || !strings.Contains(out, `"status":"ok"`) || !strings.Contains(out, `"op":"join"`) || !strings.Contains(out, `"task":"`+task[:8]+`"`) {
		t.Fatalf("join: %q, %v", out, err)
	}
	if l := subsLine(); !strings.Contains(l, "joined:grp(") {
		t.Errorf("subscribers after join = %q, want joined:grp(...)", l)
	}
	out, err = run("subscribers", "--json")
	if err != nil || !strings.Contains(out, `"joined":true`) {
		t.Errorf("subscribers --json after join: %q, %v", out, err)
	}
	if _, err := run("leave", task, "grp"); err != nil {
		t.Fatal(err)
	}
	if l := subsLine(); strings.Contains(l, "grp") {
		t.Errorf("subscribers after leave still lists grp: %q", l)
	}

	// A flag after the positionals is parsed as a flag, not taken as an argument.
	if _, err := run("join", task, "grp", "--bogus"); err == nil {
		t.Error("join with an unknown trailing flag was accepted")
	}
	if srv.Board().IsMember(hexTask(t, task), "grp") {
		t.Error("a rejected join still changed the board")
	}
	if _, err := run("subscribe", task, "plain"); err != nil {
		t.Fatal(err)
	}
	if l := subsLine(); !strings.Contains(l, "plain(") || strings.Contains(l, "joined:plain") {
		t.Errorf("subscribers after subscribe = %q, want plain without joined:", l)
	}
	if _, err := run("unsubscribe", task, "plain"); err != nil {
		t.Fatal(err)
	}

	if _, err := run("subscribe", task, agentboard.SelfTopic(hexTask(t, other))); err == nil || !strings.Contains(err.Error(), "not_readable") {
		t.Errorf("subscribe to another task's chat: err = %v, want not_readable", err)
	}
}

func hexTask(t *testing.T, id string) protocol.TaskID {
	t.Helper()
	var tid protocol.TaskID
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != len(tid.Id) {
		t.Fatalf("task id %q: %v", id, err)
	}
	copy(tid.Id[:], raw)
	return tid
}
