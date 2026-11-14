package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type fakeTCC func(ctx context.Context) error

func (f fakeTCC) RoundTripTaskControl(ctx context.Context, _ *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	return nil, f(ctx)
}

var (
	answersWithError = fakeTCC(func(context.Context) error { return errors.New("not permitted") })
	neverAnswers     = fakeTCC(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
)

// The gap between 'connected' and the client being installed: the client shows
// up 60ms into a 1s probe. That is alive, not "no client, dead".
func TestProbeLive_WaitsForTheClientInsideTheDeadline(t *testing.T) {
	wait := func(ctx context.Context) (TaskControlClient, error) {
		select {
		case <-time.After(60 * time.Millisecond):
			return answersWithError, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	alive, c := ProbeLive(context.Background(), time.Second, wait)
	if !alive || c == nil {
		t.Fatalf("alive=%v client=%v: a client that appears within the deadline and answers is alive", alive, c)
	}
}

func TestProbeLive_NoClientWithinDeadlineIsDead(t *testing.T) {
	wait := func(ctx context.Context) (TaskControlClient, error) { <-ctx.Done(); return nil, ctx.Err() }
	start := time.Now()
	alive, c := ProbeLive(context.Background(), 80*time.Millisecond, wait)
	if alive || c != nil {
		t.Fatalf("alive=%v client=%v, want dead and no client", alive, c)
	}
	if time.Since(start) > time.Second {
		t.Fatal("did not give up at the deadline")
	}
}

func TestProbeLive_UnansweredRequestIsDead(t *testing.T) {
	alive, c := ProbeLive(context.Background(), 80*time.Millisecond,
		func(context.Context) (TaskControlClient, error) { return neverAnswers, nil })
	if alive {
		t.Fatal("a request nobody answers must read as dead")
	}
	if c == nil {
		t.Fatal("the probed client must be returned so the caller closes that connection")
	}
}

func TestProbeLive_AnErrorAnswerStillProvesTheLinkAlive(t *testing.T) {
	alive, _ := ProbeLive(context.Background(), time.Second,
		func(context.Context) (TaskControlClient, error) { return answersWithError, nil })
	if !alive {
		t.Fatal("an error response is an answer")
	}
}
