package protocol

import (
	"bytes"
	"testing"
)

// A RecordEntry is both a segment's on-disk unit and a record-get stream's
// body (pillar A spec, A-D23 / A-D27), so its round trip is the format's whole
// contract.
func TestRecordEntryRoundTrip(t *testing.T) {
	in := RecordEntry{TsNs: 1, FrameType: 1}
	in.SetPayload([]byte("abc"))
	b := in.MustAppend(nil)
	var out RecordEntry
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.TsNs != 1 || out.FrameType != 1 || !bytes.Equal(out.Payload, []byte("abc")) {
		t.Fatalf("round trip = %+v", out)
	}
}

func TestOpenInteractiveExtRoundTrip(t *testing.T) {
	var with OpenInteractiveRequest
	with.SetExtPresent(true)
	var ext OpenInteractiveExt
	ext.SetRecordSet(true)
	ext.SetRecord(false)
	if !with.SetExt(ext) {
		t.Fatal("SetExt returned false")
	}
	b := with.MustAppend(nil)
	var out OpenInteractiveRequest
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := out.Ext()
	if !out.ExtPresent() || got == nil || !got.RecordSet() || got.Record() {
		t.Fatalf("ext after round trip: present=%v ext=%+v", out.ExtPresent(), got)
	}

	// Without the bit the byte is not on the wire: an open that says nothing
	// about recording is exactly one byte shorter than one that does.
	var without OpenInteractiveRequest
	if n, m := len(without.MustAppend(nil)), len(b); n != m-1 {
		t.Fatalf("no-ext request is %d bytes, ext request %d; want exactly one byte apart", n, m)
	}
}

// Every new request and its fixed-size response is one control message; the
// variable-length parts (a record list, a record's entries) ride streams.
func TestRecordRequestsFitOneDatagram(t *testing.T) {
	var tid TaskID
	for i := range tid.Id {
		tid.Id[i] = 0xff
	}
	ref := RecordRef{TaskId: tid, Run: 0xffffffff}

	reqs := map[string]func(*TaskControlRequest) bool{
		"list": func(r *TaskControlRequest) bool {
			var l RecordListRequest
			l.SetTaskPresent(true)
			l.SetTaskId(tid)
			return r.SetRecordList(l)
		},
		"get":    func(r *TaskControlRequest) bool { return r.SetRecordGet(RecordGetRequest{Ref: ref}) },
		"delete": func(r *TaskControlRequest) bool { return r.SetRecordDelete(RecordDeleteRequest{Ref: ref}) },
		"keep": func(r *TaskControlRequest) bool {
			return r.SetRecordKeep(RecordKeepRequest{Ref: ref, TtlMs: ^uint64(0)})
		},
		"switch": func(r *TaskControlRequest) bool { return r.SetRecordSwitch(RecordSwitchRequest{TaskId: tid}) },
	}
	kinds := map[string]TaskControlKind{
		"list": TaskControlKind_RecordList, "get": TaskControlKind_RecordGet,
		"delete": TaskControlKind_RecordDelete, "keep": TaskControlKind_RecordKeep,
		"switch": TaskControlKind_RecordSwitch,
	}
	for name, set := range reqs {
		r := TaskControlRequest{Header: TaskControlRequestHeader{Kind: kinds[name], RequestId: 0xffffffff}}
		if !set(&r) {
			t.Fatalf("%s: setter returned false", name)
		}
		b, err := r.Append(nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := CheckControlMessage("udp", 1170, len(b)); err != nil {
			t.Fatalf("%s request: %v", name, err)
		}
	}

	resps := map[string]func(*TaskControlResponse) bool{
		"list":   func(r *TaskControlResponse) bool { return r.SetRecordList(RecordListResponse{StreamId: ^uint64(0)}) },
		"get":    func(r *TaskControlResponse) bool { return r.SetRecordGet(RecordGetResponse{StreamId: ^uint64(0)}) },
		"delete": func(r *TaskControlResponse) bool { return r.SetRecordDelete(RecordDeleteResponse{}) },
		"keep":   func(r *TaskControlResponse) bool { return r.SetRecordKeep(RecordKeepResponse{}) },
		"switch": func(r *TaskControlResponse) bool { return r.SetRecordSwitch(RecordSwitchResponse{}) },
	}
	for name, set := range resps {
		r := TaskControlResponse{Kind: kinds[name], RequestId: 0xffffffff}
		if !set(&r) {
			t.Fatalf("%s: response setter returned false", name)
		}
		b, err := r.Append(nil)
		if err != nil {
			t.Fatalf("%s response: %v", name, err)
		}
		if err := CheckControlMessage("udp", 1170, len(b)); err != nil {
			t.Fatalf("%s response: %v", name, err)
		}
	}
}
