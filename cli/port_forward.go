package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// ForwardSpec is one parsed -L forward: listen on BindAddr:LocalPort, and
// for each accepted connection have the runner dial RemoteHost:RemotePort.
type ForwardSpec struct {
	BindAddr   string
	LocalPort  int
	RemoteHost string
	RemotePort int
}

// parseForwardSpec parses "[bind:]localport:remotehost:remoteport".
// bind defaults to 127.0.0.1 (do not expose the local port externally).
// IPv6 literal hosts are not supported (dogfood scope).
func parseForwardSpec(s string) (ForwardSpec, error) {
	parts := strings.Split(s, ":")
	var bind, rhost, lportS, rportS string
	switch len(parts) {
	case 3:
		bind = "127.0.0.1"
		lportS, rhost, rportS = parts[0], parts[1], parts[2]
	case 4:
		bind, lportS, rhost, rportS = parts[0], parts[1], parts[2], parts[3]
	default:
		return ForwardSpec{}, fmt.Errorf("forward: bad spec %q (want [bind:]localport:remotehost:remoteport)", s)
	}
	lport, err := strconv.Atoi(lportS)
	if err != nil || lport <= 0 || lport > 65535 {
		return ForwardSpec{}, fmt.Errorf("forward: bad local port in %q", s)
	}
	rport, err := strconv.Atoi(rportS)
	if err != nil || rport <= 0 || rport > 65535 {
		return ForwardSpec{}, fmt.Errorf("forward: bad remote port in %q", s)
	}
	if rhost == "" {
		return ForwardSpec{}, fmt.Errorf("forward: empty remote host in %q", s)
	}
	return ForwardSpec{BindAddr: bind, LocalPort: lport, RemoteHost: rhost, RemotePort: rport}, nil
}
