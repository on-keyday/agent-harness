package protocol

import "testing"

func TestBoardReadResponseRoundTrip(t *testing.T) {
	in := BoardReadResponse{RequestId: 7, Status: BoardStatus_Ok, StreamId: 42}
	b := in.MustAppend(nil)
	var out BoardReadResponse
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

// The body carries each message's bytes in its row; the rows used to be sliced
// out of one concatenated stream by a size field.
func TestBoardReadBodyRoundTrip(t *testing.T) {
	var ft TaskID
	ft.Id[0] = 0xAB
	rows := make([]BoardMessageRow, 2)
	for i := range rows {
		rows[i] = BoardMessageRow{Seq: uint64(3 + i), FromTask: ft, ReceivedAtUnixMs: 1700}
		rows[i].SetFromHostname([]byte("gmkhost"))
	}
	rows[0].SetBody([]byte("hello"))
	// rows[1] keeps an empty body: a zero-length message must not swallow the
	// row after it, which the size-sliced layout could not tell apart.
	var in BoardReadBody
	in.SetMsgs(rows)

	b := in.MustAppend(nil)
	var out BoardReadBody
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Msgs) != 2 {
		t.Fatalf("rows: got %d, want 2", len(out.Msgs))
	}
	if m := out.Msgs[0]; m.Seq != 3 || string(m.Body) != "hello" || string(m.FromHostname) != "gmkhost" || m.FromTask.Id[0] != 0xAB {
		t.Fatalf("row 0 round-trip mismatch: %+v", m)
	}
	if m := out.Msgs[1]; m.Seq != 4 || len(m.Body) != 0 {
		t.Fatalf("row 1 round-trip mismatch: %+v", m)
	}
}

func TestDeliveredBodyRoundTrip(t *testing.T) {
	rows := make([]DeliveredMessage, 3)
	for i := range rows {
		rows[i] = DeliveredMessage{Seq: uint64(10 + i), SenderKind: SenderKind_Agent}
		rows[i].SetTopic([]byte("chat.70fbad4a"))
		rows[i].SetPayload([]byte{byte('a' + i), byte('a' + i)})
	}
	var in DeliveredBody
	in.SetMsgs(rows)

	b := in.MustAppend(nil)
	var out DeliveredBody
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Msgs) != 3 {
		t.Fatalf("rows: got %d, want 3", len(out.Msgs))
	}
	for i, m := range out.Msgs {
		want := string([]byte{byte('a' + i), byte('a' + i)})
		if m.Seq != uint64(10+i) || string(m.Payload) != want || string(m.Topic) != "chat.70fbad4a" {
			t.Fatalf("row %d round-trip mismatch: %+v", i, m)
		}
	}
}

func TestBoardPurgeRequestRoundTrip(t *testing.T) {
	in := BoardPurgeRequest{RequestId: 1, Seq: 9}
	in.SetTopic([]byte("chat.deadbeef"))
	b := in.MustAppend(nil)
	var out BoardPurgeRequest
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RequestId != 1 || out.Seq != 9 || string(out.Topic) != "chat.deadbeef" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}
