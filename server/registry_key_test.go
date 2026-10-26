package server

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

// A runner's own reconnect (same key) takes its identity over, as before.
func TestRegistrySameKeyTakeoverStillWorks(t *testing.T) {
	reg := NewRegistry()
	key, _, _ := ed25519.GenerateKey(nil)
	id := testRunnerID("ws:127.0.0.1:8539-1")
	first := tcid("ws:127.0.0.1:8539-1")
	second := tcid("ws:127.0.0.1:8539-2")
	if _, err := reg.Add(&RunnerEntry{ID: first, Identity: id, Key: key}); err != nil {
		t.Fatal(err)
	}
	displaced, err := reg.Add(&RunnerEntry{ID: second, Identity: id, Key: key})
	if err != nil || displaced != first {
		t.Fatalf("same-key reconnect: displaced=%v err=%v, want %v, nil", displaced, err, first)
	}
}

// Another key cannot take a live identity: it would inherit tickets the board
// keys to that identity.
func TestRegistryRefusesTakeoverByDifferentKey(t *testing.T) {
	reg := NewRegistry()
	k1, _, _ := ed25519.GenerateKey(nil)
	k2, _, _ := ed25519.GenerateKey(nil)
	id := testRunnerID("ws:127.0.0.1:8539-1")
	first := tcid("ws:127.0.0.1:8539-1")
	reg.Add(&RunnerEntry{ID: first, Identity: id, Key: k1})
	_, err := reg.Add(&RunnerEntry{ID: tcid("ws:127.0.0.1:8539-2"), Identity: id, Key: k2})
	if !errors.Is(err, ErrIdentityKeyMismatch) {
		t.Fatalf("different key: err=%v, want ErrIdentityKeyMismatch", err)
	}
	if cur, ok := reg.GetByIdentity(id); !ok || cur.ID != first {
		t.Fatalf("the original holder lost the identity: %+v ok=%v", cur.ID, ok)
	}
	if _, ok := reg.Get(tcid("ws:127.0.0.1:8539-2")); ok {
		t.Fatal("the refused connection was registered anyway")
	}
}
