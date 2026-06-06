//go:build !unix

package main

import "github.com/on-keyday/agent-harness/server"

// installTrsfDump is a no-op on platforms without SIGUSR1 (e.g. Windows).
func installTrsfDump(*server.Server) {}
