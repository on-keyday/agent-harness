package cli

import (
	"sync"

	"github.com/on-keyday/agent-harness/connauth"
)

// ClientHandle wraps a *Client as a connauth.PersistHandle. Done() comes from the
// embedded peer.Conn; Close is idempotent. Used by long-lived clients (TUI,
// WebUI WASM) to plug a *cli.Client into connauth.PersistLoop without redefining the
// adapter inline. The runner has its own RunHandle (in package runner) that
// implements PersistHandle directly via *peer.Conn, so it does not use this.
type ClientHandle struct {
	C        *Client
	doneOnce sync.Once
}

// NewClientHandle wraps a *Client.
func NewClientHandle(c *Client) *ClientHandle { return &ClientHandle{C: c} }

func (h *ClientHandle) Done() <-chan struct{} { return h.C.Peer().Done() }
func (h *ClientHandle) Close()                { h.doneOnce.Do(func() { h.C.Close() }) }

var _ connauth.PersistHandle = (*ClientHandle)(nil)
