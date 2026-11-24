package server

import (
	"math"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// listRows picks a list's task rows from all (the store, in insertion order)
// by protocol.ListKeep, which holds the rule, and keeps them in that order.
// omitted is ListResultBody.finished_omitted.
func listRows(all []TaskEntry, visible func(id string) bool, finishedLimit uint32) (rows []TaskEntry, omitted uint32) {
	keep, omitted := protocol.ListKeep(len(all), func(i int) protocol.ListRow {
		return protocol.ListRow{ID: all[i].ID, Creator: all[i].CreatorTaskID, Status: all[i].Status}
	}, func(i int) bool { return visible(all[i].ID) }, finishedLimit)
	for i := range all {
		if keep[i] {
			rows = append(rows, all[i])
		}
	}
	// ListResultBody.tasks_len is a u16. Past it the encode would fail and
	// the caller would get no list at all, so the oldest finished rows go
	// first (counted as omitted) — a store that large is far past anything
	// this list was built for, and a short list beats none.
	if excess := len(rows) - maxListRows; excess > 0 {
		trimmed := rows[:0]
		for _, r := range rows {
			if excess > 0 && protocol.FinishedStatus(r.Status) {
				excess--
				if omitted < math.MaxUint32 {
					omitted++
				}
				continue
			}
			trimmed = append(trimmed, r)
		}
		if len(trimmed) > maxListRows {
			trimmed = trimmed[:maxListRows]
		}
		rows = trimmed
	}
	return rows, omitted
}

// maxListRows is tasks_len's ceiling less the one row handleList may prepend
// (a confined caller's parent hop).
const maxListRows = math.MaxUint16 - 1
