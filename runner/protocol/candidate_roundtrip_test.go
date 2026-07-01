package protocol

import "testing"

func TestOpenInteractiveResponseCandidatesRoundTrip(t *testing.T) {
	var c RunnerCandidate
	c.SetCid([]byte("ws:192.168.3.14:34184-30218"))
	c.SetHostname([]byte("gmkhost-codex"))
	c.SetMatchedRoot([]byte("/home/x/repo"))
	c.ActiveTasks = 2
	c.MaxTasks = 8

	resp := OpenInteractiveResponse{Status: OpenInteractiveStatus_AmbiguousRunner}
	// Candidates/CandidatesLen live inside the conditional (ambiguous_runner-only)
	// block, so brgen generates them as variant accessor methods rather than
	// plain struct fields (same shape as NotifyRequest.Worker()/SetWorker()).
	// SetCandidates derives CandidatesLen from the slice length.
	if !resp.SetCandidates([]RunnerCandidate{c}) {
		t.Fatalf("SetCandidates failed")
	}

	// Encode(buf) writes into a pre-sized buffer (see EncodeCopy/MustEncode's
	// "reserved" naming) and errors immediately on a nil/zero-cap slice.
	// Append(nil) is the growth-safe encode used by every other round-trip
	// test in this package (e.g. chained_relay_test.go), so use it here too.
	raw, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got OpenInteractiveResponse
	if _, err := got.Decode(raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotCandidates := got.Candidates()
	if got.Status != OpenInteractiveStatus_AmbiguousRunner || gotCandidates == nil || len(*gotCandidates) != 1 {
		t.Fatalf("status=%v candidates=%v", got.Status, gotCandidates)
	}
	cand := (*gotCandidates)[0]
	if string(cand.Cid) != "ws:192.168.3.14:34184-30218" ||
		string(cand.Hostname) != "gmkhost-codex" ||
		cand.ActiveTasks != 2 || cand.MaxTasks != 8 {
		t.Fatalf("candidate mismatch: %+v", cand)
	}

	// Non-ambiguous status must NOT carry candidate bytes (conditional block absent).
	ok := OpenInteractiveResponse{Status: OpenInteractiveStatus_Ok}
	okRaw, err := ok.Append(nil)
	if err != nil {
		t.Fatalf("encode ok: %v", err)
	}
	var gotOk OpenInteractiveResponse
	if _, err := gotOk.Decode(okRaw); err != nil {
		t.Fatalf("decode ok: %v", err)
	}
	if okCandidates := gotOk.Candidates(); okCandidates != nil && len(*okCandidates) != 0 {
		t.Fatalf("ok response carried %d candidates, want 0", len(*okCandidates))
	}
}
