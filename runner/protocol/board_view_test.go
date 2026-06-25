package protocol

import "testing"

func TestBoardReadResponseRoundTrip(t *testing.T) {
	in := BoardReadResponse{RequestId: 7, Status: BoardStatus_Ok, StreamId: 42}
	var ft TaskID
	ft.Id[0] = 0xAB
	row := BoardMessageRow{Seq: 3, FromTask: ft, ReceivedAtUnixMs: 1700, Size: 5}
	row.SetFromHostname([]byte("gmkhost"))
	in.Msgs = append(in.Msgs, row)
	in.MsgsLen = uint16(len(in.Msgs))

	b := in.MustAppend(nil)
	var out BoardReadResponse
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RequestId != 7 || out.Status != BoardStatus_Ok || out.StreamId != 42 || out.MsgsLen != 1 {
		t.Fatalf("header round-trip mismatch: %+v", out)
	}
	if out.Msgs[0].Seq != 3 || out.Msgs[0].Size != 5 || string(out.Msgs[0].FromHostname) != "gmkhost" || out.Msgs[0].FromTask.Id[0] != 0xAB {
		t.Fatalf("row round-trip mismatch: %+v", out.Msgs[0])
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
