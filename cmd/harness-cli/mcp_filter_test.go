//go:build !js

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func toolNames(tools []map[string]any) map[string]bool {
	m := map[string]bool{}
	for _, tool := range tools {
		m[tool["name"].(string)] = true
	}
	return m
}

// The caller-caps half of tools/list (spec §8.3): a tool is shown when SOME
// call of it could pass. Its errors must land on the shown side.
func TestMCPHoldFilter(t *testing.T) {
	none := toolNames(listToolsWith(toolFilter{checkHeld: true, held: 0}))
	for _, shown := range []string{"agent_send", "version", "ls", "whoami", "session_ls"} {
		if !none[shown] {
			t.Errorf("held=none: %s is hidden, but a call of it needs no bit", shown)
		}
	}
	for _, hidden := range []string{"cancel", "file_ls", "record_ls"} {
		if none[hidden] {
			t.Errorf("held=none: %s is listed, but every call of it needs a bit", hidden)
		}
	}
	w := toolNames(listToolsWith(toolFilter{checkHeld: true, held: protocol.Capability_FileWrite}))
	if !w["file_ls"] || !w["file_push"] {
		t.Error("held=file_write: file_ls / file_push hidden, but file_write satisfies them")
	}
	c := toolNames(listToolsWith(toolFilter{checkHeld: true, held: protocol.Capability_Cancel}))
	if !c["cancel"] {
		t.Error("held=cancel: cancel hidden")
	}
}

// --caps narrows past the caller's caps, never widens.
func TestMCPCapsFlagNarrows(t *testing.T) {
	got := toolNames(listToolsWith(toolFilter{checkHeld: true, held: protocol.Capability_All & protocol.Capability_FileRead}))
	if !got["file_pull"] || !got["version"] {
		t.Error("--caps file_read: file_pull or version hidden")
	}
	if got["cancel"] {
		t.Error("--caps file_read: cancel listed")
	}
	if h := heldFor(protocol.Capability_Cancel, ptrCap(protocol.Capability_FileRead)); h != 0 {
		t.Errorf("caller holds cancel, --caps file_read: held %v, want none (--caps never widens)", h)
	}
}

func ptrCap(c protocol.Capability) *protocol.Capability { return &c }

// rpcSession feeds lines and ends the input at once, so it sees only what the
// reading loop answers itself -- initialize, ping, tools/list, a refused call.
// An admitted call still in flight at end of input is not answered (spec §11
// 2c); drive those with startPipeSession.
func rpcSession(t *testing.T, srv *mcpServer, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := srv.serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout is not JSON-RPC: %q", l)
		}
		resps = append(resps, m)
	}
	return resps
}

// A tool this server withheld, or one it never had, is a JSON-RPC error and
// its body does not run (spec D15).
func TestMCPWithheldToolIsRPCError(t *testing.T) {
	call := func(id int, name, args string) string {
		return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	}
	ro := &mcpServer{h: cliVerbs{}, readOnly: true}
	caps := &mcpServer{h: cliVerbs{}, caps: ptrCap(protocol.Capability_FileRead),
		held: func() (protocol.Capability, error) { return protocol.Capability_All, nil }}
	for name, c := range map[string]struct {
		srv  *mcpServer
		line string
		want string
	}{
		"read-only write tool": {ro, call(1, "session_rename", `{"task-id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"x"}`), "--read-only"},
		"caps-filtered tool":   {caps, call(2, "cancel", `{"task-id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), "--caps"},
		"unknown tool":         {ro, call(3, "nope", `{}`), "unknown tool"},
		"not-a-tool verb":      {ro, call(4, "session_attach", `{}`), "unknown tool"},
	} {
		resps := rpcSession(t, c.srv, `{"jsonrpc":"2.0","id":0,"method":"tools/list"}`, c.line)
		e, ok := resps[len(resps)-1]["error"].(map[string]any)
		if !ok {
			t.Errorf("%s: %v, want a JSON-RPC error", name, resps[len(resps)-1])
			continue
		}
		if e["code"].(float64) != -32602 || !strings.Contains(e["message"].(string), c.want) {
			t.Errorf("%s: error %v, want -32602 naming %q", name, e, c.want)
		}
	}
}

func mustTool(t *testing.T, name string) verb.VerbSpec {
	t.Helper()
	sp, ok := lookupTool(name)
	if !ok {
		t.Fatalf("%s: not a tool", name)
	}
	return sp
}

func parseArgv(t *testing.T, sp verb.VerbSpec, argv []string) (verb.Action, error) {
	t.Helper()
	act, handled, err := verb.ParseCLICommand(append(append([]string{}, sp.Path...), argv...), nil)
	if !handled {
		t.Fatalf("%s %v: not handled", sp.FlagSetName(), argv)
	}
	return act, err
}

// A value that begins with '-' is a value, not a flag (spec §5.3).
func TestMCPTrailingAndDashValues(t *testing.T) {
	send := mustTool(t, "agent_send")
	argv, err := buildArgv(send, map[string]any{"topic": "chat.abcd1234", "text": "--force"}, false)
	if err != nil {
		t.Fatal(err)
	}
	act, err := parseArgv(t, send, argv)
	if err != nil || !strings.Contains(mustJSON(t, act), `"--force"`) {
		t.Errorf("agent_send text --force: argv %v gave %s / %v", argv, mustJSON(t, act), err)
	}
	read := mustTool(t, "board_read")
	argv, err = buildArgv(read, map[string]any{"topic": "-x"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if act, err := parseArgv(t, read, argv); err != nil || !strings.Contains(mustJSON(t, act), `"-x"`) {
		t.Errorf("board_read topic -x: argv %v gave %v / %v", argv, act, err)
	}
	if _, err := buildArgv(mustTool(t, "git_log"), map[string]any{"task-id": strings.Repeat("a", 32), "revision": "-x"}, false); err == nil {
		t.Error("git_log revision -x: accepted, but a pathspec verb has no '--' to protect it with")
	}
}

// session new through MCP is always -d: without it the session would take the
// server's terminal (spec §6).
func TestMCPSessionNewIsDetached(t *testing.T) {
	sp := mustTool(t, "session_new")
	argv, err := buildArgv(sp, map[string]any{"repo": "/r"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) == 0 || argv[0] != "--detach" {
		t.Errorf("session_new argv %v does not start with --detach", argv)
	}
	for _, omitted := range []string{"x11", "detach", "x11-display"} {
		if _, err := buildArgv(sp, map[string]any{"repo": "/r", omitted: true}, false); err == nil {
			t.Errorf("session_new accepted %s, which its tool omits", omitted)
		}
	}
	props := toolFromSpec(sp, false)["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["x11"]; ok {
		t.Error("session_new's schema advertises x11")
	}
}

// A flag that makes a read verb write (spec D17).
func TestMCPWriteFlags(t *testing.T) {
	sp := mustTool(t, "record_get")
	ro := toolFromSpec(sp, true)
	if _, ok := ro["inputSchema"].(map[string]any)["properties"].(map[string]any)["o"]; ok {
		t.Error("--read-only record_get advertises -o")
	}
	if ro["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Error("--read-only record_get is not read-only")
	}
	if _, err := buildArgv(sp, map[string]any{"record": "x", "o": "/tmp/f"}, true); err == nil {
		t.Error("--read-only accepted record_get -o")
	}
	rw := toolFromSpec(sp, false)
	if _, ok := rw["inputSchema"].(map[string]any)["properties"].(map[string]any)["o"]; !ok {
		t.Error("record_get without --read-only lost -o")
	}
	if rw["annotations"].(map[string]any)["readOnlyHint"] != false {
		t.Error("record_get with -o available claims read-only")
	}
	if !toolNames(listToolsWith(toolFilter{readOnly: true}))["record_get"] {
		t.Error("--read-only dropped record_get altogether")
	}
}

// Gate V3 over the whole surface: for every tool, the argv rebuilt from the
// smallest arguments its schema requires parses exactly as the command line a
// person would type for the same values (the same Action, or the same error).
func TestMCPArgvRoundTripEveryTool(t *testing.T) {
	for _, tool := range listTools(false) {
		name := tool["name"].(string)
		sp := mustTool(t, name)
		schema := tool["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		args := map[string]any{}
		var typed []string
		typed = append(typed, sp.MCP.Fixed...)
		req, _ := schema["required"].([]string)
		need := map[string]bool{}
		for _, r := range req {
			need[r] = true
		}
		value := func(key string) any {
			p := props[key].(map[string]any)
			if p["type"] == "array" {
				return []any{sampleScalar(p["items"].(map[string]any))}
			}
			return sampleScalar(p)
		}
		c := sp.For(verb.CLI)
		for _, a := range c.Args {
			if !need[a.Name] {
				continue
			}
			v := value(a.Name)
			args[a.Name] = v
			typed = append(typed, tokensOf(v)...)
		}
		if c.Trailing != nil && need[c.Trailing.Name] {
			v := value(c.Trailing.Name)
			args[c.Trailing.Name] = v
			typed = append(typed, tokensOf(v)...)
		}
		rebuilt, err := buildArgv(sp, args, false)
		if err != nil {
			t.Errorf("%s: buildArgv(%v): %v", name, args, err)
			continue
		}
		gotAct, gotErr := parseArgv(t, sp, rebuilt)
		wantAct, wantErr := parseArgv(t, sp, typed)
		if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) ||
			(gotErr == nil && mustJSON(t, gotAct) != mustJSON(t, wantAct)) {
			t.Errorf("%s: rebuilt %v and typed %v differ:\n got %v %v\nwant %v %v", name, rebuilt, typed, gotAct, gotErr, wantAct, wantErr)
		}
	}
}

func sampleScalar(p map[string]any) any {
	if e, ok := p["enum"].([]string); ok && len(e) > 0 {
		return e[0]
	}
	switch p["type"] {
	case "integer":
		return json.Number("1")
	case "boolean":
		return true
	}
	if p["pattern"] != nil {
		return strings.Repeat("a", 32)
	}
	return "chat.abcd1234"
}

func tokensOf(v any) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, tokensOf(e)...)
		}
		return out
	case json.Number:
		return []string{t.String()}
	case string:
		return []string{t}
	}
	return nil
}

// The server CID is resolved once, before serving: resolving it per call could
// die (a bad value, or a DNS failure) in the middle of a session.
func TestMCPPinsTheServerCID(t *testing.T) {
	calls := 0
	h := cliVerbs{cid: func() objproto.ConnectionID {
		calls++
		return objproto.MustParseConnectionID("ws:127.0.0.1:9-1")
	}}
	p := pinCID(h)
	if calls != 1 {
		t.Fatalf("pinCID resolved %d times, want once", calls)
	}
	for i := 0; i < 3; i++ {
		_ = p.cid()
	}
	if calls != 1 {
		t.Errorf("the pinned cid resolved again (%d calls)", calls)
	}
}

// A tool result is the body's stdout, then its stderr (spec D20). Binary
// output would be mangled into U+FFFD and an unbounded one could be any size,
// so both are shaped here.
func TestMCPToolResultShape(t *testing.T) {
	same := func(got, want []string) bool {
		return strings.Join(got, "\x00") == strings.Join(want, "\x00") && len(got) == len(want)
	}
	if b, isErr := toolResultBlocks([]byte("hello\n"), nil, nil); !same(b, []string{"hello\n"}) || isErr {
		t.Errorf("plain output: %q %t", b, isErr)
	}
	bin := []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0xfe, 'a'}
	if b, isErr := toolResultBlocks(bin, nil, nil); !isErr || len(b) != 1 || !strings.Contains(b[0], "-o") || strings.ContainsRune(b[0], 0xFFFD) {
		t.Errorf("binary output: %q %t, want an error pointing at -o and no replacement characters", b, isErr)
	}
	big := []byte(strings.Repeat("x", maxToolResult+10))
	if b, isErr := toolResultBlocks(big, nil, nil); isErr || len(b) != 1 || len(b[0]) > maxToolResult+200 || !strings.Contains(b[0], "truncated") {
		t.Errorf("oversized output: %d blocks, isError=%t, want one cut at %d with a notice", len(b), isErr, maxToolResult)
	}
	// A bare exit code adds nothing: the result reads as the typed command's
	// stdout, and a JSON result stays one JSON value.
	if b, isErr := toolResultBlocks([]byte("partial"), nil, exitCodeError{code: 1}); !same(b, []string{"partial"}) || !isErr {
		t.Errorf("exit-code error with printed output: %q %t", b, isErr)
	}
	js := `{"status":"session_stopped"}` + "\n"
	if b, isErr := toolResultBlocks([]byte(js), nil, exitCodeError{code: 3}); !same(b, []string{js}) || !isErr {
		t.Errorf("exit-code error after JSON: %q %t", b, isErr)
	}
	// The error text is what the command line prints on stderr, so it ends
	// the stderr block.
	if b, isErr := toolResultBlocks(nil, nil, exitCodeError{code: 3, msg: "ambiguous"}); !same(b, []string{"ambiguous"}) || !isErr {
		t.Errorf("exit-code error with a message: %q %t", b, isErr)
	}
	if b, isErr := toolResultBlocks([]byte("out\n"), []byte("session send: 5 bytes\n"), nil); !same(b, []string{"out\n", "session send: 5 bytes\n"}) || isErr {
		t.Errorf("stdout and stderr: %q %t, want two blocks, stdout first", b, isErr)
	}
	if b, isErr := toolResultBlocks(nil, []byte("board read: topic \"x\" holds no messages\n"), nil); !same(b, []string{"board read: topic \"x\" holds no messages\n"}) || isErr {
		t.Errorf("stderr only: %q %t", b, isErr)
	}
	if b, isErr := toolResultBlocks([]byte(js), []byte("warn\n"), errors.New("boom")); !same(b, []string{js, "warn\nboom"}) || !isErr {
		t.Errorf("JSON, a warning and an error: %q %t, want the JSON block intact", b, isErr)
	}
}
