package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

// httpMethods is the cycle order for the form's method field. It is not the set
// of methods the harness supports — the CLI takes a free-form --http-method —
// only the ones worth a keypress here.
var httpMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

type httpFormField int

const (
	httpFieldMethod httpFormField = iota
	httpFieldPath
	httpFieldHeaders
	httpFieldBody
	httpFieldCount
)

// httpForm collects a request for cli.BuildHTTPRequest. Headers and body are
// textareas (as tui/popup.go already uses) so headers can be one per line and
// no separator syntax has to be invented for a single-line field.
type httpForm struct {
	method  int
	path    textinput.Model
	headers textarea.Model
	body    textarea.Model
	field   httpFormField
}

func newHTTPForm() httpForm {
	p := textinput.New()
	p.Placeholder = "/healthz"
	p.CharLimit = 512
	h := textarea.New()
	h.Placeholder = "Accept: application/json"
	h.SetHeight(3)
	b := textarea.New()
	b.Placeholder = "request body"
	b.SetHeight(3)
	f := httpForm{path: p, headers: h, body: b, field: httpFieldPath}
	f.path.Focus()
	return f
}

// setForTest fills the fields without going through key events.
func (f *httpForm) setForTest(method, path, headers, body string) {
	for i, m := range httpMethods {
		if m == method {
			f.method = i
		}
	}
	f.path.SetValue(path)
	f.headers.SetValue(headers)
	f.body.SetValue(body)
}

// CycleMethod walks the method list, wrapping — unlike the pane tabs, where
// clamping matters because [+ new] is an endpoint the operator aims for.
func (f *httpForm) CycleMethod(delta int) {
	f.method = (f.method + delta + len(httpMethods)) % len(httpMethods)
}

func (f *httpForm) NextField() {
	f.field = (f.field + 1) % httpFieldCount
	f.path.Blur()
	f.headers.Blur()
	f.body.Blur()
	switch f.field {
	case httpFieldPath:
		f.path.Focus()
	case httpFieldHeaders:
		f.headers.Focus()
	case httpFieldBody:
		f.body.Focus()
	}
}

func (f *httpForm) Spec() cli.HTTPRequestSpec {
	headers := make([]string, 0, 4)
	for _, line := range strings.Split(f.headers.Value(), "\n") {
		if strings.TrimSpace(line) != "" {
			headers = append(headers, strings.TrimSpace(line))
		}
	}
	return cli.HTTPRequestSpec{
		Method:  httpMethods[f.method],
		Path:    f.path.Value(),
		Headers: headers,
		Body:    []byte(f.body.Value()),
	}
}

func (f httpForm) Update(msg tea.Msg) (httpForm, tea.Cmd) {
	var cmd tea.Cmd
	switch f.field {
	case httpFieldPath:
		f.path, cmd = f.path.Update(msg)
	case httpFieldHeaders:
		f.headers, cmd = f.headers.Update(msg)
	case httpFieldBody:
		f.body, cmd = f.body.Update(msg)
	}
	return f, cmd
}

func (f httpForm) View() string {
	mark := func(want httpFormField, s string) string {
		if f.field == want {
			return FocusedStyle.Render(s)
		}
		return s
	}
	return mark(httpFieldMethod, "method  "+httpMethods[f.method]+"  (←/→)") + "\n" +
		mark(httpFieldPath, "path") + "  " + f.path.View() + "\n" +
		mark(httpFieldHeaders, "headers") + "\n" + f.headers.View() + "\n" +
		mark(httpFieldBody, "body") + "\n" + f.body.View() + "\n" +
		FooterStyle.Render("tab next field · Enter send · ctrl+t back")
}
