package server

import (
	"log/slog"
	"sort"
	"time"
)

// RestoreFromWAL puts back task records a prune forgot.
//
// A prune deletes the TaskEntry and its log file, and `submit --resume <id>`
// answers resume_not_found afterwards because handleSubmitResume's first
// precondition is that the entry still exists. So the sweep had no undo, and
// its widest form was the bare verb -- which is how 22 records went in one
// mistyped line meant to print a usage string.
//
// The WAL still holds everything: task_created carries the repo, the prompt,
// the kind, the origin, the creator link, the capabilities and the scope, and
// the events after it carry the assignment and the outcome. Replaying that
// history with the task_pruned records DROPPED reconstructs exactly the entry
// the prune removed.
//
// Reconstruction goes through ReplayEvents on a scratch store rather than a
// second reading of the WAL here. There is one interpretation of these events
// -- the one the server boots from -- and a restore that re-implemented it
// would drift from it silently, entry by entry, exactly the way three
// hand-written verb parsers drifted.
//
// What it does NOT do:
//
//   - Invent a task. Every field comes from a record the server itself wrote.
//     An id with no task_created is reported as not_in_wal.
//   - Overwrite a live task. An id already in the store is left alone and
//     reported as already_present, so a restore can never clobber the running
//     state with an older snapshot of it.
//   - Bring back the task LOG. PruneTerminal removes <logDir>/<id>.log and the
//     WAL never held its contents. The record returns; the transcript does not.
//   - Bring back the worktree. That is prune-local's half and lives on the
//     runner, untouched by a server-side prune.
//
// Restorable is one task a prune forgot, with enough to tell which it was.
type Restorable struct {
	TaskID    string
	PrunedAt  time.Time
	CreatedAt time.Time
	RepoPath  string
	Prompt    string
}

// RestorableFromWAL lists what a restore could put back, newest prune first.
//
// Without it the restore verb is unusable: it takes ids, and the ids of
// forgotten tasks live in a file on the server host that no surface reads. An
// operator who did not write the id down before pruning had no way to learn
// it -- which is every operator who pruned by accident, the case the verb
// exists for.
//
// Reported straight off the WAL rather than from a replay, because the
// question is about records the store no longer holds: which task_pruned has
// no task_restored after it, and what the task_created before it said.
func RestorableFromWAL(events []WALEvent, live func(id string) bool) []Restorable {
	type meta struct {
		created          time.Time
		repo, prompt     string
		prunedAt         time.Time
		pruned, restored bool
	}
	byID := map[string]*meta{}
	order := []string{}
	get := func(id string) *meta {
		m, ok := byID[id]
		if !ok {
			m = &meta{}
			byID[id] = m
			order = append(order, id)
		}
		return m
	}
	for _, ev := range events {
		if ev.TaskID == "" {
			continue
		}
		switch ev.Type {
		case "task_created":
			m := get(ev.TaskID)
			m.created = time.Unix(0, ev.Ts)
			m.repo, m.prompt = ev.RepoPath, ev.Prompt
			// A resume writes another task_created for the same id, and it
			// also un-prunes nothing -- the flags are per-id state, so a
			// later create leaves an earlier prune standing until a restore
			// or another prune moves it.
		case "task_pruned":
			m := get(ev.TaskID)
			m.pruned, m.restored = true, false
			m.prunedAt = time.Unix(0, ev.Ts)
		case "task_restored":
			get(ev.TaskID).restored = true
		}
	}
	var out []Restorable
	for _, id := range order {
		m := byID[id]
		if !m.pruned || m.restored {
			continue
		}
		if live != nil && live(id) {
			continue // already back by some other path; not a candidate
		}
		out = append(out, Restorable{
			TaskID: id, PrunedAt: m.prunedAt, CreatedAt: m.created,
			RepoPath: m.repo, Prompt: m.prompt,
		})
	}
	// Newest prune first: the accident you are undoing is the last one.
	sort.Slice(out, func(i, j int) bool { return out[i].PrunedAt.After(out[j].PrunedAt) })
	return out
}

// Live reports whether an id is in the store, for RestorableFromWAL.
func (s *TaskStore) Live(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.tasks[id]
	return ok
}

func (s *TaskStore) RestoreFromWAL(events []WALEvent, ids []string) (restored, alreadyPresent, notInWAL []string) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}

	// The wanted ids' history, with the prunes taken out. Everything else about
	// each task replays as it always does -- including a later task_created for
	// a resumed id, which is why the filter keeps ORDER rather than picking the
	// first record it finds.
	var history []WALEvent
	seenCreate := map[string]bool{}
	for _, ev := range events {
		if !want[ev.TaskID] || ev.Type == "task_pruned" {
			continue
		}
		if ev.Type == "task_created" {
			seenCreate[ev.TaskID] = true
		}
		history = append(history, ev)
	}

	scratch := NewTaskStore()
	scratch.ReplayEvents(history)

	s.mu.Lock()
	now := time.Now().UnixNano()
	for _, id := range ids {
		if _, live := s.tasks[id]; live {
			alreadyPresent = append(alreadyPresent, id)
			continue
		}
		if !seenCreate[id] {
			notInWAL = append(notInWAL, id)
			continue
		}
		rebuilt, ok := scratch.tasks[id]
		if !ok {
			// A create was seen and the replay still dropped the entry. The
			// only path that does that is a task_pruned, and those are
			// filtered out above -- so this is a WAL the replay reads
			// differently than this function expects, and inventing an entry
			// here is exactly what the docstring says it will not do.
			notInWAL = append(notInWAL, id)
			continue
		}
		clone := *rebuilt
		s.tasks[id] = &clone
		s.order = append(s.order, id)
		restored = append(restored, id)
		if s.wal != nil {
			if err := s.wal.Write(WALEvent{Type: "task_restored", TaskID: id, Ts: now}); err != nil {
				slog.Error("WAL write failed", "op", "task_restored", "task_id", id, "err", err)
			}
		}
	}
	s.mu.Unlock()
	return restored, alreadyPresent, notInWAL
}
