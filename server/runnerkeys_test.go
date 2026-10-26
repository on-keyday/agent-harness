package server

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestRunnerKeyStorePersistsAcrossLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-keys.json")
	st, err := LoadRunnerKeyStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	k := newTestKey(t)
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	if added, err := st.Enroll(RunnerKeyRecord{Key: k, Label: "gmk", EnrolledAt: at, EnrolledBy: "operator"}); err != nil || !added {
		t.Fatalf("Enroll: added=%v err=%v", added, err)
	}
	again, err := LoadRunnerKeyStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := again.Lookup(k)
	if !ok || rec.Label != "gmk" || !rec.EnrolledAt.Equal(at) || rec.EnrolledBy != "operator" {
		t.Fatalf("after reload: %+v ok=%v", rec, ok)
	}
}

func TestRunnerKeyStoreCorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-keys.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunnerKeyStore(path, nil); err == nil {
		t.Fatal("a corrupt store loaded; it must be a startup error")
	}
}

func TestRunnerKeyStoreReenrollKeepsRecordReplacesLabel(t *testing.T) {
	st, _ := LoadRunnerKeyStore("", nil)
	k := newTestKey(t)
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	st.Enroll(RunnerKeyRecord{Key: k, Label: "old", EnrolledAt: at, EnrolledBy: "operator"})
	added, err := st.Enroll(RunnerKeyRecord{Key: k, Label: "", EnrolledAt: at.Add(time.Hour), EnrolledBy: "task:aa"})
	if err != nil || added {
		t.Fatalf("re-enroll: added=%v err=%v, want false, nil", added, err)
	}
	if rec, _ := st.Lookup(k); rec.Label != "old" || rec.EnrolledBy != "operator" || !rec.EnrolledAt.Equal(at) {
		t.Fatalf("an empty label replaced the record: %+v", rec)
	}
	st.Enroll(RunnerKeyRecord{Key: k, Label: "new", EnrolledBy: "task:bb"})
	if rec, _ := st.Lookup(k); rec.Label != "new" || rec.EnrolledBy != "operator" {
		t.Fatalf("a label re-enroll must replace only the label: %+v", rec)
	}
}

func TestRunnerKeyStoreResolvePrefix(t *testing.T) {
	st, _ := LoadRunnerKeyStore("", nil)
	a := ed25519.PublicKey(append([]byte{0xab, 0xcd, 0xef, 0x01, 0x11}, make([]byte, 27)...))
	b := ed25519.PublicKey(append([]byte{0xab, 0xcd, 0xef, 0x01, 0x22}, make([]byte, 27)...))
	st.Enroll(RunnerKeyRecord{Key: a})
	st.Enroll(RunnerKeyRecord{Key: b})
	if rec, err := st.Resolve("abcdef0111"); err != nil || !rec.Key.Equal(a) {
		t.Errorf("unique prefix: %v %v", rec.Key, err)
	}
	if _, err := st.Resolve("abcdef01"); !errors.Is(err, ErrAmbiguousPrefix) {
		t.Errorf("shared prefix: %v, want ErrAmbiguousPrefix", err)
	}
	if _, err := st.Resolve("abcdef0"); !errors.Is(err, ErrPrefixTooShort) {
		t.Errorf("7 hex: %v, want ErrPrefixTooShort", err)
	}
	if _, err := st.Resolve("12345678"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("no match: %v, want ErrKeyNotFound", err)
	}
	if _, err := st.Resolve("ABCDEF0111"); err != nil {
		t.Errorf("upper-case hex must resolve too: %v", err)
	}
}

func TestRunnerKeyStoreRemoveAndList(t *testing.T) {
	st, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{
		{Key: newTestKey(t), EnrolledAt: time.Unix(20, 0)},
		{Key: newTestKey(t), EnrolledAt: time.Unix(10, 0)},
	})
	list := st.List()
	if len(list) != 2 || !list[0].EnrolledAt.Before(list[1].EnrolledAt) {
		t.Fatalf("List not sorted by EnrolledAt: %+v", list)
	}
	if err := st.Remove(list[0].Key); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Lookup(list[0].Key); ok {
		t.Fatal("removed key still found")
	}
	if err := st.Remove(list[0].Key); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("second remove: %v, want ErrKeyNotFound", err)
	}
}

func TestRunnerKeyStoreFileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "runner-keys.json")
	st, _ := LoadRunnerKeyStore(path, nil)
	st.Enroll(RunnerKeyRecord{Key: newTestKey(t)})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}
