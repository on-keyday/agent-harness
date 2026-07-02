package tui

import "testing"

func TestPopupSetRepoChoicesPrependsCurrent(t *testing.T) {
	var p PopupModel
	p.SetRepoChoices([]string{"/a", "/b"}, "/x")
	got := p.Repo()
	if got != "/x" {
		t.Errorf("Repo()=%q, want /x (current must be first)", got)
	}
}

func TestPopupSetRepoChoicesDeduplicatesCurrent(t *testing.T) {
	var p PopupModel
	p.SetRepoChoices([]string{"/a", "/b", "/c"}, "/b")
	if got := p.Repo(); got != "/b" {
		t.Errorf("Repo()=%q, want /b", got)
	}
	p.CycleRepo(+1)
	if got := p.Repo(); got != "/a" {
		t.Errorf("after +1 Repo()=%q, want /a (no duplicate /b)", got)
	}
	p.CycleRepo(+1)
	if got := p.Repo(); got != "/c" {
		t.Errorf("after +2 Repo()=%q, want /c", got)
	}
}

func TestPopupSetRepoChoicesSkipsEmpty(t *testing.T) {
	var p PopupModel
	p.SetRepoChoices([]string{"", "/a", ""}, "")
	if got := p.Repo(); got != "/a" {
		t.Errorf("Repo()=%q, want /a (empties dropped)", got)
	}
}

func TestPopupSetRepoChoicesEmptyEverything(t *testing.T) {
	var p PopupModel
	p.SetRepoChoices(nil, "")
	if got := p.Repo(); got != "" {
		t.Errorf("Repo()=%q, want empty", got)
	}
	p.CycleRepo(+1)
	if got := p.Repo(); got != "" {
		t.Errorf("after CycleRepo Repo()=%q, want empty (no-op when 0 choices)", got)
	}
}

func TestPopupCycleRepoWraps(t *testing.T) {
	var p PopupModel
	p.SetRepoChoices([]string{"/a", "/b", "/c"}, "")
	if got := p.Repo(); got != "/a" {
		t.Errorf("Repo()=%q, want /a", got)
	}
	p.CycleRepo(-1)
	if got := p.Repo(); got != "/c" {
		t.Errorf("after -1 Repo()=%q, want /c (wrap)", got)
	}
	p.CycleRepo(+1)
	if got := p.Repo(); got != "/a" {
		t.Errorf("after +1 Repo()=%q, want /a", got)
	}
}

func TestPopupSetRepoSingle(t *testing.T) {
	var p PopupModel
	p.SetRepo("/a")
	if got := p.Repo(); got != "/a" {
		t.Errorf("Repo()=%q, want /a", got)
	}
	p.CycleRepo(+1)
	if got := p.Repo(); got != "/a" {
		t.Errorf("after CycleRepo Repo()=%q, want /a (no-op when 1 choice)", got)
	}
	p.SetRepo("")
	if got := p.Repo(); got != "" {
		t.Errorf("after SetRepo(\"\") Repo()=%q, want empty", got)
	}
}

func TestPopupResumeConversationToggle(t *testing.T) {
	p := NewPopup("/repo")
	p.Open()
	if p.ResumeConversation() {
		t.Fatal("ResumeConversation default = true, want false")
	}
	p.ToggleResumeConversation()
	if !p.ResumeConversation() {
		t.Fatal("ResumeConversation after toggle = false, want true")
	}
	p.Open()
	if p.ResumeConversation() {
		t.Fatal("ResumeConversation should reset when popup opens")
	}
}
