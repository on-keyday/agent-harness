package agentboard

import (
	"errors"
	"testing"
)

func TestBoardArchive_RetainedAndPlain(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	retainedWith100(t, b, "kept", Retention{Forever: true})
	before, _ := b.ListRetained("kept")
	info, err := b.Archive("kept", retTid)
	if err != nil {
		t.Fatal(err)
	}
	if info.Count != 100 || info.Topic != "kept" || info.CreatedBy != retTid {
		t.Errorf("retained archive = %+v", info)
	}
	if after, _ := b.ListRetained("kept"); len(after) != len(before) {
		t.Errorf("archive changed the ring: %d → %d", len(before), len(after))
	}

	for i := 0; i < 3; i++ {
		publish(t, b, "plain", "m")
	}
	seq := publish(t, b, "plain", "w")
	if _, ok := b.RetireSeq(seq, retTid); !ok {
		t.Fatal("retract")
	}
	info, err = b.Archive("plain", retTid)
	if err != nil {
		t.Fatal(err)
	}
	if info.Count != 4 || info.Through != seq {
		t.Errorf("plain archive = %+v", info)
	}
	page, err := b.ArchivePage(info.ID, 0, 10)
	if err != nil || len(page) != 4 || page[0].RetractedAt.IsZero() {
		t.Errorf("ArchivePage = %v %v", seqsOf(page), err)
	}
	live, err := b.ArchivePageLive(info.ID, 0, 10)
	if err != nil || len(live) != 3 {
		t.Errorf("ArchivePageLive = %v %v", seqsOf(live), err)
	}
}

func TestBoardArchive_Refusals(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	publish(t, b, "chat.abcd1234", "m")
	if _, err := b.Archive("chat.abcd1234", retTid); !errors.Is(err, ErrNotRetainable) {
		t.Errorf("chat: %v", err)
	}
	if _, err := b.Archive("missing", retTid); !errors.Is(err, ErrNoTopic) {
		t.Errorf("missing: %v", err)
	}
	if err := b.Retain("empty", Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Archive("empty", retTid); !errors.Is(err, ErrArchiveEmpty) {
		t.Errorf("empty: %v", err)
	}
}

func TestBoardArchive_OutlivesSourceDestruction(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	retainedWith100(t, b, "g", Retention{Forever: true})
	info, err := b.Archive("g", retTid)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Flush("g"); err != nil {
		t.Fatal(err)
	}
	b.PurgeTopic("g")
	if err := b.ClearRetain("g"); err != nil {
		t.Fatal(err)
	}
	page, err := b.ArchivePage(info.ID, 0, 200)
	if err != nil || len(page) != 100 {
		t.Errorf("archive after destruction = %d %v", len(page), err)
	}
	list, err := b.Archives("g")
	if err != nil || len(list) != 1 || list[0].ID != info.ID {
		t.Errorf("Archives(g) = %+v %v", list, err)
	}
	if list, _ := b.Archives("other"); len(list) != 0 {
		t.Errorf("Archives(other) = %+v", list)
	}
}

func TestBoardArchive_Purge(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	seqs := retainedWith100(t, b, "g", Retention{Forever: true})
	info, err := b.Archive("g", retTid)
	if err != nil {
		t.Fatal(err)
	}
	if removed, found, err := b.ArchivePurge(info.ID, seqs[0]); !removed || !found || err != nil {
		t.Errorf("purge seq = %v %v %v", removed, found, err)
	}
	if removed, found, _ := b.ArchivePurge(info.ID, seqs[0]); removed || !found {
		t.Errorf("purge again = %v %v", removed, found)
	}
	if removed, found, err := b.ArchivePurge(info.ID, 0); !removed || !found || err != nil {
		t.Errorf("purge whole = %v %v %v", removed, found, err)
	}
	if _, found, _ := b.ArchivePurge(info.ID, 0); found {
		t.Errorf("purged archive still found")
	}
}

func TestBoardArchive_SeqSeedAboveArchive(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{SeqSeed: 1 << 40}, dir)
	publish(t, b, "plain", "m")
	info, err := b.Archive("plain", retTid)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	_ = s.Close()
	b2, s2 := newRetainBoard(t, Config{SeqSeed: 1}, dir)
	defer b2.Close()
	defer s2.Close()
	if seq := publish(t, b2, "plain", "m"); seq <= info.Through {
		t.Errorf("seq %d not above archived %d", seq, info.Through)
	}
}
