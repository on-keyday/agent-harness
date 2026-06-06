//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/on-keyday/agent-harness/server"
)

// installTrsfDump wires SIGUSR1 to a trsf internal-state dump (debug aid):
//
//	kill -USR1 <server-pid>
//
// prints every active connection's transport state (role runner/client) to the
// server log — use it during a stuck remote-forward to see whether a relay's
// recv streams aren't draining.
func installTrsfDump(s *server.Server) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			s.DumpTrsfState()
		}
	}()
}
