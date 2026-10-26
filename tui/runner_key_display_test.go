package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The Key column comes and goes with the panel width; the rows must swap with
// it (checklist item 34), and the cursor must survive the swap.
func TestRunnerTableKeyColumnSwapsWithRows(t *testing.T) {
	m := NewRunners()
	var rs []protocol.RunnerInfo
	for i := 0; i < 3; i++ {
		r := protocol.RunnerInfo{Id: protocol.RunnerID{Id: [16]byte{byte(i + 1)}}}
		r.SetHostname([]byte("gmkhost"))
		r.SetKeyLabel([]byte("gmk"))
		rs = append(rs, r)
	}
	m.SetRows(rs)
	m.SetSize(200, 10)
	m.table.SetCursor(2)
	for _, w := range []int{200, 60, keyColumnMinWidth - 1, keyColumnMinWidth, 200} {
		m.SetSize(w, 10)
		cols, rows := m.table.Columns(), m.table.Rows()
		for _, row := range rows {
			if len(row) != len(cols) {
				t.Fatalf("w=%d: %d cells against %d columns", w, len(row), len(cols))
			}
		}
		has := false
		for i, c := range cols {
			if c.Title == "Key" {
				has = true
				if rows[0][i] != "gmk" {
					t.Errorf("w=%d: Key cell %q, want gmk", w, rows[0][i])
				}
			}
		}
		if want := w >= keyColumnMinWidth; has != want {
			t.Errorf("w=%d: Key column present=%v, want %v", w, has, want)
		}
		if m.table.Cursor() != 2 {
			t.Errorf("w=%d: cursor moved to %d", w, m.table.Cursor())
		}
	}
}

func TestRunnerDetailShowsKey(t *testing.T) {
	r := protocol.RunnerInfo{}
	r.SetLegacyAuth(true)
	if !strings.Contains(formatRunnerDetail(r), "key:           legacy") {
		t.Errorf("legacy runner detail:\n%s", formatRunnerDetail(r))
	}
	r = protocol.RunnerInfo{}
	r.Key[0] = 0xab
	r.SetKeyLabel([]byte("gmk"))
	if d := formatRunnerDetail(r); !strings.Contains(d, "key:           gmk (ab00000000000000") {
		t.Errorf("enrolled runner detail:\n%s", d)
	}
}

func TestConnRowStateMarksLegacy(t *testing.T) {
	ci := &protocol.ConnInfo{Role: protocol.ConnRole_Agent}
	ci.SetCid([]byte("ws:127.0.0.1:1-1"))
	ci.SetIdentified(true)
	ci.SetLegacyAuth(true)
	if row := connInfoToRow(ci); row[len(row)-1] != "legacy" {
		t.Errorf("state %q, want legacy", row[len(row)-1])
	}
}
