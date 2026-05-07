package server

import "testing"

func TestSessionRegistry_AddGetRemove(t *testing.T) {
	r := NewSessionRegistry()
	if got := r.Get("nope"); got != nil {
		t.Fatal("empty registry must return nil")
	}
	mux := &SessionMux{taskID: "t1"}
	r.Add("t1", mux)
	if got := r.Get("t1"); got != mux {
		t.Fatalf("Get returned %v want %v", got, mux)
	}
	r.Remove("t1")
	if got := r.Get("t1"); got != nil {
		t.Fatal("Remove failed")
	}
}

func TestSessionRegistry_AddReplaces(t *testing.T) {
	r := NewSessionRegistry()
	a, b := &SessionMux{taskID: "x"}, &SessionMux{taskID: "x"}
	r.Add("x", a)
	r.Add("x", b)
	if got := r.Get("x"); got != b {
		t.Fatal("Add must replace existing entry")
	}
}

func TestSessionRegistry_Snapshot(t *testing.T) {
	r := NewSessionRegistry()
	r.Add("a", &SessionMux{taskID: "a"})
	r.Add("b", &SessionMux{taskID: "b"})
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len=%d want 2", len(snap))
	}
	r.Remove("a")
	if len(snap) != 2 {
		t.Fatal("snapshot should not be affected by post-snapshot Remove")
	}
}
