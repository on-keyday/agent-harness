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

// Every RunnerRequest leaves the server through sendRunnerRequest. The encode
// prefix is the tell: a RunnerRequest encoded anywhere else is one sent past
// the check.
func TestRunnerRequestsAreSentOnlyThroughSendRunnerRequest(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const tell = "byte(appwire.AppKind_RunnerControl)})"
	for _, f := range files {
		// psk.go re-encodes the handshake's RunnerHello as a RunnerMESSAGE for
		// the in-process dispatcher (d.Dispatch); nothing goes on the wire.
		if strings.HasSuffix(f, "_test.go") || f == "runner_send.go" || f == "psk.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), tell) {
			t.Errorf("%s encodes a RunnerRequest itself; send it through sendRunnerRequest", f)
		}
	}
}
