package protocol

import "testing"

func TestOpenFileTransferRequest_MkdirParentsRoundTrip(t *testing.T) {
	req := OpenFileTransferRequest{
		TaskId:    TaskID{Id: [16]byte{1, 2, 3}},
		Direction: FileTransferDirection_Mkdir,
	}
	req.SetRelPath([]byte("a/b/c"))
	req.SetForce(false)
	req.SetMkdirParents(true)
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &OpenFileTransferRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Direction != FileTransferDirection_Mkdir {
		t.Errorf("direction = %v want mkdir", got.Direction)
	}
	if !got.MkdirParents() || got.Force() {
		t.Errorf("bits: mkdir_parents=%v force=%v want true/false",
			got.MkdirParents(), got.Force())
	}
	if string(got.RelPath) != "a/b/c" {
		t.Errorf("rel = %q want a/b/c", got.RelPath)
	}
}

func TestRunnerOpenFileTransferRequest_MkdirParentsRoundTrip(t *testing.T) {
	req := RunnerOpenFileTransferRequest{
		TaskId:    TaskID{Id: [16]byte{9}},
		StreamId:  7,
		Direction: FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("x/y"))
	req.SetForce(true)
	req.SetMkdirParents(true)
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &RunnerOpenFileTransferRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.MkdirParents() || !got.Force() || got.StreamId != 7 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
