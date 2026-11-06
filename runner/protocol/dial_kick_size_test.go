package protocol

import (
	"bytes"
	"testing"
)

// controlDatagramBudget is the body a control message may use over UDP: the
// 1200-byte PLPMTUD floor minus objproto's header and AEAD tag. An oversized
// message is dropped with no error to either end
// (docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md §4).
const controlDatagramBudget = 1170

func worstDialAdd(nCandidates, each int) *TaskControlRequest {
	r := RunnerDialAddRequest{}
	r.SetKeyPrefix(bytes.Repeat([]byte("a"), 64))
	r.SetLabel(bytes.Repeat([]byte("l"), 64))
	cands := make([]RunnerDialCandidate, nCandidates)
	for i := range cands {
		cands[i].SetText(bytes.Repeat([]byte("c"), each))
	}
	r.SetCandidates(cands)
	req := &TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_RunnerDialAdd}}
	req.SetRunnerDialAdd(r)
	return req
}

func encodedLen(t *testing.T, enc interface{ Append([]byte) ([]byte, error) }) int {
	t.Helper()
	b, err := enc.Append([]byte{0})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return len(b)
}

func TestDialKickControlMessagesFitOneDatagram(t *testing.T) {
	// 8 candidates totalling 512 bytes (the spec's cap): 64 each.
	if n := encodedLen(t, worstDialAdd(8, 64)); n > controlDatagramBudget {
		t.Errorf("worst runner_dial_add is %d bytes, budget %d", n, controlDatagramBudget)
	}

	kick := RunnerKickRequest{}
	kick.SetPrefix(bytes.Repeat([]byte("f"), 64))
	kick.SetForce(true)
	kreq := &TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_RunnerKick}}
	kreq.SetRunnerKick(kick)
	if n := encodedLen(t, kreq); n > controlDatagramBudget {
		t.Errorf("worst runner_kick is %d bytes, budget %d", n, controlDatagramBudget)
	}

	leave := LeaveRequest{}
	leave.SetBy(bytes.Repeat([]byte("b"), 64))
	lreq := &RunnerRequest{Kind: RunnerRequestType_Leave}
	lreq.SetLeave(leave)
	if n := encodedLen(t, lreq); n > controlDatagramBudget {
		t.Errorf("worst leave is %d bytes, budget %d", n, controlDatagramBudget)
	}
}

// The cap on the total is what keeps the add request in one datagram: without
// it, the per-candidate limit alone (8 x 255) would not fit. Negative control
// for the test above.
func TestDialAddWithoutTotalCapWouldNotFit(t *testing.T) {
	if n := encodedLen(t, worstDialAdd(8, 255)); n <= controlDatagramBudget {
		t.Errorf("8 x 255-byte candidates encode to %d bytes; expected over %d — the size test above could not fail", n, controlDatagramBudget)
	}
}
