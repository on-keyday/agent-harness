package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// One header per line — the field is a textarea precisely so no separator
// syntax has to be invented.
func TestHTTPFormSpecSplitsHeadersByLine(t *testing.T) {
	f := newHTTPForm()
	f.setForTest("POST", "/api", "Accept: application/json\nX-Trace: 1", `{"a":1}`)

	spec := f.Spec()
	if spec.Method != "POST" || spec.Path != "/api" {
		t.Fatalf("spec = %+v", spec)
	}
	if len(spec.Headers) != 2 || spec.Headers[1] != "X-Trace: 1" {
		t.Errorf("headers = %#v", spec.Headers)
	}
	if string(spec.Body) != `{"a":1}` {
		t.Errorf("body = %q", spec.Body)
	}
}

func TestHTTPFormSkipsBlankHeaderLines(t *testing.T) {
	f := newHTTPForm()
	f.setForTest("GET", "/", "\n\nAccept: */*\n\n", "")
	if got := f.Spec().Headers; len(got) != 1 || got[0] != "Accept: */*" {
		t.Errorf("headers = %#v, want one entry", got)
	}
}

func TestHTTPFormCycleMethodWraps(t *testing.T) {
	f := newHTTPForm()
	f.CycleMethod(-1)
	if got := f.Spec().Method; got != httpMethods[len(httpMethods)-1] {
		t.Errorf("cycling back from the first method gave %q", got)
	}
	f.CycleMethod(+1)
	if got := f.Spec().Method; got != httpMethods[0] {
		t.Errorf("cycling forward again gave %q", got)
	}
}

// The form is a mode of the active pane: with no pane there is nothing to send
// to, and toggling must not pretend otherwise.
func TestRawModalFormModeNeedsAPane(t *testing.T) {
	m := NewRawConnectModal()
	m.Show("task-1")
	m.ToggleForm()
	if m.InForm() {
		t.Fatal("form opened on the [+ new] slot, where there is no connection")
	}
	m.AddPane("task-1", "127.0.0.1", 8080, 1)
	m.ToggleForm()
	if !m.InForm() {
		t.Fatal("form did not open on a pane")
	}
	m.ToggleForm()
	if m.InForm() {
		t.Fatal("second toggle did not return to byte entry")
	}
}

// A build error must be reported without touching the connection: the pane
// stays live and nothing is sent.
func TestRawModalSendFormReportsBuildError(t *testing.T) {
	m := NewRawConnectModal()
	m.Show("task-1")
	m.AddPane("task-1", "127.0.0.1", 8080, 1)
	m.SetConn(1, nil, nil, "connected (fwd 1)")
	m.ToggleForm()
	m.SetFormForTest("GET", "relative-path", "", "")

	err := m.SendForm()
	if err == nil {
		t.Fatal("a relative path must not build")
	}
	if !strings.Contains(err.Error(), "must start with /") {
		t.Errorf("unhelpful error: %v", err)
	}
	if p := m.ActivePane(); p == nil || !p.live {
		t.Error("a build error must not close the pane")
	}
}

// Enter in form mode must send the BUILT REQUEST, not the target input line.
// The byte-entry Enter case runs first in app.go's key switch, so a missing
// guard there silently sends the wrong bytes — and with a nil conn the two
// paths are distinguishable: the form reports on the pane and leaves it live,
// while the byte-entry send's failure marks it closed.
func TestRawModalFormEnterSendsTheRequest(t *testing.T) {
	a := New(Config{})
	a.rawModal.Show("task-1")
	a.rawModal.AddPane("task-1", "127.0.0.1", 8080, 1)
	a.rawModal.SetConn(1, nil, nil, "connected (fwd 1)")
	a.rawModal.SetSpec("this-is-the-target-line")
	a.rawModal.ToggleForm()
	a.rawModal.SetFormForTest("GET", "/healthz", "", "")

	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = m.(*App)

	p := a.rawModal.ActivePane()
	if p == nil {
		t.Fatal("pane vanished")
	}
	if !p.live {
		t.Fatalf("Enter took the byte-entry path (pane closed with %q)", p.note)
	}
	// The discriminator is live-vs-closed above: the form path reports on the
	// pane, the byte-entry path marks it closed. Asserting a "http:" prefix
	// here would only be testing the wrapper that used to double the builder's
	// own prefix, not the routing this test exists for.
	if !strings.Contains(p.note, "not connected") {
		t.Errorf("note = %q, want the send failure recorded on the pane", p.note)
	}
}
