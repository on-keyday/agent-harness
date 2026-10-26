package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunnerKeyRecord is one enrolled runner key. The server knows keys only,
// never machines or slots: any number of runners may authenticate with one.
type RunnerKeyRecord struct {
	Key        ed25519.PublicKey
	Label      string
	EnrolledAt time.Time
	EnrolledBy string // "operator" or "task:<32 hex>"
}

type runnerKeyJSON struct {
	Key        string    `json:"key"`
	Label      string    `json:"label"`
	EnrolledAt time.Time `json:"enrolled_at"`
	EnrolledBy string    `json:"enrolled_by"`
}

var (
	ErrKeyNotFound     = errors.New("no enrolled key has that prefix")
	ErrAmbiguousPrefix = errors.New("more than one enrolled key has that prefix")
	ErrPrefixTooShort  = errors.New("a key prefix needs at least 8 hex characters")
)

// RunnerKeyStore is the set of keys the server lets register under
// AuthRole.runner, persisted as <data-dir>/runner-keys.json. It is not in the
// WAL: nothing in a task record refers to it.
type RunnerKeyStore struct {
	mu   sync.Mutex
	path string // "" = memory only
	keys map[string]RunnerKeyRecord
}

// LoadRunnerKeyStore reads path (absent = empty) and adds initial on top.
// A file that does not parse is an error: starting with an empty set would
// shut out every runner, silently.
func LoadRunnerKeyStore(path string, initial []RunnerKeyRecord) (*RunnerKeyStore, error) {
	st := &RunnerKeyStore{path: path, keys: map[string]RunnerKeyRecord{}}
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			var rows []runnerKeyJSON
			if err := json.Unmarshal(data, &rows); err != nil {
				return nil, fmt.Errorf("runner keys %s: %w", path, err)
			}
			for _, r := range rows {
				k, err := hex.DecodeString(r.Key)
				if err != nil || len(k) != ed25519.PublicKeySize {
					return nil, fmt.Errorf("runner keys %s: key %q is not a hex Ed25519 public key", path, r.Key)
				}
				st.keys[r.Key] = RunnerKeyRecord{Key: k, Label: r.Label, EnrolledAt: r.EnrolledAt, EnrolledBy: r.EnrolledBy}
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("runner keys %s: %w", path, err)
		}
	}
	for _, r := range initial {
		st.keys[hex.EncodeToString(r.Key)] = r
	}
	return st, nil
}

func (st *RunnerKeyStore) Lookup(pub ed25519.PublicKey) (RunnerKeyRecord, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.keys[hex.EncodeToString(pub)]
	return r, ok
}

// Enroll adds rec. An already enrolled key keeps its record, except that a
// non-empty rec.Label replaces the label; added reports which happened.
func (st *RunnerKeyStore) Enroll(rec RunnerKeyRecord) (added bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	id := hex.EncodeToString(rec.Key)
	if cur, ok := st.keys[id]; ok {
		if rec.Label == "" || rec.Label == cur.Label {
			return false, nil
		}
		cur.Label = rec.Label
		st.keys[id] = cur
		return false, st.saveLocked()
	}
	st.keys[id] = rec
	return true, st.saveLocked()
}

// Resolve finds the one enrolled key whose hex starts with prefixHex.
func (st *RunnerKeyStore) Resolve(prefixHex string) (RunnerKeyRecord, error) {
	p := strings.ToLower(strings.TrimSpace(prefixHex))
	if len(p) < 8 {
		return RunnerKeyRecord{}, ErrPrefixTooShort
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var found []RunnerKeyRecord
	for id, r := range st.keys {
		if strings.HasPrefix(id, p) {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return RunnerKeyRecord{}, ErrKeyNotFound
	case 1:
		return found[0], nil
	}
	return RunnerKeyRecord{}, ErrAmbiguousPrefix
}

func (st *RunnerKeyStore) Remove(pub ed25519.PublicKey) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	id := hex.EncodeToString(pub)
	if _, ok := st.keys[id]; !ok {
		return ErrKeyNotFound
	}
	delete(st.keys, id)
	return st.saveLocked()
}

// List returns every record, oldest enrollment first.
func (st *RunnerKeyStore) List() []RunnerKeyRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]RunnerKeyRecord, 0, len(st.keys))
	for _, r := range st.keys {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EnrolledAt.Equal(out[j].EnrolledAt) {
			return out[i].EnrolledAt.Before(out[j].EnrolledAt)
		}
		return bytes.Compare(out[i].Key, out[j].Key) < 0
	})
	return out
}

func (st *RunnerKeyStore) saveLocked() error {
	if st.path == "" {
		return nil
	}
	rows := make([]runnerKeyJSON, 0, len(st.keys))
	for id, r := range st.keys {
		rows = append(rows, runnerKeyJSON{Key: id, Label: r.Label, EnrolledAt: r.EnrolledAt, EnrolledBy: r.EnrolledBy})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("runner keys: %w", err)
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return fmt.Errorf("runner keys: %w", err)
	}
	return nil
}
