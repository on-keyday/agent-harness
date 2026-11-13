package server

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// The record switch on an open (pillar A spec, A-D22).

func withRecordExt(oi *protocol.OpenInteractiveRequest, on bool) *protocol.OpenInteractiveRequest {
	oi.SetExtPresent(true)
	var ext protocol.OpenInteractiveExt
	ext.SetRecordSet(true)
	ext.SetRecord(on)
	oi.SetExt(ext)
	return oi
}

func openWithCaps(h *TaskHandler, oi *protocol.OpenInteractiveRequest, caps protocol.Capability) protocol.OpenInteractiveResponse {
	resp, _ := h.handleOpenInteractive("", nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, caps, "triage")
	return resp
}

func TestOpenRecordFollowsRequestThenServerDefault(t *testing.T) {
	h := newTestHandler(t)
	if e := taskOf(t, h, openBG(h, withRecordExt(bgOpen("claude"), true), "p")); !e.Record {
		t.Fatal("an open that says on is not recorded")
	}
	if e := taskOf(t, h, openBG(h, withRecordExt(bgOpen("claude"), false), "p")); e.Record {
		t.Fatal("an open that says off is recorded")
	}
	if e := taskOf(t, h, openBG(h, bgOpen("claude"), "p")); e.Record {
		t.Fatal("an open that says nothing is recorded under a default of off")
	}
	h.RecordDefault = true
	resp := openBG(h, bgOpen("claude"), "p")
	if e := taskOf(t, h, resp); !e.Record {
		t.Fatal("an open that says nothing is not recorded under a default of on")
	}

	// A resume that says nothing takes the server default, not the previous
	// run's setting (A-D7's rule).
	h.RecordDefault = false
	id := hex.EncodeToString(resp.TaskId.Id[:])
	h.Tasks.Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	re := bgOpen("claude")
	copy(re.ResumeTaskId.Id[:], resp.TaskId.Id[:])
	if r := openBG(h, re, "again"); r.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("resume: %v", r.Status)
	}
	if e, _ := h.Tasks.Get(id); e.Record || e.Run != 2 {
		t.Fatalf("resumed run %d record=%v, want run 2 not recorded", e.Run, e.Record)
	}
}

func TestOpenWithRecordOnNeedsRecordPersist(t *testing.T) {
	h := newTestHandler(t)
	without := protocol.Capability_All &^ protocol.Capability_RecordPersist
	if r := openWithCaps(h, withRecordExt(bgOpen("claude"), true), without); r.Status != protocol.OpenInteractiveStatus_RecordNotPermitted {
		t.Fatalf("on without record_persist: %v, want record_not_permitted", r.Status)
	}
	if r := openWithCaps(h, withRecordExt(bgOpen("claude"), false), without); r.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("off without the bit: %v", r.Status)
	}
	if r := openWithCaps(h, bgOpen("claude"), without); r.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("nothing said without the bit: %v", r.Status)
	}
}

func TestStreamKindIsNeverRecorded(t *testing.T) {
	h := newTestHandler(t)
	oi := withRecordExt(bgOpen("claude"), true)
	oi.SetEventStream(true)
	resp := openBG(h, oi, "p")
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("stream open with record on: %v", resp.Status)
	}
	if e := taskOf(t, h, resp); e.Record {
		t.Fatal("a stream session is marked recorded")
	}
}

func TestRecordSwitchPersistsAcrossReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewTaskStore()
	s.SetWAL(w)
	id := runNumberTask(t, s)
	if err := s.SetRecord(id, true); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewTaskStore()
	r.ReplayEvents(events)
	if e, _ := r.Get(id); !e.Record {
		t.Fatal("task_record_changed did not replay")
	}
}

// Review Focus 1, server half: a held recorded run re-adopted by the next
// server continues the same record.
func TestReadoptContinuesTheSameRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	h := newTestHandler(t)
	h.Records, _ = NewRecordStore(RecordConfig{Dir: dir})
	id := runNumberTask(t, h.Tasks)
	if err := h.Tasks.SetRecord(id, true); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r1 := newFakeStream(t)
	m1 := NewSessionMux(ctx, id, protocol.TaskKind_Interactive, r1, NewRingBuffer(4096), SessionHooks{})
	h.startRecording(id, m1, recordFromRing)
	r1.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("before")))
	waitFor(t, func() bool { return m1.LastOutputUnixNano() != 0 })
	m1.Stop() // a deliberate shutdown: the record is detached, not closed

	h.Records, _ = NewRecordStore(RecordConfig{Dir: dir}) // the next server
	r2 := newFakeStream(t)
	m2 := NewSessionMux(ctx, id, protocol.TaskKind_Interactive, r2, NewRingBuffer(4096), SessionHooks{})
	defer m2.Stop()
	h.startRecording(id, m2, recordContinue)
	r2.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("after")))
	waitFor(t, func() bool { return m2.LastOutputUnixNano() != 0 })
	m2.StopRecording()

	if infos := h.Records.List(id); len(infos) != 1 || infos[0].Run != 1 {
		t.Fatalf("records %+v, want one for run 1", infos)
	}
	var got []string
	_ = h.Records.Read(id, 1, func(e protocol.RecordEntry) error { got = append(got, string(e.Payload)); return nil })
	if strings.Join(got, ",") != "before,after" {
		t.Fatalf("record holds %v, want before then after once", got)
	}
}

func TestPruneDeletesRecords(t *testing.T) {
	s := New(Config{DataDir: t.TempDir()})
	if s.taskHandler.Records == nil {
		t.Fatal("a server with a data dir has no record store")
	}
	id := runNumberTask(t, s.tasks)
	w, _ := s.taskHandler.Records.OpenRun(id, 1, time.Now())
	_ = w.Append(recEntry(1, "x"))
	_ = w.Close(time.Now())
	s.tasks.Finish(id, 0, nil)
	if removed, _, _ := s.tasks.PruneByIDs(nil, []string{id}, t.TempDir()); removed != 1 {
		t.Fatalf("pruned %d", removed)
	}
	if n := len(s.taskHandler.Records.List(id)); n != 0 {
		t.Fatalf("%d records left after the task was pruned", n)
	}
}

func TestRunEndStampsTheRecord(t *testing.T) {
	h := newTestHandler(t)
	h.Records, _ = NewRecordStore(RecordConfig{Dir: filepath.Join(t.TempDir(), "records")})
	id := runNumberTask(t, h.Tasks)
	_ = h.Tasks.SetRecord(id, true)
	w, _ := h.Records.OpenRun(id, 1, time.Now())
	_ = w.Append(recEntry(1, "x"))
	_ = w.Detach()
	h.Tasks.Cancel(id, protocol.EndReason_Deadline, protocol.TaskID{})
	h.finishRecording(id)
	resumeForRun(t, h.Tasks, id)
	infos := h.Records.List(id)
	if len(infos) != 1 || infos[0].EndReason != protocol.EndReason_Deadline || infos[0].EndedNs == 0 {
		t.Fatalf("first run's record %+v, want its end and reason", infos)
	}
}
