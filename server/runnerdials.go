package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"
)

// RunnerDialRecord is one runner the server keeps dialing
// (docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md).
// Candidates are kept as the text that was typed, so every attempt resolves
// them again: a host name follows a cloud host whose address changes.
type RunnerDialRecord struct {
	ID         uint32
	Candidates []string
	Key        ed25519.PublicKey // the enrolled key a connection for this record must authenticate with
	Label      string
	AddedAt    time.Time
	AddedBy    string // "operator" or "task:<32 hex>"
}

type runnerDialJSON struct {
	ID         uint32    `json:"id"`
	Candidates []string  `json:"candidates"`
	Key        string    `json:"key"`
	Label      string    `json:"label"`
	AddedAt    time.Time `json:"added_at"`
	AddedBy    string    `json:"added_by"`
}

var ErrDialNotFound = errors.New("no dial record has that id")

// RunnerDialStore is the set of dial records, persisted as
// <data-dir>/runner-dials.json beside runner-keys.json and the same way. It is
// not in the WAL: nothing in a task record refers to a dial record.
type RunnerDialStore struct {
	mu   sync.Mutex
	path string // "" = memory only
	recs map[uint32]RunnerDialRecord
}

// LoadRunnerDialStore reads path (absent = empty). A file that does not parse
// is an error: starting with no records would silently stop dialing runners.
func LoadRunnerDialStore(path string) (*RunnerDialStore, error) {
	st := &RunnerDialStore{path: path, recs: map[uint32]RunnerDialRecord{}}
	if path == "" {
		return st, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, fmt.Errorf("runner dials %s: %w", path, err)
	}
	var rows []runnerDialJSON
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("runner dials %s: %w", path, err)
	}
	for _, r := range rows {
		k, err := hex.DecodeString(r.Key)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("runner dials %s: record %08x: key %q is not a hex Ed25519 public key", path, r.ID, r.Key)
		}
		if r.ID == 0 {
			return nil, fmt.Errorf("runner dials %s: a record has id 0", path)
		}
		st.recs[r.ID] = RunnerDialRecord{ID: r.ID, Candidates: r.Candidates, Key: k,
			Label: r.Label, AddedAt: r.AddedAt, AddedBy: r.AddedBy}
	}
	return st, nil
}

// Add stores rec under a fresh random non-zero id, which it returns in the
// stored record.
func (st *RunnerDialStore) Add(rec RunnerDialRecord) (RunnerDialRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return RunnerDialRecord{}, err
		}
		id := binary.BigEndian.Uint32(b[:])
		if _, taken := st.recs[id]; id != 0 && !taken {
			rec.ID = id
			break
		}
	}
	rec.Candidates = append([]string(nil), rec.Candidates...)
	rec.Key = append(ed25519.PublicKey(nil), rec.Key...)
	st.recs[rec.ID] = rec
	if err := st.saveLocked(); err != nil {
		delete(st.recs, rec.ID)
		return RunnerDialRecord{}, err
	}
	return rec, nil
}

func (st *RunnerDialStore) Get(id uint32) (RunnerDialRecord, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.recs[id]
	return r, ok
}

func (st *RunnerDialStore) Remove(id uint32) (RunnerDialRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.recs[id]
	if !ok {
		return RunnerDialRecord{}, ErrDialNotFound
	}
	delete(st.recs, id)
	if err := st.saveLocked(); err != nil {
		st.recs[id] = r // the disk still has it: so must memory
		return RunnerDialRecord{}, err
	}
	return r, nil
}

// RemoveByKey removes every record bound to key (runner unenroll's cascade)
// and returns them, oldest first.
func (st *RunnerDialStore) RemoveByKey(key ed25519.PublicKey) []RunnerDialRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []RunnerDialRecord
	for id, r := range st.recs {
		if r.Key.Equal(key) {
			out = append(out, r)
			delete(st.recs, id)
		}
	}
	if len(out) > 0 {
		if err := st.saveLocked(); err != nil {
			for _, r := range out { // the disk still has them: so must memory
				st.recs[r.ID] = r
			}
			slog.Error("runner dials: removing a key's records failed; they stay", "err", err)
			return nil
		}
	}
	sortDialRecords(out)
	return out
}

// List returns every record, oldest first.
func (st *RunnerDialStore) List() []RunnerDialRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]RunnerDialRecord, 0, len(st.recs))
	for _, r := range st.recs {
		out = append(out, r)
	}
	sortDialRecords(out)
	return out
}

// OwnerOfCandidate names the record whose candidates include text exactly.
func (st *RunnerDialStore) OwnerOfCandidate(text string) (uint32, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, r := range st.recs {
		for _, c := range r.Candidates {
			if c == text {
				return id, true
			}
		}
	}
	return 0, false
}

func sortDialRecords(rs []RunnerDialRecord) {
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].AddedAt.Equal(rs[j].AddedAt) {
			return rs[i].AddedAt.Before(rs[j].AddedAt)
		}
		return rs[i].ID < rs[j].ID
	})
}

func (st *RunnerDialStore) saveLocked() error {
	if st.path == "" {
		return nil
	}
	rows := make([]runnerDialJSON, 0, len(st.recs))
	for _, r := range st.recs {
		rows = append(rows, runnerDialJSON{ID: r.ID, Candidates: r.Candidates, Key: hex.EncodeToString(r.Key),
			Label: r.Label, AddedAt: r.AddedAt, AddedBy: r.AddedBy})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("runner dials: %w", err)
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return fmt.Errorf("runner dials: %w", err)
	}
	return nil
}
