package server

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Bound enrollment, C2: a key's binding is part of its record.

func tid(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = b
	t.Id[15] = 0x7a
	return t
}

func TestRunnerKeysPersistBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-keys.json")
	st, err := LoadRunnerKeyStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	k := newTestKey(t)
	bound := []protocol.TaskID{tid(1), tid(2)}
	if added, err := st.Enroll(RunnerKeyRecord{Key: k, Label: "ctl", EnrolledAt: time.Now().UTC(), Bound: bound}); err != nil || !added {
		t.Fatalf("Enroll: added=%v err=%v", added, err)
	}
	again, err := LoadRunnerKeyStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := again.Lookup(k)
	if !ok || !reflect.DeepEqual(rec.Bound, bound) {
		t.Fatalf("after reload: bound=%v ok=%v, want %v", rec.Bound, ok, bound)
	}
}

// A file written before the binding existed has no bound_tasks: unbound.
func TestRunnerKeysFileWithoutBindingLoadsUnbound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-keys.json")
	k := newTestKey(t)
	fixture := `[{"key":"` + hex.EncodeToString(k) + `","label":"old","enrolled_at":"2026-10-27T09:00:00Z","enrolled_by":"operator"}]`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadRunnerKeyStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := st.Lookup(k)
	if !ok || rec.Bound != nil {
		t.Fatalf("old record: bound=%v ok=%v, want nil (unbound)", rec.Bound, ok)
	}
}

func TestRunnerKeysRejectsMalformedBoundID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-keys.json")
	k := newTestKey(t)
	fixture := `[{"key":"` + hex.EncodeToString(k) + `","label":"x","enrolled_at":"2026-10-27T09:00:00Z","enrolled_by":"operator","bound_tasks":["abc"]}]`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunnerKeyStore(path, nil); err == nil || !strings.Contains(err.Error(), "abc") {
		t.Fatalf("load with a malformed bound id: err=%v, want an error naming it", err)
	}
}

// The binding is fixed at enroll: a re-enroll naming another binding changes
// nothing, label included, and says so.
func TestEnrollKeepsRecordWhenBindingDiffers(t *testing.T) {
	st, _ := LoadRunnerKeyStore("", nil)
	k := newTestKey(t)
	if _, err := st.Enroll(RunnerKeyRecord{Key: k, Label: "a", Bound: []protocol.TaskID{tid(1)}}); err != nil {
		t.Fatal(err)
	}
	added, err := st.Enroll(RunnerKeyRecord{Key: k, Label: "b", Bound: []protocol.TaskID{tid(2)}})
	if added || !errors.Is(err, ErrBindingDiffers) {
		t.Fatalf("re-enroll with another binding: added=%v err=%v, want ErrBindingDiffers", added, err)
	}
	rec, _ := st.Lookup(k)
	if rec.Label != "a" || !reflect.DeepEqual(rec.Bound, []protocol.TaskID{tid(1)}) {
		t.Fatalf("record changed: %+v", rec)
	}
	// Unbound against bound differs too.
	if _, err := st.Enroll(RunnerKeyRecord{Key: k}); !errors.Is(err, ErrBindingDiffers) {
		t.Fatalf("re-enroll unbound over a bound key: err=%v, want ErrBindingDiffers", err)
	}
	// The same binding: the label may still be replaced, as before.
	if added, err := st.Enroll(RunnerKeyRecord{Key: k, Label: "c", Bound: []protocol.TaskID{tid(1)}}); added || err != nil {
		t.Fatalf("same binding: added=%v err=%v", added, err)
	}
	if rec, _ := st.Lookup(k); rec.Label != "c" {
		t.Fatalf("same binding: label=%q, want c", rec.Label)
	}
}

// ResolveIn resolves among the records keep admits: a prefix two keys share
// is not ambiguous when keep drops one of them.
func TestResolveInSkipsUnkept(t *testing.T) {
	st, _ := LoadRunnerKeyStore("", nil)
	a := make([]byte, 32)
	b := make([]byte, 32)
	a[0], a[1], a[31] = 0xab, 0xcd, 1
	b[0], b[1], b[31] = 0xab, 0xcd, 2
	st.Enroll(RunnerKeyRecord{Key: a, Bound: []protocol.TaskID{tid(1)}})
	st.Enroll(RunnerKeyRecord{Key: b})
	if _, err := st.Resolve("abcd0000"); !errors.Is(err, ErrAmbiguousPrefix) {
		t.Fatalf("Resolve over both: err=%v, want ambiguous", err)
	}
	rec, err := st.ResolveIn("abcd0000", func(r RunnerKeyRecord) bool { return r.Bound != nil })
	if err != nil || rec.Key[31] != 1 {
		t.Fatalf("ResolveIn: rec=%x err=%v, want the bound key", rec.Key, err)
	}
	if _, err := st.ResolveIn("abcd0000", func(RunnerKeyRecord) bool { return false }); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("ResolveIn keeping nothing: err=%v, want not found", err)
	}
}

func TestNormalizeBinding(t *testing.T) {
	got, err := normalizeBinding([]protocol.TaskID{tid(3), tid(1), tid(3)})
	if err != nil || !reflect.DeepEqual(got, []protocol.TaskID{tid(1), tid(3)}) {
		t.Fatalf("normalize = %v, %v; want sorted, deduped", got, err)
	}
	if got, err := normalizeBinding(nil); err != nil || got != nil {
		t.Fatalf("normalize(nil) = %v, %v; want nil (unbound)", got, err)
	}
	if _, err := normalizeBinding([]protocol.TaskID{{}}); err == nil {
		t.Fatal("a zero task id must be refused")
	}
	many := make([]protocol.TaskID, 17)
	for i := range many {
		many[i] = tid(byte(i + 1))
	}
	if _, err := normalizeBinding(many); err == nil {
		t.Fatal("17 tasks must be refused")
	}
	if _, err := normalizeBinding(many[:16]); err != nil {
		t.Fatalf("16 tasks: %v", err)
	}
}
