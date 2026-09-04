package protocol

import (
	"bytes"
	"testing"
)

// wal_status was APPENDED to RestoreTasksResponse. Appending is the safe edit
// -- it leaves every field before it at the same offset -- and this is the
// check that it stayed appended: moving it earlier would silently change how
// candidates decode, on the one message an operator reads after losing tasks.
func TestRestoreTasksResponseRoundTripsWALStatus(t *testing.T) {
	for _, status := range []RestoreWALStatus{
		RestoreWALStatus_Ok, RestoreWALStatus_NoDataDir,
		RestoreWALStatus_Missing, RestoreWALStatus_Unreadable,
	} {
		in := RestoreTasksResponse{Restored: 1, AlreadyPresent: 2, NotInWal: 3, WalStatus: status}
		var task RestorableTask
		copy(task.TaskId.Id[:], []byte("abcdefghijklmnop"))
		task.SetRepoPath([]byte("/r"))
		task.SetPrompt([]byte("p"))
		if !in.SetCandidates([]RestorableTask{task}) {
			t.Fatal("SetCandidates")
		}
		var buf bytes.Buffer
		if err := in.Write(&buf); err != nil {
			t.Fatalf("%v: write: %v", status, err)
		}
		var out RestoreTasksResponse
		if err := out.Read(bytes.NewReader(buf.Bytes())); err != nil {
			t.Fatalf("%v: read: %v", status, err)
		}
		if out.WalStatus != status {
			t.Errorf("wal_status = %v, want %v", out.WalStatus, status)
		}
		// The fields BEFORE it, which an appended field must not disturb.
		if out.Restored != 1 || out.AlreadyPresent != 2 || out.NotInWal != 3 {
			t.Errorf("%v: counters changed across the round trip: %+v", status, out)
		}
		if len(out.Candidates) != 1 || out.Candidates[0].TaskId != task.TaskId {
			t.Errorf("%v: candidates changed across the round trip: %+v", status, out.Candidates)
		}
	}
}

// The status is the LAST field, so a decoder that stops after candidates is
// reading a truncated message rather than a valid one. Checked by length: the
// encoding must be exactly one byte longer than the same response without a
// status would be, and that byte must be at the end.
func TestWALStatusIsTheTrailingByte(t *testing.T) {
	r := RestoreTasksResponse{Restored: 7, WalStatus: RestoreWALStatus_Unreadable}
	var buf bytes.Buffer
	if err := r.Write(&buf); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) == 0 {
		t.Fatal("empty encoding")
	}
	if got := RestoreWALStatus(b[len(b)-1]); got != RestoreWALStatus_Unreadable {
		t.Errorf("last byte = %v, want the status; an inserted field would shift it", got)
	}
}

// Every value the schema declares renders as a word. A default-case fallback
// reaching an operator ("RestoreWALStatus(3)") is the same failure the enum
// was added to fix: a message that does not say which state it is.
func TestEveryWALStatusHasAName(t *testing.T) {
	for i := 0; ; i++ {
		s := RestoreWALStatus(i)
		if s.String() == "RestoreWALStatus("+itoa(i)+")" {
			if i < 4 {
				t.Fatalf("RestoreWALStatus(%d) has no name, and the schema declares four", i)
			}
			return
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
