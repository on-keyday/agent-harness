package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func getAnswer(t *testing.T, size uint64, body string) []byte {
	t.Helper()
	res := protocol.TranscriptResult{Status: protocol.TranscriptStatus_Ok, Op: protocol.TranscriptOp_Get}
	res.SetGet(protocol.TranscriptGetBody{Size: size})
	return append(res.MustAppend(nil), body...)
}

// Review Focus 5: a stream that ends before the size its header announced is
// a cut, reported as an error — never a short file passed off as complete.
func TestReadTranscriptGetCutStreamIsError(t *testing.T) {
	var out bytes.Buffer
	_, err := readTranscriptAnswer("get", bytes.NewReader(getAnswer(t, 10, "short")), &out)
	if err == nil || !strings.Contains(err.Error(), "5 of 10") {
		t.Fatalf("err %v, want a cut naming 5 of 10 bytes", err)
	}
	out.Reset()
	if _, err := readTranscriptAnswer("get", bytes.NewReader(getAnswer(t, 5, "whole")), &out); err != nil || out.String() != "whole" {
		t.Fatalf("complete: err %v out %q", err, out.String())
	}
	// A stream that ends inside the header is a cut too.
	if _, err := readTranscriptAnswer("get", bytes.NewReader(getAnswer(t, 5, "whole")[:2]), &out); err == nil {
		t.Fatal("a cut inside the header was accepted")
	}
}

// A refusal on the stream is an error naming the status and the runner's
// reason; the reason is what tells "no such session" from "agy unavailable".
func TestReadTranscriptRefusalNamesTheReason(t *testing.T) {
	res := protocol.TranscriptResult{Status: protocol.TranscriptStatus_NoSuchSession, Op: protocol.TranscriptOp_Files}
	res.SetMessage([]byte("no session x in this task's directory (unavailable: agy: not built)"))
	res.SetFiles(protocol.TranscriptFilesBody{})
	_, err := readTranscriptAnswer("ls", bytes.NewReader(res.MustAppend(nil)), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no_such_session") || !strings.Contains(err.Error(), "agy") {
		t.Fatalf("err %v", err)
	}
}

// A row names the agent, the id, both times and the size; a size the store
// cannot know until read says so rather than printing 0B (never elide or
// fake a zero), and a shared session is marked.
func TestTranscriptSessionRowFields(t *testing.T) {
	var s protocol.TranscriptSession
	s.SetAgent([]byte("claude"))
	s.SetId([]byte("abc"))
	s.CreatedNs = uint64(time.Date(2026, 11, 14, 1, 2, 3, 0, time.Local).UnixNano())
	s.UpdatedNs = s.CreatedNs
	s.SetSizeKnown(true)
	row := TranscriptSessionRow(s)
	for _, want := range []string{"claude", "abc", "created=2026-11-14 01:02:03", "updated=2026-11-14 01:02:03", "size=0B"} {
		if !strings.Contains(row, want) {
			t.Fatalf("row %q lacks %q", row, want)
		}
	}
	s.SetSizeKnown(false)
	s.SetShared(true)
	row = TranscriptSessionRow(s)
	if !strings.Contains(row, "size=unknown") || !strings.Contains(row, "shared") {
		t.Fatalf("row %q, want size=unknown and shared", row)
	}
}

// -o is created only once a byte arrives: a refusal leaves no empty file
// (A1's deferred minor, not repeated here).
func TestTranscriptOutputFileNotCreatedOnRefusal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.jsonl")
	w := NewLazyFile(p)
	res := protocol.TranscriptResult{Status: protocol.TranscriptStatus_NoSuchFile, Op: protocol.TranscriptOp_Get}
	res.SetGet(protocol.TranscriptGetBody{})
	if _, err := readTranscriptAnswer("get", bytes.NewReader(res.MustAppend(nil)), w); err == nil {
		t.Fatal("a refusal was accepted")
	}
	w.Close()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("refusal left a file: %v", err)
	}
	w = NewLazyFile(p)
	if _, err := readTranscriptAnswer("get", bytes.NewReader(getAnswer(t, 3, "abc")), w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "abc" {
		t.Fatalf("file %q", b)
	}
}

// Bounds: the server and runner would refuse these anyway; the CLI refuses
// them before a request that could not fit one datagram is built.
func TestCheckTranscriptNames(t *testing.T) {
	if err := CheckTranscriptNames(strings.Repeat("s", 128), strings.Repeat("f", 512)); err != nil {
		t.Fatalf("at the bounds: %v", err)
	}
	if err := CheckTranscriptNames(strings.Repeat("s", 129), ""); err == nil {
		t.Fatal("a 129-byte session id was accepted")
	}
	if err := CheckTranscriptNames("s", strings.Repeat("f", 513)); err == nil {
		t.Fatal("a 513-byte file name was accepted")
	}
}

// The server answers a transcript request at once — the runner's work comes
// on the stream — so a missing answer is a server that dropped a request it
// could not decode (one older than transcripts), and the error says so
// instead of hanging (Task 5 review, client obligation 8). A caller's own
// cancel stays a cancel.
func TestTranscriptNoAnswerNamesAnOlderServer(t *testing.T) {
	never := func(ctx context.Context) (*protocol.TaskControlResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := awaitTranscriptAnswer(context.Background(), "ls", 20*time.Millisecond, never)
	if err == nil || !strings.Contains(err.Error(), "older than transcripts") {
		t.Fatalf("err %v, want one naming a server older than transcripts", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := awaitTranscriptAnswer(ctx, "ls", time.Minute, never); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "older") {
		t.Fatalf("cancelled: err %v, want context.Canceled alone", err)
	}
}

// A fetched file of zero bytes (an empty -wal, say) still lands as an empty
// file: success with no file would read as "nothing was written" (parity walk
// items 31 and 33). A refusal still leaves none.
func TestTranscriptToFileWritesAnEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.db-wal")
	err := TranscriptToFile(p, func(w io.Writer) error {
		_, err := readTranscriptAnswer("get", bytes.NewReader(getAnswer(t, 0, "")), w)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Size() != 0 {
		t.Fatalf("stat %v %v, want an empty file", fi, err)
	}
	q := filepath.Join(t.TempDir(), "refused")
	if err := TranscriptToFile(q, func(io.Writer) error { return errors.New("no_such_file") }); err == nil {
		t.Fatal("a refusal was reported as success")
	}
	if _, err := os.Stat(q); !os.IsNotExist(err) {
		t.Fatalf("a refusal left a file: %v", err)
	}
}
