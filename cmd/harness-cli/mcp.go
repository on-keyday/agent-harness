//go:build !js

package main

// The MCP consumer: `harness-cli mcp` serves the control plane as Model
// Context Protocol tools over stdio.
//
// It is a CONSUMER of the declaration in cli/verb/table.go, not a second
// implementation. tools/list is generated from the same VerbSpec rows the
// command line uses; tools/call synthesizes argv and runs the SAME parser and
// the SAME handler the command line runs. The only thing this file owns is the
// wire: JSON-RPC framing, the tool schema, and the argv reconstruction.
//
// Authority is unchanged. A tool runs as the connection that started this
// process, so the server's caps are the boundary, exactly as they are for a
// typed command. The surface is typed, not privileged.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/buildinfo"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// mcpProtocolVersion is the revision this server speaks. initialize echoes the
// client's requested version when it sends one, so a newer client is not
// refused over a version this server does not track.
const mcpProtocolVersion = "2025-06-18"

// JSON-RPC error codes (the ones this server can produce).
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
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

// serveMCP is the verb body. It reads requests from stdin and writes responses
// to the handler's sink (os.Stdout on the command line).
func serveMCP(h cliVerbs, readOnly bool) error {
	return serveMCPIO(h, readOnly, os.Stdin, h.stdout())
}

// serveMCPIO is the loop with its streams injected, so a test drives it
// without a subprocess.
func serveMCPIO(h cliVerbs, readOnly bool, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	write := func(resp mcpResponse) {
		resp.JSONRPC = "2.0"
		_ = enc.Encode(resp) // the stream is the only report channel; a failed write ends the loop
	}
	writeErr := func(id json.RawMessage, code int, msg string) {
		write(mcpResponse{ID: id, Error: &mcpError{Code: code, Message: msg}})
	}

	sc := bufio.NewScanner(in)
	// Tool arguments and results are small; a request line is not. 4 MiB is
	// generous for a request and still bounded, so a runaway client cannot
	// make the server allocate without limit.
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

		case "notifications/initialized", "notifications/cancelled":
			// No answer: these are one-way by definition.

		case "ping":
			write(mcpResponse{ID: req.ID, Result: map[string]any{}})

		case "tools/list":
			write(mcpResponse{ID: req.ID, Result: map[string]any{"tools": listTools(readOnly)}})

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
			text, isErr := callTool(h, p.Name, p.Arguments, readOnly)
			res := map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			}
			if isErr {
				res["isError"] = true
			}
			write(mcpResponse{ID: req.ID, Result: res})

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

// listTools renders tools/list from the table. Order follows the table, so the
// golden is stable.
func listTools(readOnly bool) []map[string]any {
	tools := make([]map[string]any, 0, 16)
	for _, spec := range verb.Verbs {
		if spec.MCP == nil {
			continue
		}
		// A tool named after the server itself would be the server invoking
		// itself. The declaration already omits it; this keeps a later edit
		// from making that mistake silently.
		if spec.Path[0] == "mcp" {
			continue
		}
		if readOnly && spec.MCP.SideEffect == verb.MCPWrite {
			continue
		}
		tools = append(tools, toolFromSpec(spec))
	}
	return tools
}

// toolFromSpec is the pure function declaration → tool object.
func toolFromSpec(spec verb.VerbSpec) map[string]any {
	cli := spec.For(verb.CLI)
	props := map[string]any{}
	var required []string

	for _, a := range cli.Args {
		props[a.Name] = argSchema(a)
		if !a.Optional {
			required = append(required, a.Name)
		}
	}
	for _, f := range cli.Flags {
		props[f.Name] = flagSchema(f)
	}
	if cli.Pathspec {
		props["pathspec"] = map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
		}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}

	annotations := map[string]any{
		"readOnlyHint":    spec.MCP.SideEffect == verb.MCPRead,
		"destructiveHint": spec.MCP.SideEffect == verb.MCPWrite,
	}

	return map[string]any{
		"name":        toolName(spec),
		"description": toolDescription(spec),
		"inputSchema": schema,
		"annotations": annotations,
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
	switch f.Type {
	case verb.FlagBool:
		s = map[string]any{"type": "boolean"}
	case verb.FlagUint, verb.FlagUint64:
		s = map[string]any{"type": "integer", "minimum": 0}
	case verb.FlagDuration:
		s = map[string]any{"type": "string"}
	default:
		s = map[string]any{"type": "string"}
	}
	if len(f.OneOf) > 0 {
		s["enum"] = f.OneOf
	}
	if d := jsonDefault(f); d != nil {
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

// callTool runs one tool and returns its text. It never returns an error to the
// wire: a refusal is a tool result with isError, which is what a client shows
// to the model.
func callTool(h cliVerbs, name string, rawArgs json.RawMessage, readOnly bool) (string, bool) {
	spec, ok := lookupTool(name)
	if !ok {
		return "unknown tool: " + name, true
	}
	if readOnly && spec.MCP.SideEffect == verb.MCPWrite {
		return name + ": refused by --read-only (declared " + string(spec.MCP.SideEffect) + ")", true
	}

	args := map[string]any{}
	if len(rawArgs) > 0 && string(rawArgs) != "null" {
		dec := json.NewDecoder(bytes.NewReader(rawArgs))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return name + ": arguments: " + err.Error(), true
		}
	}

	argv, err := buildArgv(spec, args)
	if err != nil {
		return name + ": " + err.Error(), true
	}

	// The SAME path a typed command takes: Parse validates (check + Validate),
	// BuildFunc only repacks an already-validated Bound, and the generated
	// dispatch routes the Action to the handler.
	sp := spec.For(verb.CLI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, argv)
	if err != nil {
		return name + ": " + err.Error(), true
	}
	act, err := sp.BuildFunc()(b)
	if err != nil {
		return name + ": " + err.Error(), true
	}

	// A COPY with its own sink: the handler must not write into the JSON-RPC
	// stream. cid is a function value and copies safely.
	sub := h
	var buf bytes.Buffer
	sub.out = &buf
	r, handled := verb.DispatchCLIAction[error](sub, act)
	if !handled {
		return name + ": no handler for this verb", true
	}
	if r != nil {
		// No prefix: a denial reads byte-for-byte as the typed command's.
		return r.Error(), true
	}
	return buf.String(), false
}

// lookupTool finds a declared tool by its advertised name.
func lookupTool(name string) (verb.VerbSpec, bool) {
	for _, spec := range verb.Verbs {
		if spec.MCP == nil || spec.Path[0] == "mcp" {
			continue
		}
		if toolName(spec) == name {
			return spec, true
		}
	}
	return verb.VerbSpec{}, false
}

// buildArgv reconstructs the command line a user would have typed. Flags come
// first (Parse permutes, but a canonical order keeps the differential test
// stable), then positionals in declaration order, then the pathspec.
//
// An unknown key is refused: a typo must not silently become a default.
func buildArgv(spec verb.VerbSpec, args map[string]any) ([]string, error) {
	cli := spec.For(verb.CLI)

	allowed := map[string]bool{"pathspec": cli.Pathspec}
	for _, a := range cli.Args {
		allowed[a.Name] = true
	}
	for _, f := range cli.Flags {
		allowed[f.Name] = true
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

	var argv []string
	for _, f := range cli.Flags {
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

	// A mode group is one choice; more than one true is a caller error the
	// declaration cannot express in JSON Schema, so it is refused here.
	if cli.Modes != nil {
		chosen := 0
		for _, m := range cli.Modes.Names {
			if b, ok := args[m].(bool); ok && b {
				chosen++
			}
		}
		if chosen > 1 {
			return nil, fmt.Errorf("at most one of %s may be set", strings.Join(cli.Modes.Names, ", "))
		}
	}

	for _, a := range cli.Args {
		v, ok := args[a.Name]
		if !ok {
			continue
		}
		if a.Variadic {
			items, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("%s: expected an array", a.Name)
			}
			for _, it := range items {
				s, err := scalarString(a.Name, it)
				if err != nil {
					return nil, err
				}
				argv = append(argv, s)
			}
			continue
		}
		s, err := scalarString(a.Name, v)
		if err != nil {
			return nil, err
		}
		argv = append(argv, s)
	}

	if cli.Pathspec {
		if v, ok := args["pathspec"]; ok {
			items, ok := v.([]any)
			if !ok {
				return nil, errors.New("pathspec: expected an array")
			}
			if len(items) > 0 {
				argv = append(argv, "--")
				for _, it := range items {
					s, err := scalarString("pathspec", it)
					if err != nil {
						return nil, err
					}
					argv = append(argv, s)
				}
			}
		}
	}
	return argv, nil
}

// flagTokens renders one flag's JSON value as command-line tokens.
func flagTokens(f verb.Flag, v any) ([]string, error) {
	switch f.Type {
	case verb.FlagBool:
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
	case verb.FlagUint, verb.FlagUint64:
		s, err := scalarString(f.Name, v)
		if err != nil {
			return nil, err
		}
		return []string{"--" + f.Name + "=" + s}, nil
	default:
		s, err := scalarString(f.Name, v)
		if err != nil {
			return nil, err
		}
		return []string{"--" + f.Name + "=" + s}, nil
	}
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
