//go:build !js

package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

var updateMCPGolden = flag.Bool("update-mcp", false, "rewrite testdata/mcp_tools_golden.json")

// TestMCPToolsGolden pins the whole tool surface. A table edit that changes
// what an agent can call is visible as a diff, not as a surprise at runtime.
func TestMCPToolsGolden(t *testing.T) {
	got, err := json.MarshalIndent(listTools(false), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "mcp_tools_golden.json")
	if *updateMCPGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run with -update-mcp to create it): %v", path, err)
	}
	if string(append(got, '\n')) != string(want) {
		t.Errorf("the MCP tool surface changed:\n got: %s\nwant: %s", got, want)
	}
}

// TestMCPToolNamesAreUnique is the build property the table cannot express: a
// duplicate name would make tools/call ambiguous.
func TestMCPToolNamesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, spec := range verb.Verbs {
		if spec.MCP == nil {
			continue
		}
		name := toolName(spec)
		if prev, ok := seen[name]; ok {
			t.Errorf("tool name %q is used by both %s and %s", name, prev, spec.FlagSetName())
		}
		seen[name] = spec.FlagSetName()
	}
}

// TestMCPNeverExposesItself: the server is not a tool (D9).
func TestMCPNeverExposesItself(t *testing.T) {
	for _, tool := range listTools(false) {
		if tool["name"] == "mcp" {
			t.Fatalf("mcp is a tool: %v", tool)
		}
	}
}

// TestMCPExcludesInteractiveVerbs: the verbs that stream, serve until
// interrupted or own the terminal are excluded by declaration (spec §6), so
// this asserts the declaration did not grow into them.
func TestMCPExcludesInteractiveVerbs(t *testing.T) {
	excluded := []string{
		"watch", "notify-watch", "exec tap", "forward tap", "session stream attach",
		"forward", "ssh-gateway", "session attach", "file edit", "file new",
		"exec", "lifecycle", "mcp",
	}
	for _, name := range excluded {
		spec, ok := verb.Lookup(strings.Fields(name)...)
		if !ok {
			t.Fatalf("%s: no such verb", name)
		}
		if spec.MCP != nil {
			t.Errorf("%s is declared as an MCP tool but is interactive/streaming", name)
		}
	}
}

// TestMCPWaitingVerbsAreTools: a verb that waits and then answers once is a
// tool since calls overlap and can be cancelled (spec D18, D19). The long
// polls are read tools whose durable sinks are write flags (D17).
func TestMCPWaitingVerbsAreTools(t *testing.T) {
	cases := []struct {
		verb       string
		effect     verb.MCPEffect
		writeFlags []string
	}{
		{"session exec", verb.MCPWrite, nil},
		{"agent dispatch", verb.MCPWrite, nil},
		{"agent wait", verb.MCPRead, nil},
		{"session await-idle", verb.MCPRead, []string{"notify", "topic"}},
		{"await-progress", verb.MCPRead, []string{"notify", "topic"}},
	}
	for _, c := range cases {
		spec, ok := verb.Lookup(strings.Fields(c.verb)...)
		if !ok {
			t.Fatalf("%s: no such verb", c.verb)
		}
		if spec.MCP == nil {
			t.Errorf("%s is not a tool (MCPNotATool %q)", c.verb, spec.MCPNotATool)
			continue
		}
		if spec.MCP.SideEffect != c.effect {
			t.Errorf("%s: side effect %q, want %q", c.verb, spec.MCP.SideEffect, c.effect)
		}
		if strings.Join(spec.MCP.WriteFlags, ",") != strings.Join(c.writeFlags, ",") {
			t.Errorf("%s: write flags %v, want %v", c.verb, spec.MCP.WriteFlags, c.writeFlags)
		}
	}
}

// TestMCPReadOnlyDropsWriteTools: --read-only filters on the declaration.
func TestMCPReadOnlyDropsWriteTools(t *testing.T) {
	names := func(tools []map[string]any) map[string]bool {
		m := map[string]bool{}
		for _, tool := range tools {
			m[tool["name"].(string)] = true
		}
		return m
	}
	full := names(listTools(false))
	ro := names(listTools(true))
	if !full["session_rename"] {
		t.Fatal("session_rename is not a tool at all")
	}
	if ro["session_rename"] {
		t.Error("--read-only still lists the write tool session_rename")
	}
	if !ro["version"] {
		t.Error("--read-only dropped the read tool version")
	}
}

// TestMCPArgvRoundTrip: schema → argv → Parse → Action must equal the Action a
// hand-typed command line produces. This is gate V3.
func TestMCPArgvRoundTrip(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		argv []string // the hand-typed command line after the path
	}{
		{"version", map[string]any{"json": true}, []string{"--json"}},
		{"version", map[string]any{}, nil},
		{"caps", map[string]any{"json": true}, []string{"--json"}},
		{"transcript_ls", map[string]any{"task-id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{"session_rename", map[string]any{"task-id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "name": "x"}, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "x"}},
	}
	for _, c := range cases {
		spec, ok := lookupTool(c.tool)
		if !ok {
			t.Fatalf("%s: not a tool", c.tool)
		}
		gotArgv, err := buildArgv(spec, c.args, false)
		if err != nil {
			t.Fatalf("%s: buildArgv: %v", c.tool, err)
		}
		gotAct, handled, err := verb.ParseCLICommand(append(append([]string{}, spec.Path...), gotArgv...), nil)
		if err != nil || !handled {
			t.Fatalf("%s: rebuilt argv %v: handled=%t err=%v", c.tool, gotArgv, handled, err)
		}
		wantAct, handled, err := verb.ParseCLICommand(append(append([]string{}, spec.Path...), c.argv...), nil)
		if err != nil || !handled {
			t.Fatalf("%s: typed argv %v: handled=%t err=%v", c.tool, c.argv, handled, err)
		}
		if got, want := mustJSON(t, gotAct), mustJSON(t, wantAct); got != want {
			t.Errorf("%s: rebuilt %v and typed %v produced different Actions:\n got %s\nwant %s", c.tool, gotArgv, c.argv, got, want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMCPStdioSession drives the loop over pipes: initialize, initialized,
// tools/list, a tools/call and a malformed frame. stdout must be JSON-RPC only
// (pipeSession fails on a line that is not).
func TestMCPStdioSession(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"version","arguments":{"json":true}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`not json at all`,
		`{"jsonrpc":"2.0","id":5,"method":"ping"}`,
	}, "\n") + "\n"

	// The input stays open until every answer is in: end of input is the
	// client's shutdown, and a call still in flight then is not answered
	// (spec §11 2c).
	p := startPipeSession(t, &mcpServer{h: cliVerbs{}})
	for _, l := range strings.Split(strings.TrimSpace(in), "\n") {
		p.send(l)
	}
	var responses []map[string]any
	for len(responses) < 6 {
		m := p.next()
		if m["jsonrpc"] != "2.0" {
			t.Fatalf("missing jsonrpc 2.0: %v", m)
		}
		responses = append(responses, m)
	}
	p.end()
	if err := <-p.done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	for r := range p.resps {
		responses = append(responses, r)
	}

	// initialize, tools/list, tools/call version, tools/call nope, parse error,
	// ping. Calls run concurrently (spec D18), so responses are matched by id;
	// the parse error has none.
	if len(responses) != 6 {
		t.Fatalf("got %d responses, want 6: %v", len(responses), responses)
	}
	byID := map[float64]map[string]any{}
	var noID []map[string]any
	for _, r := range responses {
		if id, ok := r["id"].(float64); ok {
			byID[id] = r
		} else {
			noID = append(noID, r)
		}
	}
	if _, ok := byID[1]["result"].(map[string]any); !ok {
		t.Errorf("initialize result missing: %v", byID[1])
	}
	if _, ok := byID[2]["result"].(map[string]any); !ok {
		t.Errorf("tools/list result missing: %v", byID[2])
	}
	if res, ok := byID[3]["result"].(map[string]any); !ok {
		t.Errorf("tools/call result missing: %v", byID[3])
	} else if res["isError"] == true {
		t.Errorf("tools/call version failed: %v", res)
	}
	// An unknown tool is a JSON-RPC error, not a tool result (spec D15).
	if _, ok := byID[4]["error"].(map[string]any); !ok {
		t.Errorf("unknown tool did not come back as a JSON-RPC error: %v", byID[4])
	}
	if len(noID) != 1 {
		t.Fatalf("want exactly one id-less response (the parse error), got %v", noID)
	}
	if _, ok := noID[0]["error"].(map[string]any); !ok {
		t.Errorf("malformed frame did not produce a JSON-RPC error: %v", noID[0])
	}
	if _, ok := byID[5]["result"].(map[string]any); !ok {
		t.Errorf("ping result missing: %v", byID[5])
	}
}

// TestMCPReadOnlyRefusesWriteTool: the filter is enforced at call time too,
// as a JSON-RPC error, and the body does not run (D12, D15).
func TestMCPReadOnlyRefusesWriteTool(t *testing.T) {
	srv := &mcpServer{h: cliVerbs{}, readOnly: true}
	_, rpcErr := resolveTool(srv.filter(), "session_rename")
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "read-only") {
		t.Fatalf("--read-only: %v, want a JSON-RPC error naming --read-only", rpcErr)
	}
}

// TestMCPUnknownArgumentIsRefused: a typo must not silently become a default.
func TestMCPUnknownArgumentIsRefused(t *testing.T) {
	spec, _ := lookupTool("version")
	if _, err := buildArgv(spec, map[string]any{"jason": true}, false); err == nil {
		t.Fatal("unknown argument was accepted")
	}
}

// TestMCPToolSchemasAreWellFormed: every declared tool has the three fields a
// client needs, and its schema is an object.
func TestMCPToolSchemasAreWellFormed(t *testing.T) {
	for _, tool := range listTools(false) {
		name, _ := tool["name"].(string)
		if name == "" {
			t.Errorf("tool without a name: %v", tool)
			continue
		}
		if d, _ := tool["description"].(string); d == "" {
			t.Errorf("%s: no description", name)
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("%s: inputSchema is not an object schema: %v", name, tool["inputSchema"])
		}
		if _, ok := tool["annotations"].(map[string]any); !ok {
			t.Errorf("%s: no annotations", name)
		}
	}
}
