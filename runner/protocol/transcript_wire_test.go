package protocol

import (
	"bytes"
	"testing"
)

// The transcript result is the header a runner writes on the stream ahead of
// the answer (pillar B spec, Wire): a sessions list, a session's files, or a
// get whose raw bytes follow the header until EOF.
func TestTranscriptResultRoundTrip(t *testing.T) {
	var own, shared TranscriptSession
	own.SetAgent([]byte("claude"))
	own.SetId([]byte("a1"))
	own.CreatedNs, own.UpdatedNs, own.Size = 1, 2, 3
	shared.SetAgent([]byte("codex"))
	shared.SetId([]byte("b2"))
	shared.SetShared(true)

	sessions := TranscriptResult{Status: TranscriptStatus_Ok, Op: TranscriptOp_Sessions}
	var sb TranscriptSessionsBody
	sb.SetSessions([]TranscriptSession{own, shared})
	sessions.SetSessions(sb)
	var gotS TranscriptResult
	if err := gotS.DecodeExact(sessions.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	rows := gotS.Sessions().Sessions
	if len(rows) != 2 || string(rows[0].Agent) != "claude" || rows[0].Shared() || !rows[1].Shared() || rows[0].Size != 3 {
		t.Fatalf("sessions after round trip: %+v", rows)
	}

	files := TranscriptResult{Status: TranscriptStatus_Ok, Op: TranscriptOp_Files}
	var f TranscriptFile
	f.SetName([]byte("export.json"))
	var fb TranscriptFilesBody
	fb.SetFiles([]TranscriptFile{f})
	files.SetFiles(fb)
	var gotF TranscriptResult
	if err := gotF.DecodeExact(files.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if fs := gotF.Files().Files; len(fs) != 1 || fs[0].SizeKnown() || string(fs[0].Name) != "export.json" {
		t.Fatalf("files after round trip: %+v", fs)
	}

	// A get's header is followed by the raw bytes: a reader decodes the header
	// and the rest of the stream is the file.
	get := TranscriptResult{Status: TranscriptStatus_Ok, Op: TranscriptOp_Get}
	get.SetGet(TranscriptGetBody{Size: 9})
	stream := append(get.MustAppend(nil), []byte("RAW BYTES")...)
	var gotG TranscriptResult
	r := bytes.NewReader(stream)
	if err := gotG.Read(r); err != nil {
		t.Fatal(err)
	}
	rest := make([]byte, r.Len())
	_, _ = r.Read(rest)
	if gotG.Op != TranscriptOp_Get || string(rest) != "RAW BYTES" {
		t.Fatalf("get header %+v, rest %q", gotG, rest)
	}
}

func TestRunnerTranscriptRequestRoundTrip(t *testing.T) {
	in := RunnerTranscriptRequest{StreamId: 7, Op: TranscriptOp_Get}
	in.TaskId.Id[0] = 0xab
	in.SetIncludeShared(true)
	in.SetRepoPath([]byte("/r"))
	in.SetSession([]byte("s"))
	in.SetFile([]byte("f"))
	var out RunnerTranscriptRequest
	if err := out.DecodeExact(in.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if out.StreamId != 7 || !out.IncludeShared() || string(out.RepoPath) != "/r" || string(out.Session) != "s" || string(out.File) != "f" || out.TaskId.Id[0] != 0xab {
		t.Fatalf("round trip = %+v", out)
	}
}

// A runner that serves transcripts says so; one that predates the bit sends
// zero, and the server never asks it (an unknown request type would drop it).
func TestRunnerHelloTranscriptsBit(t *testing.T) {
	var h RunnerHello
	h.SetTranscripts(true)
	var out RunnerHello
	if err := out.DecodeExact(h.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !out.Transcripts() || out.SkillsInjected() {
		t.Fatalf("hello bits: transcripts=%v skills=%v", out.Transcripts(), out.SkillsInjected())
	}
}

// Bounds of the pillar B plan: a session id ≤ 128 bytes and a file name ≤ 512
// keep both control messages that carry them in one datagram (the review of
// Task 1 measured 1306 and 1374 bytes at the plan's first bounds of 256/1024).
const (
	transcriptSessionBound = 128
	transcriptFileBound    = 512
	transcriptRepoAssumed  = 400 // a repo path longer than any the fleet serves
)

func worstTranscriptMessages(session, file int) (*TaskControlRequest, *RunnerRequest) {
	var get TranscriptGetRequest
	get.SetSession(bytes.Repeat([]byte("s"), session))
	get.SetFile(bytes.Repeat([]byte("f"), file))
	req := &TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_TranscriptGet}}
	req.SetTranscriptGet(get)
	var r RunnerTranscriptRequest
	r.SetRepoPath(bytes.Repeat([]byte("r"), transcriptRepoAssumed))
	r.SetSession(bytes.Repeat([]byte("s"), session))
	r.SetFile(bytes.Repeat([]byte("f"), file))
	r.SetIncludeShared(true)
	rr := &RunnerRequest{Kind: RunnerRequestType_Transcript}
	rr.SetTranscript(r)
	return req, rr
}

func TestTranscriptControlMessagesFitOneDatagram(t *testing.T) {
	get, runner := worstTranscriptMessages(transcriptSessionBound, transcriptFileBound)
	if n := encodedLen(t, get); n > controlDatagramBudget {
		t.Errorf("worst transcript_get is %d bytes, budget %d", n, controlDatagramBudget)
	}
	if n := encodedLen(t, runner); n > controlDatagramBudget {
		t.Errorf("worst runner transcript request is %d bytes, budget %d", n, controlDatagramBudget)
	}
}

// Negative control: at the first bounds (256/1024) the runner request does
// not fit, so the test above can fail.
func TestTranscriptFirstBoundsWouldNotFit(t *testing.T) {
	_, runner := worstTranscriptMessages(256, 1024)
	if n := encodedLen(t, runner); n <= controlDatagramBudget {
		t.Errorf("256/1024 encode to %d bytes; expected over %d", n, controlDatagramBudget)
	}
}

// A get says how many bytes follow, so a reader can tell a cut stream from a
// complete one (review of Task 1, I2); a session says whether its size is
// known (opencode's is not, until exported).
func TestTranscriptGetHeaderCarriesSize(t *testing.T) {
	get := TranscriptResult{Status: TranscriptStatus_Ok, Op: TranscriptOp_Get}
	get.SetGet(TranscriptGetBody{Size: 9})
	var out TranscriptResult
	if err := out.Read(bytes.NewReader(append(get.MustAppend(nil), "RAW BYTES"...))); err != nil {
		t.Fatal(err)
	}
	if out.Get() == nil || out.Get().Size != 9 {
		t.Fatalf("get body %+v", out.Get())
	}
	var s TranscriptSession
	s.SetSizeKnown(true)
	s.SetShared(true)
	var back TranscriptSession
	if err := back.DecodeExact(s.MustAppend(nil)); err != nil || !back.SizeKnown() || !back.Shared() {
		t.Fatalf("session bits after round trip: %+v %v", back, err)
	}
}
