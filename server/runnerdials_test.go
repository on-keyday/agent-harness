package server

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func dialTestKey(b byte) ed25519.PublicKey {
	k := make(ed25519.PublicKey, ed25519.PublicKeySize)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestRunnerDialStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-dials.json")
	st, err := LoadRunnerDialStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	a, err := st.Add(RunnerDialRecord{Candidates: []string{"ws:a.example:8540-*", "udp:a.example:8541-*"},
		Key: dialTestKey(1), Label: "aws-a", AddedAt: t0, AddedBy: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Add(RunnerDialRecord{Candidates: []string{"ws:b.example:8540-*"},
		Key: dialTestKey(2), AddedAt: t0.Add(time.Minute), AddedBy: "task:00112233445566778899aabbccddeeff"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || b.ID == 0 || a.ID == b.ID {
		t.Fatalf("ids must be non-zero and distinct: %d %d", a.ID, b.ID)
	}
	re, err := LoadRunnerDialStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := re.List()
	want := []RunnerDialRecord{a, b}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded\n got %+v\nwant %+v", got, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", fi.Mode(), err)
	}
}

func TestRunnerDialStoreUnparsableFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-dials.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunnerDialStore(path); err == nil {
		t.Fatal("an unparsable file must fail the load, not start empty")
	}
}

func TestRunnerDialStoreAbsentFileIsEmpty(t *testing.T) {
	st, err := LoadRunnerDialStore(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || len(st.List()) != 0 {
		t.Fatalf("absent file: %v, %d records", err, len(st.List()))
	}
}

func TestRunnerDialStoreRemoveByKey(t *testing.T) {
	st, _ := LoadRunnerDialStore("")
	for _, c := range []string{"ws:a:1-*", "ws:a:2-*"} {
		if _, err := st.Add(RunnerDialRecord{Candidates: []string{c}, Key: dialTestKey(1)}); err != nil {
			t.Fatal(err)
		}
	}
	keep, _ := st.Add(RunnerDialRecord{Candidates: []string{"ws:b:1-*"}, Key: dialTestKey(2)})
	removed := st.RemoveByKey(dialTestKey(1))
	if len(removed) != 2 {
		t.Fatalf("removed %d, want 2", len(removed))
	}
	if l := st.List(); len(l) != 1 || l[0].ID != keep.ID {
		t.Fatalf("left %+v", l)
	}
}

func TestRunnerDialStoreRemove(t *testing.T) {
	st, _ := LoadRunnerDialStore("")
	r, _ := st.Add(RunnerDialRecord{Candidates: []string{"ws:a:1-*"}, Key: dialTestKey(1)})
	if got, err := st.Remove(r.ID); err != nil || got.ID != r.ID {
		t.Fatalf("remove: %+v %v", got, err)
	}
	if _, err := st.Remove(r.ID); !errors.Is(err, ErrDialNotFound) {
		t.Fatalf("second remove: %v, want ErrDialNotFound", err)
	}
}

func TestRunnerDialStoreOwnerOfCandidate(t *testing.T) {
	st, _ := LoadRunnerDialStore("")
	r, _ := st.Add(RunnerDialRecord{Candidates: []string{"ws:a:1-*", "udp:a:2-*"}, Key: dialTestKey(1)})
	if id, ok := st.OwnerOfCandidate("udp:a:2-*"); !ok || id != r.ID {
		t.Fatalf("owner: %d %v", id, ok)
	}
	if _, ok := st.OwnerOfCandidate("udp:a:3-*"); ok {
		t.Fatal("no record has that text")
	}
}
