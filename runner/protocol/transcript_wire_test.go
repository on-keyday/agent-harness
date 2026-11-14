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
