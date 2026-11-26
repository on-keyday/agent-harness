package server

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// actDef builds a valid definition: params as --param specs, then the
// template words after "--".
func actDef(t *testing.T, name string, specs []string, words ...string) protocol.ActionDefinition {
	t.Helper()
	var d protocol.ActionDefinition
	d.SetName([]byte(name))
	var ps []protocol.ActionParam
	for _, s := range specs {
		p, err := actiondef.ParseParam(s)
		if err != nil {
			t.Fatalf("ParseParam(%q): %v", s, err)
		}
		ps = append(ps, p)
	}
	d.SetParams(ps)
	els, err := actiondef.ParseTemplate(words, ps)
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	d.SetTemplate(els)
	if err := actiondef.Validate(&d); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return d
}

var actNow = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

func TestActionStoreCreateAssignsRandomIDRevisionOne(t *testing.T) {
	st, _ := LoadActionStore("")
	a, err := st.Create(actDef(t, "a", nil, "true"), "operator", actNow)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Create(actDef(t, "b", nil, "true"), "operator", actNow)
	if err != nil {
		t.Fatal(err)
	}
	if a.Id.Id == ([16]byte{}) || b.Id.Id == ([16]byte{}) || a.Id == b.Id {
		t.Fatalf("ids %x %x: want two distinct non-zero ids", a.Id.Id, b.Id.Id)
	}
	if a.Revision != 1 || a.CreatedAtMs != uint64(actNow.UnixMilli()) || a.EditedAtMs != a.CreatedAtMs ||
		string(a.CreatedBy) != "operator" || string(a.EditedBy) != "operator" {
		t.Fatalf("record = %+v", a)
	}
}

func TestActionStoreNameTaken(t *testing.T) {
	st, _ := LoadActionStore("")
	a, _ := st.Create(actDef(t, "a", nil, "true"), "operator", actNow)
	st.Create(actDef(t, "b", nil, "true"), "operator", actNow)
	if _, err := st.Create(actDef(t, "a", nil, "false"), "operator", actNow); !errors.Is(err, ErrActionNameTaken) {
		t.Fatalf("create a twice: %v, want ErrActionNameTaken", err)
	}
	if _, err := st.Edit(a.Id.Id, 1, actDef(t, "b", nil, "true"), "operator", actNow, nil); !errors.Is(err, ErrActionNameTaken) {
		t.Fatalf("rename a onto b: %v, want ErrActionNameTaken", err)
	}
	// Renaming to its own name is not taken.
	if _, err := st.Edit(a.Id.Id, 1, actDef(t, "a", nil, "false"), "operator", actNow, nil); err != nil {
		t.Fatalf("edit a keeping its name: %v", err)
	}
}

// Two editors that read revision 1 cannot both write: the second is refused,
// and what stands is the first's.
func TestActionStoreStaleRevision(t *testing.T) {
	st, _ := LoadActionStore("")
	a, _ := st.Create(actDef(t, "a", nil, "true"), "operator", actNow)
	first, err := st.Edit(a.Id.Id, 1, actDef(t, "a", nil, "first"), "task:aa", actNow.Add(time.Minute), nil)
	if err != nil || first.Revision != 2 || string(first.EditedBy) != "task:aa" || first.CreatedAtMs != a.CreatedAtMs ||
		first.EditedAtMs != uint64(actNow.Add(time.Minute).UnixMilli()) {
		t.Fatalf("first edit: %+v %v", first, err)
	}
	if _, err := st.Edit(a.Id.Id, 1, actDef(t, "a", nil, "second"), "task:bb", actNow, nil); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("second edit at revision 1: %v, want ErrStaleRevision", err)
	}
	got, _ := st.Get(a.Id.Id)
	if got.Revision != 2 || string(got.Definition.Template[0].Literal().Bytes) != "first" {
		t.Fatalf("after the refused edit: %+v", got)
	}
	if _, err := st.Edit([16]byte{9}, 1, actDef(t, "z", nil, "true"), "operator", actNow, nil); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("edit of an unknown id: %v", err)
	}
}

func TestActionStoreEditGateRefusalWritesNothing(t *testing.T) {
	st, _ := LoadActionStore("")
	a, _ := st.Create(actDef(t, "a", nil, "true"), "operator", actNow)
	refusal := errors.New("holders unreachable")
	if _, err := st.Edit(a.Id.Id, 1, actDef(t, "a", nil, "changed"), "task:aa", actNow, func() error { return refusal }); !errors.Is(err, refusal) {
		t.Fatalf("gated edit: %v, want the gate's error", err)
	}
	got, _ := st.Get(a.Id.Id)
	if got.Revision != 1 || string(got.Definition.Template[0].Literal().Bytes) != "true" {
		t.Fatalf("a refused gate changed the action: %+v", got)
	}
}

func TestActionStoreRoundTripFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions.json")
	st, err := LoadActionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	d := actDef(t, "deploy", []string{"env=choice:staging,prod", "ref=token"}, "./deploy.sh", "--env", "{env}", "--ref", "{ref}")
	d.SetDescription([]byte("deploy a ref"))
	a, _ := st.Create(d, "operator", actNow)
	gone, _ := st.Create(actDef(t, "gone", nil, "true"), "operator", actNow)
	a, err = st.Edit(a.Id.Id, 1, d, "task:00112233445566778899aabbccddeeff", actNow.Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Remove(gone.Id.Id); err != nil {
		t.Fatal(err)
	}
	re, err := LoadActionStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	list := re.List()
	if len(list) != 1 {
		t.Fatalf("reloaded %d actions, want 1", len(list))
	}
	if !reflect.DeepEqual(list[0], a) {
		t.Fatalf("reloaded:\n%+v\nwant:\n%+v", list[0], a)
	}
	if _, ok := re.Get(gone.Id.Id); ok {
		t.Fatal("a removed action came back")
	}
	if err := re.Remove(gone.Id.Id); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
}

// Native JSON fields, never encoded wire bytes: a later .bgn change must not
// reinterpret the file.
func TestActionStoreFileIsNativeJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions.json")
	st, _ := LoadActionStore(path)
	st.Create(actDef(t, "deploy", []string{"env=choice:staging,prod"}, "./deploy.sh", "--env", "{env}"), "operator", actNow)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name": "deploy"`, `"./deploy.sh"`, `"staging"`, `"choice"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("file lacks %s:\n%s", want, raw)
		}
	}
}

func TestLoadActionStoreUnparseableIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := LoadActionStore(path); err == nil {
		t.Fatal("an unparseable store loaded")
	}
}

func TestLoadActionStoreMissingFileIsEmpty(t *testing.T) {
	st, err := LoadActionStore(filepath.Join(t.TempDir(), "actions.json"))
	if err != nil || len(st.List()) != 0 {
		t.Fatalf("missing file: %v, %d actions", err, len(st.List()))
	}
}

// List is sorted by name, so every surface shows the same order.
func TestActionStoreListSortedByName(t *testing.T) {
	st, _ := LoadActionStore("")
	for _, n := range []string{"c", "a", "b"} {
		st.Create(actDef(t, n, nil, "true"), "operator", actNow)
	}
	var names []string
	for _, r := range st.List() {
		names = append(names, string(r.Definition.Name))
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("order %v", names)
	}
}
