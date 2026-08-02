package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

// TestEditReopenCycle walks the whole operator-visible loop with the transfer
// factored out: load bytes, edit them in the popup, encode what would be
// pushed, then load THOSE bytes again. The reopen is the assertion — a file
// this editor just wrote must never come back as "not editable text".
func TestEditReopenCycle(t *testing.T) {
	body := "これは日本語のメモです。\nsecond line\n三行目\n"
	cases := []struct {
		name string
		orig []byte
	}{
		{"lf", []byte(body)},
		{"crlf", []byte(strings.ReplaceAll(body, "\n", "\r\n"))},
		{"bom_lf", append([]byte{0xEF, 0xBB, 0xBF}, []byte(body)...)},
		{"bom_crlf", append([]byte{0xEF, 0xBB, 0xBF}, []byte(strings.ReplaceAll(body, "\n", "\r\n"))...)},
		{"no_trailing_newline", []byte("一行だけ")},
		{"empty", []byte("")},
	}
	edits := []struct {
		name string
		keys []tea.KeyMsg
	}{
		{"append text", []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("追記")}}},
		{"newline then text", []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune("new")}}},
		{"backspace", []tea.KeyMsg{{Type: tea.KeyBackspace}}},
		{"delete everything", []tea.KeyMsg{
			{Type: tea.KeyBackspace}, {Type: tea.KeyBackspace}, {Type: tea.KeyBackspace},
			{Type: tea.KeyBackspace}, {Type: tea.KeyBackspace}, {Type: tea.KeyBackspace},
			{Type: tea.KeyBackspace}, {Type: tea.KeyBackspace}, {Type: tea.KeyBackspace},
			{Type: tea.KeyBackspace}, {Type: tea.KeyBackspace}, {Type: tea.KeyBackspace},
		}},
		{"space", []tea.KeyMsg{{Type: tea.KeySpace}}},
		{"no edit at all", nil},
	}

	for _, tc := range cases {
		for _, ed := range edits {
			t.Run(tc.name+"/"+ed.name, func(t *testing.T) {
				doc, err := cli.NewFileEditDocForTest("memo.txt", tc.orig)
				if err != nil {
					t.Fatalf("first load rejected the fixture: %v", err)
				}
				m := NewFileEdit()
				m.SetSize(80, 24)
				m.OpenEdit("task", doc)
				for _, k := range ed.keys {
					m, _ = m.Update(k)
				}
				next := doc.Encode(m.Text())

				// The reopen: this is what the operator does, and what failed.
				if _, err := cli.NewFileEditDocForTest("memo.txt", next); err != nil {
					t.Fatalf("reopening what we just saved failed: %v\nbytes: %q", err, next)
				}
			})
		}
	}
}
