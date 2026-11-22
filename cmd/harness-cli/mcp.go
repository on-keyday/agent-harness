//go:build !js

package main

// The MCP consumer: `harness-cli mcp` serves the control plane as Model
// Context Protocol tools over stdio (docs/superpowers/specs/
// 2026-11-22-harness-cli-mcp-surface-design.md).
//
// It is a CONSUMER of the declaration in cli/verb/table.go, not a second
// implementation. tools/list is generated from the same VerbSpec rows the
// command line uses; tools/call synthesizes argv and runs the SAME parser and
// the SAME handler the command line runs. The only things this file owns are
// the wire (JSON-RPC framing), the tool schema, the argv reconstruction and the
// list filters.
//
// Authority is unchanged. A tool runs as the connection that started this
// process, so the server's caps are the boundary, exactly as they are for a
// typed command. The filters change what is LISTED, never what is allowed.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/buildinfo"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// mcpProtocolVersion is the revision this server speaks. initialize echoes the
// client's requested version when it sends one, so a newer client is not
// refused over a version this server does not track.
const mcpProtocolVersion = "2025-06-18"

// JSON-RPC error codes (the ones this server can produce).
const (
	rpcParseError     = -32700
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// mcpRequest is one JSON-RPC message. ID is raw so a notification (no id) is
// distinguishable from a request whose id is 0 or "".
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

// mcpServer is one `harness-cli mcp` process: the handler the command line
// uses, and the filters the operator asked for.
type mcpServer struct {
	h        cliVerbs
	readOnly bool
	// caps is --caps: nil when not given.
	caps *protocol.Capability
	// held answers which capabilities this connection holds (whoami). Nil
	// skips the caller-caps half of the filter.
	held func() (protocol.Capability, error)

	// listed is the filter the last tools/list applied; tools/call refuses a
	// tool it would not have listed (spec D15). Only the reading loop touches
	// it.
	listed    toolFilter
	hasListed bool

	// run executes one admitted tools/call under its own context; nil is
	// runTool. A test injects one to hold a call open.
	run func(ctx context.Context, f toolFilter, spec verb.VerbSpec, args json.RawMessage) (blocks []string, isErr bool)
}

// serveMCP is the verb body. It reads requests from the process's stdin and
// writes responses to its stdout, which is the JSON-RPC channel (D11).
func serveMCP(h cliVerbs, readOnly bool, caps *protocol.Capability) error {
	h = pinCID(h)
	srv := &mcpServer{h: h, readOnly: readOnly, caps: caps, held: func() (protocol.Capability, error) {
		resp, err := cli.WhoAmI(h.ctx, h.cid())
		if err != nil {
			return 0, err
		}
		if resp.PrincipalTaskId.Id == ([16]byte{}) {
			return protocol.Capability_All, nil // the operator holds every bit
		}
		return resp.Capabilities, nil
	}}
	return srv.serve(os.Stdin, os.Stdout)
}

// pinCID resolves the server ConnectionID once and hands back a handler that
// returns that value. The command line's resolver dies on a bad value or a
// failed DNS lookup, which in one tool call would end the server and every
// call after it; resolved here, before initialize, a bad value fails the
// server's start instead.
func pinCID(h cliVerbs) cliVerbs {
	cid := h.cid()
	h.cid = func() objproto.ConnectionID { return cid }
	return h
}

// heldFor is what a filter may assume the caller can satisfy: its own caps,
// narrowed by --caps when given. --caps never widens past the caller.
func heldFor(caller protocol.Capability, caps *protocol.Capability) protocol.Capability {
	if caps != nil {
		return caller & *caps
	}
	return caller
}

// filter is what tools/list applies now. A whoami that fails leaves the
// caller-caps half off (logged to stderr): the filter saves context and
// authorizes nothing, so an unfiltered list is the safe side.
func (s *mcpServer) filter() toolFilter {
	f := toolFilter{readOnly: s.readOnly}
	caller, known := protocol.Capability_All, false
	if s.held != nil {
		if c, err := s.held(); err != nil {
			fmt.Fprintf(os.Stderr, "harness-cli mcp: whoami failed (%v); listing without the caller-caps filter\n", err)
		} else {
			caller, known = c, true
		}
	}
	if known || s.caps != nil {
		f.checkHeld = true
		f.held = heldFor(caller, s.caps)
	}
	return f
}

// serve is the loop with its streams injected, so a test drives it without a
// subprocess.
//
// The loop reads, and answers in arrival order, everything but a tool's body:
// initialize, ping, tools/list, and the admission of a tools/call (unknown or
// withheld → JSON-RPC error). An admitted call runs on its own goroutine under
// its own context, so a call that waits holds up nothing else (spec D18);
// notifications/cancelled cancels that context and its response is dropped.
// End of input is the client going away: what is in flight is cancelled,
// waited for and not answered.
func (s *mcpServer) serve(in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	var wmu sync.Mutex // one response per line, from any goroutine
	write := func(resp mcpResponse) {
		resp.JSONRPC = "2.0"
		wmu.Lock()
		_ = enc.Encode(resp) // the stream is the only report channel
		wmu.Unlock()
	}
	writeErr := func(id json.RawMessage, code int, msg string) {
		write(mcpResponse{ID: id, Error: &mcpError{Code: code, Message: msg}})
	}

	run := s.run
	if run == nil {
		run = s.runTool
	}
	base, stop := context.WithCancel(s.h.baseCtx())
	type flight struct {
		cancel    context.CancelFunc
		cancelled bool // by the client: its response is dropped
	}
	var fmu sync.Mutex
	inFlight := map[string]*flight{}
	var calls sync.WaitGroup
	defer func() {
		// End of input: the client is gone, and may have closed our stdout
		// too -- a write there would end the process by SIGPIPE before the
		// bodies close their connections. So what is in flight is withdrawn
		// as a cancellation withdraws it: cancelled, waited for, unanswered.
		fmu.Lock()
		for _, f := range inFlight {
			f.cancelled = true
		}
		fmu.Unlock()
		stop()
		calls.Wait()
	}()

	sc := bufio.NewScanner(in)
	// Tool arguments are small; a request line is bounded at 4 MiB so a
	// runaway client cannot make the server allocate without limit.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req mcpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			writeErr(nil, rpcParseError, "parse error: "+err.Error())
			continue
		}
		// A notification carries no id and gets no response, including on
		// error (JSON-RPC 2.0 §4.1).
		notification := len(req.ID) == 0

		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			ver := p.ProtocolVersion
			if ver == "" {
				ver = mcpProtocolVersion
			}
			write(mcpResponse{ID: req.ID, Result: map[string]any{
				"protocolVersion": ver,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{
					"name":    "harness-cli",
					"version": buildinfo.Read().Revision,
				},
			}})

		case "notifications/initialized":
			// One-way by definition.

		case "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &p) != nil {
				continue
			}
			fmu.Lock()
			if f := inFlight[idKey(p.RequestID)]; f != nil {
				f.cancelled = true
				f.cancel()
			}
			fmu.Unlock()

		case "ping":
			write(mcpResponse{ID: req.ID, Result: map[string]any{}})

		case "tools/list":
			s.listed, s.hasListed = s.filter(), true
			write(mcpResponse{ID: req.ID, Result: map[string]any{"tools": listToolsWith(s.listed)}})

		case "tools/call":
			if notification {
				continue
			}
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				writeErr(req.ID, rpcInvalidParams, "tools/call params: "+err.Error())
				continue
			}
			if !s.hasListed {
				s.listed, s.hasListed = s.filter(), true
			}
			spec, rpcErr := resolveTool(s.listed, p.Name)
			if rpcErr != nil {
				writeErr(req.ID, rpcErr.Code, rpcErr.Message)
				continue
			}
			ctx, cancel := context.WithCancel(base)
			key, f := idKey(req.ID), &flight{cancel: cancel}
			fmu.Lock()
			inFlight[key] = f
			fmu.Unlock()
			calls.Add(1)
			go func(id json.RawMessage, filter toolFilter, args json.RawMessage) {
				defer calls.Done()
				blocks, isErr := run(ctx, filter, spec, args)
				fmu.Lock()
				dropped := f.cancelled
				if inFlight[key] == f {
					delete(inFlight, key)
				}
				fmu.Unlock()
				cancel()
				if dropped {
					return // the client withdrew the request (MCP cancellation)
				}
				content := make([]map[string]any, 0, len(blocks))
				for _, b := range blocks {
					content = append(content, map[string]any{"type": "text", "text": b})
				}
				res := map[string]any{"content": content}
				if isErr {
					res["isError"] = true
				}
				write(mcpResponse{ID: id, Result: res})
			}(req.ID, s.listed, p.Arguments)

		default:
			if notification {
				continue
			}
			writeErr(req.ID, rpcMethodNotFound, "method not found: "+req.Method)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcp: read stdin: %w", err)
	}
	return nil
}

// idKey is a request id as a map key. A cancellation names the id it cancels
// with the same JSON value the request carried.
func idKey(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}

// toolFilter is what tools/list hides. readOnly drops write tools and the
// flags that make a read tool write; checkHeld hides a tool no call of which
// held can satisfy (spec §8).
type toolFilter struct {
	readOnly  bool
	checkHeld bool
	held      protocol.Capability
}

// withheld says why f hides spec, or "" when it does not.
func (f toolFilter) withheld(spec verb.VerbSpec) string {
	if f.readOnly && spec.MCP.SideEffect == verb.MCPWrite {
		return "refused by --read-only: it writes"
	}
	if f.checkHeld && !toolSatisfiable(spec, f.held) {
		return "withheld by --caps and this connection's capabilities: no call of it could pass with " + verb.CapsLabel(f.held)
	}
	return ""
}

// toolSatisfiable is spec §8.1's rule: a tool sending no kind always is; one
// sending kinds is when ANY of them has a request held can satisfy. Generous on
// purpose -- a hidden tool the caller could use fails silently.
func toolSatisfiable(spec verb.VerbSpec, held protocol.Capability) bool {
	if len(spec.Sends) == 0 {
		return true
	}
	for _, k := range spec.Sends {
		if authority.MaySatisfy(k, held) {
			return true
		}
	}
	return false
}

// tools are the verbs that are tools, in table order.
func tools() []verb.VerbSpec {
	var out []verb.VerbSpec
	for _, spec := range verb.Verbs {
		if spec.MCP != nil && spec.CmdlineSurfaces.Has(verb.CLI) {
			out = append(out, spec)
		}
	}
	return out
}

// listTools is the whole surface with only --read-only applied: what the
// golden pins.
func listTools(readOnly bool) []map[string]any {
	return listToolsWith(toolFilter{readOnly: readOnly})
}

func listToolsWith(f toolFilter) []map[string]any {
	out := make([]map[string]any, 0, 128)
	for _, spec := range tools() {
		if f.withheld(spec) != "" {
			continue
		}
		out = append(out, toolFromSpec(spec, f.readOnly))
	}
	return out
}

// schemaFlags are the flags a tool's schema carries: not the omitted ones,
// and not the write flags of a read-only server.
func schemaFlags(spec verb.VerbSpec, readOnly bool) []verb.Flag {
	drop := map[string]bool{}
	for _, n := range spec.MCP.Omit {
		drop[n] = true
	}
	if readOnly {
		for _, n := range spec.MCP.WriteFlags {
			drop[n] = true
		}
	}
	var out []verb.Flag
	for _, f := range spec.For(verb.CLI).Flags {
		if !drop[f.Name] {
			out = append(out, f)
		}
	}
	return out
}

// listTrailing reports whether a verb's trailing words are a word list (an
// argv) rather than one joined text.
func listTrailing(t *verb.Trailing) bool {
	return t.Field == "" && t.FieldArgs != ""
}

// toolFromSpec is the pure function declaration → tool object.
func toolFromSpec(spec verb.VerbSpec, readOnly bool) map[string]any {
	c := spec.For(verb.CLI)
	props := map[string]any{}
	var required []string

	for _, a := range c.Args {
		props[a.Name] = argSchema(a)
		if !a.Optional && !a.Variadic {
			required = append(required, a.Name)
		}
	}
	for _, f := range schemaFlags(spec, readOnly) {
		props[f.Name] = flagSchema(f)
	}
	if c.Trailing != nil {
		if listTrailing(c.Trailing) {
			props[c.Trailing.Name] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		} else {
			props[c.Trailing.Name] = map[string]any{"type": "string"}
		}
		if c.Trailing.Required && c.Trailing.UnlessFlag == "" {
			required = append(required, c.Trailing.Name)
		}
	}
	if c.Pathspec {
		props["pathspec"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}

	writes := spec.MCP.SideEffect == verb.MCPWrite || (len(spec.MCP.WriteFlags) > 0 && !readOnly)
	return map[string]any{
		"name":        toolName(spec),
		"description": toolDescription(spec),
		"inputSchema": schema,
		"annotations": map[string]any{"readOnlyHint": !writes, "destructiveHint": writes},
	}
}

// toolName is the path joined with "_". MCP tool names are [A-Za-z0-9_-]{1,64};
// a dot, which reads more naturally, is not in that set.
func toolName(spec verb.VerbSpec) string {
	return strings.Join(spec.Path, "_")
}

// toolDescription is the verb's first Note line: the same sentence the help
// shows first, so a tool and its command line describe themselves identically.
func toolDescription(spec verb.VerbSpec) string {
	if len(spec.Notes) > 0 {
		return spec.Notes[0]
	}
	return spec.FlagSetName()
}

func argSchema(a verb.Arg) map[string]any {
	var item map[string]any
	switch a.Type {
	case verb.ArgTaskID:
		item = map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}$"}
	case verb.ArgUint:
		item = map[string]any{"type": "integer", "minimum": 0}
	default:
		item = map[string]any{"type": "string"}
	}
	if len(a.OneOfArg) > 0 {
		item["enum"] = a.OneOfArg
	}
	if a.Variadic {
		return map[string]any{"type": "array", "items": item}
	}
	return item
}

func flagSchema(f verb.Flag) map[string]any {
	var s map[string]any
	switch {
	case f.Custom != nil:
		// The custom flags are repeatable (--agent-arg, --scope-for): one
		// value per occurrence.
		s = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case f.Type == verb.FlagBool:
		s = map[string]any{"type": "boolean"}
	case f.Type == verb.FlagUint, f.Type == verb.FlagUint64:
		s = map[string]any{"type": "integer", "minimum": 0}
	default:
		s = map[string]any{"type": "string"}
	}
	if len(f.OneOf) > 0 {
		s["enum"] = f.OneOf
	}
	if d := jsonDefault(f); d != nil && f.Custom == nil {
		s["default"] = d
	}
	return s
}

// jsonDefault renders a flag's default as JSON. A time.Duration's Go value is
// nanoseconds; its string form is what a user types.
func jsonDefault(f verb.Flag) any {
	if f.Default == nil {
		return nil
	}
	if d, ok := f.Default.(interface{ String() string }); ok {
		if _, isDuration := f.Default.(interface{ Nanoseconds() int64 }); isDuration {
			return d.String()
		}
	}
	return f.Default
}

// noStdin is a tool call's stdin: the process's own is the JSON-RPC stream.
type noStdin struct{}

func (noStdin) Read([]byte) (int, error) {
	return 0, errors.New("a tool call has no stdin: give the body as text or with --data")
}

// resolveTool admits a tools/call: a tool this server would not list is a
// JSON-RPC error (spec D15), decided before anything runs.
func resolveTool(f toolFilter, name string) (verb.VerbSpec, *mcpError) {
	spec, ok := lookupTool(name)
	if !ok {
		return verb.VerbSpec{}, &mcpError{Code: rpcInvalidParams, Message: "unknown tool: " + name}
	}
	if why := f.withheld(spec); why != "" {
		return verb.VerbSpec{}, &mcpError{Code: rpcInvalidParams, Message: name + ": " + why}
	}
	return spec, nil
}

// runTool runs one admitted tool under ctx. A refusal by the verb or by the
// server is a tool result with isError, which is what a client shows the
// model.
func (s *mcpServer) runTool(ctx context.Context, f toolFilter, spec verb.VerbSpec, rawArgs json.RawMessage) (blocks []string, isErr bool) {
	name := toolName(spec)
	args := map[string]any{}
	if len(rawArgs) > 0 && string(rawArgs) != "null" {
		dec := json.NewDecoder(bytes.NewReader(rawArgs))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return []string{name + ": arguments: " + err.Error()}, true
		}
	}
	argv, err := buildArgv(spec, args, f.readOnly)
	if err != nil {
		return []string{name + ": " + err.Error()}, true
	}

	// The SAME path a typed command takes: Parse validates (check + Validate),
	// BuildFunc only repacks an already-validated Bound, and the generated
	// dispatch routes the Action to the handler.
	sp := spec.For(verb.CLI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, argv)
	if err != nil {
		return []string{name + ": " + err.Error()}, true
	}
	act, err := sp.BuildFunc()(b)
	if err != nil {
		return []string{name + ": " + err.Error()}, true
	}

	// A COPY with its own sink, no stdin and this call's context: the handler
	// must not touch the JSON-RPC stream, and cancelling the call reaches every
	// client it dials. The verb may send only the kinds it declares (D14).
	sub := s.h
	var buf, errBuf bytes.Buffer
	sub.out, sub.errOut = &buf, &errBuf
	sub.in = noStdin{}
	sub.ctx = cli.WithExpectedKinds(ctx, spec.FlagSetName(), spec.Sends)
	r, handled := verb.DispatchCLIAction[error](sub, act)
	if !handled {
		return []string{name + ": no handler for this verb"}, true
	}
	return toolResultBlocks(buf.Bytes(), errBuf.Bytes(), r)
}

// maxToolResult bounds what one tool call hands the model, per block: a
// record or a transcript can be any size, and the whole of it would be one
// JSON line.
const maxToolResult = 1 << 20

// toolResultBlocks shapes what a body wrote, and its error, into the text
// blocks of a tool result (spec §5.3, D20): stdout, then -- when there is any
// -- stderr ending with the error's text, which is where the command line
// prints it. stdout stays a block of its own, so a JSON result parses as JSON.
// stdout that is not UTF-8 -- a transcript tar, a raw record -- is refused
// rather than sent, since JSON would replace its bytes with U+FFFD.
func toolResultBlocks(out, errOut []byte, err error) ([]string, bool) {
	isErr := err != nil
	stdout := ""
	if utf8.Valid(out) {
		stdout = boundText(string(out), len(out))
	} else {
		stdout = fmt.Sprintf("the output is binary (%d bytes) and a tool result is text: write it to a file with -o", len(out))
		isErr = true
	}
	stderr := strings.ToValidUTF8(string(errOut), "\uFFFD")
	if err != nil {
		msg := err.Error()
		var ec exitCodeError
		if errors.As(err, &ec) {
			msg = ec.msg // empty for a body that already printed why
		}
		if stderr != "" && msg != "" && !strings.HasSuffix(stderr, "\n") {
			stderr += "\n"
		}
		stderr += msg
	}
	stderr = boundText(stderr, len(stderr))

	var blocks []string
	if stdout != "" || stderr == "" {
		blocks = append(blocks, stdout)
	}
	if stderr != "" {
		blocks = append(blocks, stderr)
	}
	return blocks, isErr
}

// boundText cuts text past maxToolResult on a rune boundary, with a notice.
func boundText(text string, total int) string {
	if len(text) <= maxToolResult {
		return text
	}
	cut := maxToolResult
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n[output truncated at %d of %d bytes; -o writes the whole of it to a file where the verb has one]", cut, total)
}

// lookupTool finds a declared tool by its advertised name.
func lookupTool(name string) (verb.VerbSpec, bool) {
	for _, spec := range tools() {
		if toolName(spec) == name {
			return spec, true
		}
	}
	return verb.VerbSpec{}, false
}

// buildArgv reconstructs the command line a user would have typed: the tool's
// Fixed tokens, the flags, then positionals in declaration order, the trailing
// text, and the pathspec. A key the schema does not carry is refused: a typo
// must not silently become a default, and an omitted flag must stay omitted.
func buildArgv(spec verb.VerbSpec, args map[string]any, readOnly bool) ([]string, error) {
	c := spec.For(verb.CLI)
	flags := schemaFlags(spec, readOnly)

	allowed := map[string]bool{"pathspec": c.Pathspec}
	for _, a := range c.Args {
		allowed[a.Name] = true
	}
	for _, f := range flags {
		allowed[f.Name] = true
	}
	if c.Trailing != nil {
		allowed[c.Trailing.Name] = true
	}
	var unknown []string
	for k := range args {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, errors.New("unknown argument(s): " + strings.Join(unknown, ", "))
	}

	// A mode group is one choice; more than one true is a caller error the
	// declaration cannot express in JSON Schema, so it is refused here.
	if c.Modes != nil {
		chosen := 0
		for _, m := range c.Modes.Names {
			if b, ok := args[m].(bool); ok && b {
				chosen++
			}
		}
		if chosen > 1 {
			return nil, fmt.Errorf("at most one of %s may be set", strings.Join(c.Modes.Names, ", "))
		}
	}

	argv := append([]string{}, spec.MCP.Fixed...)
	for _, f := range flags {
		v, ok := args[f.Name]
		if !ok {
			continue
		}
		tokens, err := flagTokens(f, v)
		if err != nil {
			return nil, err
		}
		argv = append(argv, tokens...)
	}

	// Positionals and the trailing text, as values.
	var values []string
	for _, a := range c.Args {
		v, ok := args[a.Name]
		if !ok {
			continue
		}
		vs, err := valueTokens(a.Name, v, a.Variadic)
		if err != nil {
			return nil, err
		}
		values = append(values, vs...)
	}
	var trail []string
	if c.Trailing != nil {
		if v, ok := args[c.Trailing.Name]; ok {
			vs, err := valueTokens(c.Trailing.Name, v, listTrailing(c.Trailing))
			if err != nil {
				return nil, err
			}
			trail = vs
		}
	}

	// A value beginning with '-' must not be read as a flag (spec §5.3).
	dashed := false
	for _, v := range values {
		dashed = dashed || strings.HasPrefix(v, "-")
	}
	for _, v := range trail {
		dashed = dashed || strings.HasPrefix(v, "-")
	}
	switch {
	case !dashed:
		argv = append(append(argv, values...), trail...)
	case c.Pathspec:
		return nil, errors.New("a value beginning with '-' cannot be passed to a verb whose '--' introduces its pathspec")
	case c.Trailing != nil:
		// Flags are not permuted here: one '--' ends them, and everything
		// after it is positional.
		argv = append(append(append(argv, "--"), values...), trail...)
	default:
		// The parser permutes: a '--' protects only the token after it.
		for _, v := range values {
			if strings.HasPrefix(v, "-") {
				argv = append(argv, "--")
			}
			argv = append(argv, v)
		}
	}

	if c.Pathspec {
		if v, ok := args["pathspec"]; ok {
			items, err := valueTokens("pathspec", v, true)
			if err != nil {
				return nil, err
			}
			if len(items) > 0 {
				argv = append(append(argv, "--"), items...)
			}
		}
	}
	return argv, nil
}

// valueTokens renders a positional or trailing value: one token, or one per
// element where the schema says array.
func valueTokens(name string, v any, list bool) ([]string, error) {
	if !list {
		s, err := scalarString(name, v)
		if err != nil {
			return nil, err
		}
		return []string{s}, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected an array", name)
	}
	var out []string
	for _, it := range items {
		s, err := scalarString(name, it)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// flagTokens renders one flag's JSON value as command-line tokens.
func flagTokens(f verb.Flag, v any) ([]string, error) {
	if f.Custom != nil {
		items, ok := v.([]any)
		if !ok {
			items = []any{v}
		}
		var out []string
		for _, it := range items {
			s, err := scalarString(f.Name, it)
			if err != nil {
				return nil, err
			}
			out = append(out, "--"+f.Name+"="+s)
		}
		return out, nil
	}
	if f.Type == verb.FlagBool {
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s: expected a boolean", f.Name)
		}
		if b {
			return []string{"--" + f.Name}, nil
		}
		// false is the zero value; emit it only when the default is true, so
		// the flag can actually be turned off.
		if def, _ := f.Default.(bool); def {
			return []string{"--" + f.Name + "=false"}, nil
		}
		return nil, nil
	}
	s, err := scalarString(f.Name, v)
	if err != nil {
		return nil, err
	}
	return []string{"--" + f.Name + "=" + s}, nil
}

// scalarString accepts the JSON scalars a command line can carry.
func scalarString(name string, v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case nil:
		return "", fmt.Errorf("%s: null is not a value", name)
	default:
		return "", fmt.Errorf("%s: expected a scalar, got %T", name, v)
	}
}
