package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

type recordingSender struct {
	transport string
	budget    int
	sent      [][]byte
}

func (r *recordingSender) ConnectionID() objproto.ConnectionID {
	return objproto.ConnectionID{Transport: r.transport}
}
func (r *recordingSender) SendMessage(b []byte) (int, uint64, error) {
	r.sent = append(r.sent, b)
	return len(b), 0, nil
}
func (r *recordingSender) MaxDatagramSize() int { return r.budget }

// A RunnerRequest that does not fit one udp datagram is refused before it is
// sent; the same request over ws goes out.
func TestSendRunnerRequestRefusesOverBudgetOnUDP(t *testing.T) {
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CloseExecRun}
	req.SetCloseExecRun(protocol.CloseExecRunRequest{ExecId: 1})
	udp := &recordingSender{transport: "udp", budget: 4}
	if err := sendRunnerRequest(udp, req); err == nil || len(udp.sent) != 0 {
		t.Fatalf("udp over budget: err=%v sends=%d, want an error and nothing sent", err, len(udp.sent))
	}
	ws := &recordingSender{transport: "ws", budget: 4}
	if err := sendRunnerRequest(ws, req); err != nil || len(ws.sent) != 1 {
		t.Fatalf("ws: err=%v sends=%d, want one send", err, len(ws.sent))
	}
}

// Every RunnerRequest leaves the server through sendRunnerRequest. Any use of
// the RunnerControl kind elsewhere is how a request gets encoded and sent past
// the check — whatever the spelling (MustAppend, Append, EncodeCopy plus a
// prepended byte, a kind held in a variable), it names the kind. The allowed
// uses: the sender itself, the receive-side dispatch case, and psk.go, which
// re-encodes the handshake's RunnerHello as a RunnerMESSAGE for the in-process
// dispatcher (d.Dispatch) — nothing goes on the wire there.
func TestRunnerRequestsAreSentOnlyThroughSendRunnerRequest(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "runner_send.go" || f == "psk.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "AppKind_RunnerControl") {
				continue
			}
			if strings.TrimSpace(line) == "case appwire.AppKind_RunnerControl:" {
				continue // receiving, not sending
			}
			t.Errorf("%s:%d uses AppKind_RunnerControl; send RunnerRequests through sendRunnerRequest", f, i+1)
		}
	}
}
