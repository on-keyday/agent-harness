package cli

import (
	"context"
	"time"
)

// TaskControlClient is what a liveness probe needs from a connection.
type TaskControlClient = taskControlClient

// ProbeLive reports whether the connection `wait` yields answers a whoami
// within d. The wait for the client is INSIDE the same deadline: after a
// reconnect the persist loop announces 'connected' a moment before it installs
// the new client, and a probe landing in that gap must not read "no client yet"
// as a dead link — it waits for the client, and only a client that never shows
// up (or never answers) inside d counts as dead.
//
// Any answer, even an error response, proves the link alive; only the deadline
// expiring is death. Returns the client it asked (nil if none appeared) so the
// caller can close exactly that connection.
func ProbeLive(ctx context.Context, d time.Duration, wait func(context.Context) (TaskControlClient, error)) (alive bool, c TaskControlClient) {
	pctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	c, err := wait(pctx)
	if err != nil {
		return false, nil
	}
	_, err = WhoAmIWith(pctx, c)
	return !(err != nil && pctx.Err() != nil), c
}
