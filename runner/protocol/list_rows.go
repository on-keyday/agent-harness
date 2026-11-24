package protocol

import "encoding/hex"

// FinishedStatus reports a task that ended: the history ListQuery's bound
// counts, and what every listing calls terminal.
func FinishedStatus(s TaskStatus) bool {
	switch s {
	case TaskStatus_Succeeded, TaskStatus_Failed, TaskStatus_Cancelled:
		return true
	}
	return false
}

// ListRow is what ListKeep reads of one task: its id (hex), its creator, and
// its status.
type ListRow struct {
	ID      string
	Creator TaskID
	Status  TaskStatus
}

// ListKeep is the rule ListQuery describes, over n tasks in creation order
// (row(i) for i in 0..n): keep every visible task that has not ended, the
// newest finishedLimit visible finished tasks, and every visible creator, up
// to the root, of a task kept. omitted counts the visible finished tasks not
// kept. The server answers a list with it, and the TUI cuts its table with it
// between polls, so the two cannot disagree on which rows a bound keeps.
//
// The bound counts what the caller may see, never the whole store: counted
// over the store, another tree's newer tasks would push a confined caller's
// own history out of a list it could not tell was cut. A creator kept for its
// child neither uses up the bound nor counts as omitted, so a tree drawn from
// the result never re-roots a branch because its parent fell past the bound.
func ListKeep(n int, row func(i int) ListRow, visible func(i int) bool, finishedLimit uint32) (keep []bool, omitted uint32) {
	rows := make([]ListRow, n)
	at := make(map[string]int, n)
	for i := range rows {
		rows[i] = row(i)
		at[rows[i].ID] = i
	}
	keep = make([]bool, n)
	room := uint64(finishedLimit)
	var visibleFinished, keptFinished uint64
	for i := n - 1; i >= 0; i-- {
		if !visible(i) {
			continue
		}
		if !FinishedStatus(rows[i].Status) {
			keep[i] = true
			continue
		}
		visibleFinished++
		if room > 0 {
			keep[i] = true
			keptFinished++
			room--
		}
	}
	for i := range rows {
		if !keep[i] {
			continue
		}
		for c := rows[i].Creator.Id; c != ([16]byte{}); {
			j, ok := at[hex.EncodeToString(c[:])]
			if !ok || keep[j] || !visible(j) {
				break
			}
			keep[j] = true
			if FinishedStatus(rows[j].Status) {
				// Kept for its child, so not left out: it leaves the omitted
				// count without having used the bound.
				keptFinished++
			}
			c = rows[j].Creator.Id
		}
	}
	gone := visibleFinished - keptFinished
	if gone > 0xffffffff {
		gone = 0xffffffff
	}
	return keep, uint32(gone)
}
