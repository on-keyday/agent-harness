//go:build !js

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// pipeSession drives serve over pipes so a test can stage input against the
// responses it has already seen.
type pipeSession struct {
	t     *testing.T
	lines chan string // fed to the server by one writer, so send never blocks
	resps chan map[string]any
	done  chan error
}

func startPipeSession(t *testing.T, srv *mcpServer) *pipeSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &pipeSession{t: t, lines: make(chan string, 16), resps: make(chan map[string]any, 16), done: make(chan error, 1)}
	go func() {
		for l := range p.lines {
			if _, err := io.WriteString(inW, l+"\n"); err != nil {
				return
			}
		}
		inW.Close()
	}()
	go func() {
		err := srv.serve(inR, outW)
		outW.Close()
		p.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("stdout is not JSON-RPC: %q", sc.Text())
				continue
			}
			p.resps <- m
		}
		close(p.resps)
	}()
	return p
}

func (p *pipeSession) send(line string) { p.lines <- line }

// end closes the server's input: the client went away.
func (p *pipeSession) end() { close(p.lines) }

// next is the next response, or a failure after a deadline: a response that
// never comes is what this file exists to catch.
func (p *pipeSession) next() map[string]any {
	p.t.Helper()
	select {
	case m, ok := <-p.resps:
		if !ok {
			p.t.Fatal("the server closed its output")
		}
		return m
	case <-time.After(5 * time.Second):
		p.t.Fatal("no response within 5s")
	}
	return nil
}

func idOf(m map[string]any) float64 {
	id, _ := m["id"].(float64)
	return id
}

// A call that waits does not hold up the calls behind it, and
// notifications/cancelled ends it with no response (spec D18, V11).
func TestMCPCallsOverlapAndCancel(t *testing.T) {
	cancelled := make(chan struct{})
	srv := &mcpServer{h: cliVerbs{}, run: func(ctx context.Context, _ toolFilter, spec verb.VerbSpec, _ json.RawMessage) (string, bool) {
		if toolName(spec) != "agent_wait" {
			return "fast", false
		}
		<-ctx.Done()
		close(cancelled)
		return "ended: " + ctx.Err().Error(), true
	}}
	p := startPipeSession(t, srv)

	p.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_wait","arguments":{}}}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"version","arguments":{}}}`)
	if r := p.next(); idOf(r) != 2 {
		t.Fatalf("first response %v, want the fast call (id 2) while id 1 waits", r)
	}
	p.send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if r := p.next(); idOf(r) != 3 {
		t.Fatalf("response %v, want ping (id 3) while id 1 waits", r)
	}

	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"test"}}`)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("notifications/cancelled did not reach the waiting call's context")
	}
	// The cancelled call answers nothing: the next response is the ping
	// sent after it.
	p.send(`{"jsonrpc":"2.0","id":4,"method":"ping"}`)
	if r := p.next(); idOf(r) != 4 {
		t.Fatalf("response %v after cancelling id 1, want only ping id 4", r)
	}
	p.end()
	if err := <-p.done; err != nil {
		t.Fatal(err)
	}
	for r := range p.resps {
		t.Errorf("response after end of input: %v", r)
	}
}

// End of input is the client going away (MCP's stdio shutdown): what is in
// flight is cancelled and serve returns instead of waiting on it.
func TestMCPEndOfInputCancelsInFlight(t *testing.T) {
	srv := &mcpServer{h: cliVerbs{}, run: func(ctx context.Context, _ toolFilter, _ verb.VerbSpec, _ json.RawMessage) (string, bool) {
		<-ctx.Done()
		return "ended", true
	}}
	p := startPipeSession(t, srv)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_wait","arguments":{}}}`)
	p.end()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after end of input with a call in flight")
	}
}

// Each call runs under its own context, carrying its own verb's Sends
// (spec §8.2, V12): a context shared between calls would let one verb's
// declaration check another's requests.
func TestMCPCallContextsAreDistinct(t *testing.T) {
	ctxs := make(chan context.Context, 2)
	release := make(chan struct{})
	srv := &mcpServer{h: cliVerbs{}, run: func(ctx context.Context, _ toolFilter, _ verb.VerbSpec, _ json.RawMessage) (string, bool) {
		ctxs <- ctx
		<-release
		return "", false
	}}
	p := startPipeSession(t, srv)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_wait","arguments":{}}}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"version","arguments":{}}}`)
	a, b := <-ctxs, <-ctxs
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	// Whichever call is id 2 is cancelled; the other must not be.
	select {
	case <-a.Done():
	case <-b.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling id 2 cancelled neither call")
	}
	if a.Err() != nil && b.Err() != nil {
		t.Errorf("cancelling one call cancelled both: %v / %v", a.Err(), b.Err())
	}
	close(release)
	p.end()
	<-p.done
}
