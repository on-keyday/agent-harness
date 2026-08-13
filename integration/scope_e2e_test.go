//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// scopeEnv starts an in-process server + runner and returns the server CID.
func scopeEnv(t *testing.T, addr string) (context.Context, objproto.ConnectionID, string) {
	t.Helper()
	clearAgentEnv(t)
	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	go func() { _ = s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	go func() {
		_ = runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)
	return ctx, peerCID, repo
}

// lsRow returns the `ls --json` task row for taskID. The document is one
// object with runners/tasks arrays, not JSON Lines.
func lsRow(t *testing.T, ctx context.Context, c *cli.Client, taskID string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := c.ListJSON(ctx, &buf); err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	var doc struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("ls --json decode: %v\n%s", err, buf.String())
	}
	for _, row := range doc.Tasks {
		if id, _ := row["id"].(string); id == taskID {
			return row
		}
	}
	t.Fatalf("task %s not in ls --json output:\n%s", taskID, buf.String())
	return nil
}

// A scope survives the spawn round trip and shows up in ls, and `caps set`
// rewrites a task's authority over the real wire with nothing restarted —
// which is the whole point of the RPC: Resume, the only previous writer,
// requires a terminal target.
func TestScopeRoundTripAndLiveSetCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	ctx, peerCID, repo := scopeEnv(t, "127.0.0.1:18561")

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	scope, err := cli.ParseScope("none")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := c.Submit(ctx, repo, "scoped", cli.SessionOpts{
		Caps:  cli.CapsPtr(protocol.Capability_Spawn | protocol.Capability_FileRead),
		Scope: scope,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	row := lsRow(t, ctx, c, taskID)
	if got, _ := row["scope"].(string); got != "none" {
		t.Fatalf("ls scope = %q, want none — the --scope did not survive the spawn", got)
	}
	if got, _ := row["caps"].(string); got != "spawn,file_read" {
		t.Fatalf("ls caps = %q, want spawn,file_read", got)
	}

	// Live re-grant: narrow the caps and widen the scope in one call.
	global, err := cli.ParseScope("global")
	if err != nil {
		t.Fatal(err)
	}
	res, err := cli.SetCapsWith(ctx, c, cli.SetCapsOpts{
		TaskID: taskID,
		Caps:   cli.CapsPtr(protocol.Capability_FileRead),
		Scope:  &global,
	})
	if err != nil {
		t.Fatalf("caps set: %v", err)
	}
	if len(res.Affected) != 1 || res.Affected[0] != taskID {
		t.Fatalf("affected = %v, want [%s]", res.Affected, taskID)
	}

	row = lsRow(t, ctx, c, taskID)
	if got, _ := row["caps"].(string); got != "file_read" {
		t.Fatalf("caps after set = %q, want file_read", got)
	}
	if got, _ := row["scope"].(string); got != "global" {
		t.Fatalf("scope after set = %q, want global", got)
	}
	t.Logf("task %s re-granted live: caps=file_read scope=global", taskID)
}

// An unpermitted scope is reported, not silently clamped. The operator here is
// unrestricted, so the rejection has to come from an id that exists nowhere.
func TestScopeGrammarErrorsReachTheClient(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	ctx, peerCID, repo := scopeEnv(t, "127.0.0.1:18562")

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// A bad grammar never leaves the client.
	if _, err := cli.ParseScope("global+ids:" + strings.Repeat("ab", 16)); err == nil {
		t.Error("ids under global parsed; the server would have dropped them silently")
	}

	// A well-formed scope naming a real task is accepted from an operator,
	// whose own set is unrestricted.
	first, err := c.Submit(ctx, repo, "first", cli.SessionOpts{})
	if err != nil {
		t.Fatalf("submit first: %v", err)
	}
	sc, err := cli.ParseScope("ids:" + first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Submit(ctx, repo, "second", cli.SessionOpts{Scope: sc})
	if err != nil {
		t.Fatalf("submit with ids scope: %v", err)
	}
	row := lsRow(t, ctx, c, second)
	if got, _ := row["scope"].(string); got != "ids:"+first {
		t.Fatalf("scope = %q, want ids:%s", got, first)
	}
}
