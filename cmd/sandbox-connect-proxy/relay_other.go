//go:build !unix

package main

// The relay serves a unix socket to a podman container and locks with flock;
// the sandbox it belongs to exists on Linux only. The binary still builds
// everywhere `make build` runs.
func runRelay([]string) int {
	logf("FATAL relay: only supported on unix")
	return 2
}
