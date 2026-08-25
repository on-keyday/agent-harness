package server

import "testing"

func TestExecRegistry(t *testing.T) {
	r := newExecRegistry()

	// Ids start at 1: 0 must never be a real id, so a zero-valued field is
	// unambiguously "none" — the forward registry's counter draws the same line.
	id1 := r.add(&execRun{taskIDHex: "aaaa", argv: []string{"ls"}})
	if id1 != 1 {
		t.Errorf("first id = %d, want 1", id1)
	}
	id2 := r.add(&execRun{taskIDHex: "bbbb", argv: []string{"true"}})
	if id2 != 2 {
		t.Errorf("second id = %d, want 2", id2)
	}

	if _, ok := r.get(id1); !ok {
		t.Error("get on a live id must find it")
	}
	if n := r.countForTask("aaaa"); n != 1 {
		t.Errorf("countForTask(aaaa) = %d, want 1", n)
	}
	if n := r.countForTask("cccc"); n != 0 {
		t.Errorf("countForTask on a task with none = %d, want 0", n)
	}
	if got := len(r.list("")); got != 2 {
		t.Errorf("list(all) = %d entries, want 2", got)
	}
	if got := r.list("aaaa"); len(got) != 1 || got[0].taskIDHex != "aaaa" {
		t.Errorf("list(aaaa) = %+v, want just the aaaa entry", got)
	}

	if _, ok := r.remove(id1); !ok {
		t.Error("remove on a live id must report it")
	}
	if _, ok := r.get(id1); ok {
		t.Error("a removed entry must be gone")
	}
	// Removing twice is not an error: a runner can report a finish for an exec
	// whose client already went away and took the registration with it.
	if _, ok := r.remove(id1); ok {
		t.Error("removing twice must report not-found, not succeed")
	}
	if n := r.countForTask("aaaa"); n != 0 {
		t.Errorf("countForTask after removal = %d, want 0", n)
	}
}

// The listing must be ordered so two calls agree: the operator reads it as a
// table, and an unstable order makes a stable set look like it is churning.
func TestExecRegistryListIsOrdered(t *testing.T) {
	r := newExecRegistry()
	for i := 0; i < 5; i++ {
		r.add(&execRun{taskIDHex: "aaaa", argv: []string{"x"}})
	}
	first := r.list("")
	second := r.list("")
	if len(first) != 5 {
		t.Fatalf("list returned %d entries, want 5", len(first))
	}
	for i := range first {
		if first[i].execID != second[i].execID {
			t.Fatalf("list order differs between calls at %d", i)
		}
		if i > 0 && first[i-1].execID > first[i].execID {
			t.Fatalf("list is not ascending by id at %d", i)
		}
	}
}

// The lazy accessor must hand back the SAME registry every time, or two
// callers would register into different maps and each would see half the
// execs.
func TestExecsAccessorIsStable(t *testing.T) {
	h := &TaskHandler{}
	a := h.execs()
	b := h.execs()
	if a != b {
		t.Error("execs() returned two different registries")
	}
	id := a.add(&execRun{taskIDHex: "aaaa"})
	if _, ok := b.get(id); !ok {
		t.Error("an entry added through one accessor is invisible through another")
	}
}
