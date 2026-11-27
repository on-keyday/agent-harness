package hostlog

import (
	"bytes"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRingTailAndRange(t *testing.T) {
	r := NewRing(10)
	_, _ = r.Write([]byte("012345"))
	snap := r.ReadSnapshot(protocol.HostLogMode_Tail, 0, 4)
	if snap.Source != protocol.HostLogSource_Ring || snap.TotalSize != 6 || snap.StartOffset != 2 || string(snap.Data) != "2345" {
		t.Fatalf("unexpected tail before wrap: %+v data=%q", snap, string(snap.Data))
	}

	// Wrap around: total=14, retained="456789abcd" (offsets 4..14).
	_, _ = r.Write([]byte("6789abcd"))
	snap = r.ReadSnapshot(protocol.HostLogMode_Tail, 0, 6)
	if snap.TotalSize != 14 || snap.StartOffset != 8 || string(snap.Data) != "89abcd" {
		t.Fatalf("unexpected tail after wrap: %+v data=%q", snap, string(snap.Data))
	}

	// Range before earliest clamps to earliest (4).
	snap = r.ReadSnapshot(protocol.HostLogMode_Range, 1, 5)
	if snap.TotalSize != 14 || snap.StartOffset != 4 || string(snap.Data) != "45678" {
		t.Fatalf("unexpected clamped range: %+v data=%q", snap, string(snap.Data))
	}

	// Range across wrap boundary.
	snap = r.ReadSnapshot(protocol.HostLogMode_Range, 7, 5)
	if snap.StartOffset != 7 || string(snap.Data) != "789ab" {
		t.Fatalf("unexpected wrap-spanning range: %+v data=%q", snap, string(snap.Data))
	}

	// Range past EOF returns empty at TotalSize.
	snap = r.ReadSnapshot(protocol.HostLogMode_Range, 99, 5)
	if snap.StartOffset != 14 || len(snap.Data) != 0 {
		t.Fatalf("unexpected past-EOF range: %+v data=%q", snap, string(snap.Data))
	}

	// Write larger than capacity keeps trailing cap bytes.
	_, _ = r.Write([]byte("0123456789XYZ"))
	snap = r.ReadSnapshot(protocol.HostLogMode_Tail, 0, 10)
	if snap.TotalSize != 27 || snap.StartOffset != 17 || string(snap.Data) != "3456789XYZ" {
		t.Fatalf("unexpected oversize write tail: %+v data=%q", snap, string(snap.Data))
	}
}

func TestFileReaderTailAndRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	content := bytes.Repeat([]byte("abcdefghij"), 3) // 30 bytes
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	rd := NewFileReader(path)

	snap, err := rd.Read(protocol.HostLogMode_Tail, 0, 12)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Source != protocol.HostLogSource_File || snap.TotalSize != 30 || snap.StartOffset != 18 || string(snap.Data) != string(content[18:]) {
		t.Fatalf("unexpected file tail: %+v data=%q", snap, string(snap.Data))
	}

	snap, err = rd.Read(protocol.HostLogMode_Range, 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalSize != 30 || snap.StartOffset != 5 || string(snap.Data) != string(content[5:15]) {
		t.Fatalf("unexpected file range: %+v data=%q", snap, string(snap.Data))
	}

	snap, err = rd.Read(protocol.HostLogMode_Range, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if snap.StartOffset != 30 || len(snap.Data) != 0 {
		t.Fatalf("unexpected file past-EOF: %+v", snap)
	}
}

func TestSetupPreservesDefaultSlogFormat(t *testing.T) {
	prevOut := log.Writer()
	t.Cleanup(func() { log.SetOutput(prevOut) })

	path := filepath.Join(t.TempDir(), "app.log")
	rd, closeFn, err := Setup(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	slog.Info("hello format", "k", "v")
	snap, err := rd.Read(protocol.HostLogMode_Tail, 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	got := string(snap.Data)
	if !strings.Contains(got, " INFO hello format k=v") {
		t.Fatalf("expected Go default slog format, got: %q", got)
	}
	if strings.Contains(got, "level=INFO") {
		t.Fatalf("unexpected TextHandler format: %q", got)
	}
}
