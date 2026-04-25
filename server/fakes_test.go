package server

import (
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/trsf"
)

type fakeConn struct {
	id   objproto.ConnectionID
	sent [][]byte
}

func (f *fakeConn) ConnectionID() objproto.ConnectionID { return f.id }
func (f *fakeConn) SendMessage(b []byte) (int, uint64, error) {
	f.sent = append(f.sent, append([]byte{}, b...))
	return len(b), 0, nil
}

// CreateSendStream returns nil; tests that rely on streamed responses
// (GetTaskLog) wire a real connection or skip the assertion.
func (f *fakeConn) CreateSendStream() trsf.SendStream { return nil }
