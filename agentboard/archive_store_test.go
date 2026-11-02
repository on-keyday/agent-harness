package agentboard

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

var wantArchiveID = regexp.MustCompile(`^[0-9a-f]{12}$`)

func TestArchive_FromLogCopiesThrough(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	defer s.Close()
	for seq := uint64(1); seq <= 100; seq++ {
		if err := s.Append("g", storeMsg(seq, "m")); err != nil {
			t.Fatal(err)
		}
	}
	for _, seq := range []uint64{10, 20, 30} {
		w := storeMsg(seq, "m")
		w.RetractedAt = time.UnixMilli(1_700_000_001_000)
		if err := s.Retract("g", w); err != nil {
			t.Fatal(err)
		}
	}
	at := time.UnixMilli(1_700_000_002_000)
	info, err := s.ArchiveLog("g", fsTask, at)
	if err != nil {
		t.Fatal(err)
	}
	if !wantArchiveID.MatchString(info.ID) {
		t.Errorf("id %q is not 12 hex", info.ID)
	}
	if info.Count != 100 || info.Through != 100 || info.Topic != "g" || info.CreatedBy != fsTask || !info.CreatedAt.Equal(at) {
		t.Errorf("info = %+v", info)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive", info.ID+".log")); err != nil {
		t.Errorf("archive file: %v", err)
	}
	page, err := s.ArchivePage(info.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 100 || page[0].Seq != 100 {
		t.Fatalf("page len=%d first=%v", len(page), seqsOf(page[:1]))
	}
	withdrawn := 0
	for _, m := range page {
		if !m.RetractedAt.IsZero() {
			withdrawn++
		}
	}
	if withdrawn != 3 {
		t.Errorf("withdrawn = %d, want 3", withdrawn)
	}
	older, err := s.ArchivePage(info.ID, 51, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := seqsOf(older); len(got) != 10 || got[0] != 50 || got[9] != 41 {
		t.Errorf("before 51 = %v", got)
	}
	got, ok, err := s.ArchiveOf(info.ID)
	if err != nil || !ok || got.Count != 100 || got.Bytes <= 0 {
		t.Errorf("ArchiveOf = %+v %v %v", got, ok, err)
	}
}

func TestArchive_MessagesSnapshot(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	defer s.Close()
	w := storeMsg(2, "m")
	w.RetractedAt = time.UnixMilli(1_700_000_000_900)
	info, err := s.ArchiveMessages("plain", []RetainedMessage{storeMsg(1, "a"), w, storeMsg(3, "c")}, fsTask, time.UnixMilli(1_700_000_003_000))
	if err != nil {
		t.Fatal(err)
	}
	if info.Count != 3 || info.Through != 3 {
		t.Errorf("info = %+v", info)
	}
	page, err := s.ArchivePage(info.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := seqsOf(page); len(got) != 3 || got[0] != 3 || page[1].RetractedAt.IsZero() {
		t.Errorf("page = %v", got)
	}
	if _, err := s.ArchiveMessages("plain", nil, fsTask, time.Now()); !errors.Is(err, ErrArchiveEmpty) {
		t.Errorf("empty snapshot err = %v", err)
	}
}

func TestArchive_RacingAppendIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	defer s.Close()
	if err := s.Append("g", storeMsg(1, "m")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for seq := uint64(2); seq <= 200; seq++ {
			_ = s.Append("g", storeMsg(seq, "m"))
		}
	}()
	info, err := s.ArchiveLog("g", fsTask, time.Now())
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ArchivePage(info.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(page)) != info.Through || info.Count != len(page) {
		t.Errorf("archived %d, through %d, count %d", len(page), info.Through, info.Count)
	}
	for _, m := range page {
		if m.Seq > info.Through {
			t.Errorf("seq %d above through %d", m.Seq, info.Through)
		}
	}
}

func TestArchive_SurvivesSweepAndDrop(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	if err := s.Append("g", storeMsg(1, "m")); err != nil {
		t.Fatal(err)
	}
	info, err := s.ArchiveLog("g", fsTask, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Drop("g"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	if _, err := s2.Load(64); err != nil {
		t.Fatal(err)
	}
	list, err := s2.Archives()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != info.ID || list[0].Count != 1 {
		t.Errorf("archives = %+v", list)
	}
}

func TestArchive_RemoveSeqRewritesBytes(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	defer s.Close()
	for seq := uint64(1); seq <= 5; seq++ {
		body := "m"
		if seq == 3 {
			body = "secret-token-xyz"
		}
		if err := s.Append("g", storeMsg(seq, body)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := s.ArchiveLog("g", fsTask, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "archive", info.ID+".log")
	removed, err := s.ArchiveRemove(info.ID, 3)
	if err != nil || !removed {
		t.Fatalf("remove = %v %v", removed, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Payloads are base64 in the log; check the encoded form too.
	if bytes.Contains(raw, []byte("secret-token-xyz")) || bytes.Contains(raw, []byte("c2VjcmV0LXRva2VuLXh5eg")) {
		t.Errorf("purged payload still on disk")
	}
	got, _, _ := s.ArchiveOf(info.ID)
	if got.Count != 4 {
		t.Errorf("count after remove = %d, want 4", got.Count)
	}
	if removed, err := s.ArchiveRemove(info.ID, 99); err != nil || removed {
		t.Errorf("remove missing seq = %v %v", removed, err)
	}
	if removed, err := s.ArchiveRemove(info.ID, 0); err != nil || !removed {
		t.Fatalf("remove whole = %v %v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("archive file still present: %v", err)
	}
	if _, ok, _ := s.ArchiveOf(info.ID); ok {
		t.Errorf("ArchiveOf after delete found it")
	}
}

func TestArchive_Empty(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	defer s.Close()
	if _, err := s.ArchiveLog("g", fsTask, time.Now()); !errors.Is(err, ErrArchiveEmpty) {
		t.Fatalf("err = %v, want ErrArchiveEmpty", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "archive")); len(entries) != 0 {
		t.Errorf("archive dir holds %d entries", len(entries))
	}
}

func TestArchive_UnknownID(t *testing.T) {
	s := openTestStore(t, t.TempDir(), time.Hour)
	defer s.Close()
	for _, id := range []string{"000000000000", "../retention", ""} {
		if _, ok, err := s.ArchiveOf(id); ok || err != nil {
			t.Errorf("ArchiveOf(%q) = %v %v", id, ok, err)
		}
		if removed, err := s.ArchiveRemove(id, 0); removed || err != nil {
			t.Errorf("ArchiveRemove(%q) = %v %v", id, removed, err)
		}
	}
}
