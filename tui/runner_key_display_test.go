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
	r.Key[0] = 0xab
	r.SetKeyLabel([]byte("gmk"))
	if d := formatRunnerDetail(r); !strings.Contains(d, "key:           gmk (ab00000000000000") {
		t.Errorf("enrolled runner detail:\n%s", d)
	}
}

// The Link column joins at linkColumnMinWidth, before Roots, whole, and swaps
// with its rows like Key (checklist 34).
func TestRunnerTableLinkColumnAtItsThreshold(t *testing.T) {
	m := NewRunners()
	r := protocol.RunnerInfo{Id: protocol.RunnerID{Id: [16]byte{1}}, Link: protocol.RunnerLink_DialRecord, LinkRecord: 0x3fa9c012}
	r.SetHostname([]byte("cloud"))
	m.SetRows([]protocol.RunnerInfo{r})
	for _, w := range []int{200, linkColumnMinWidth - 1, linkColumnMinWidth, 60, 200} {
		m.SetSize(w, 10)
		cols, rows := m.table.Columns(), m.table.Rows()
		if len(rows[0]) != len(cols) {
			t.Fatalf("w=%d: %d cells against %d columns", w, len(rows[0]), len(cols))
		}
		at := -1
		for i, c := range cols {
			if c.Title == "Link" {
				at = i
			}
		}
		if want := w >= linkColumnMinWidth; (at >= 0) != want {
			t.Fatalf("w=%d: Link column present=%v, want %v", w, at >= 0, want)
		}
		if at >= 0 {
			if rows[0][at] != "dial:3fa9c012" {
				t.Errorf("w=%d: Link cell %q", w, rows[0][at])
			}
			// The widest value, dial:<8 hex>, must fit without truncation:
			// a cut id cannot be typed into runner dial rm (seen live at 12).
			if cols[at].Width < len("dial:3fa9c012") {
				t.Errorf("w=%d: Link column %d cells, the widest value needs %d", w, cols[at].Width, len("dial:3fa9c012"))
			}
			if cols[at+1].Title != "Roots" {
				t.Errorf("w=%d: Link sits before %q, want Roots", w, cols[at+1].Title)
			}
		}
	}
}

func TestRunnerDetailShowsLink(t *testing.T) {
	r := protocol.RunnerInfo{Link: protocol.RunnerLink_DialRecord, LinkRecord: 0x3fa9c012}
	if d := formatRunnerDetail(r); !strings.Contains(d, "link:          dial:3fa9c012") {
		t.Errorf("detail:\n%s", d)
	}
	r = protocol.RunnerInfo{Link: protocol.RunnerLink_Via}
	r.LinkVia.Id[0] = 0xab
	if d := formatRunnerDetail(r); !strings.Contains(d, "link:          via:ab000000 (relay ab000000000000000000000000000000)") {
		t.Errorf("via detail:\n%s", d)
	}
}
