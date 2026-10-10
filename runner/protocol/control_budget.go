package protocol

import "fmt"

// CheckControlMessage refuses a control message that would be dropped
// silently. Every control message is ONE objproto message, which over UDP is
// ONE datagram, and a datagram that does not fit the path is dropped with no
// error at either end. budget is what fits right now: the connection's trsf
// MaxDatagramSize(), which follows PLPMTUD. ws and wss ride TCP and are not
// checked.
//
// The callers are the paths whose messages carry caller-chosen lengths — a
// client's task-control request and the server's RunnerRequest — not every
// send; see docs/superpowers/specs/2026-10-10-exec-request-streamed-design.md.
func CheckControlMessage(transport string, budget, n int) error {
	if transport != "udp" || n <= budget {
		return nil
	}
	return fmt.Errorf("control message is %d bytes; one udp datagram holds %d on this path, and the rest would be dropped silently — this request has to stream its body (or connect over ws)", n, budget)
}
