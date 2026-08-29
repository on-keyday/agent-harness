package workspace

import (
	"strings"
	"testing"
)

const sample = `# my harness setup

[workspace default]
server-cid = ws:example.invalid:8539-*
repo       = /abs/path/to/repo
grid       = --under 3f2a9c00000000000000000000000001

[workspace default task 3f2a9c00000000000000000000000001]
resume  = continue
runner  = assigned
forward = -L 3000:127.0.0.1:3000
forward = -R 8080:127.0.0.1:8080

# a second one, for the phone
[workspace phone]
server-cid = ws:example.invalid:8539-*
`

func TestParseRenderRoundTrip(t *testing.T) {
	f, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(f.Render()); got != sample {
		t.Errorf("Render is not byte-identical to input:\n--- got ---\n%s\n--- want ---\n%s", got, sample)
	}
}

func TestParseValues(t *testing.T) {
	f, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ws, ok := f.Workspace("default")
	if !ok {
		t.Fatal(`Workspace("default") not found`)
	}
	if ws.Repo != "/abs/path/to/repo" {
		t.Errorf("Repo = %q", ws.Repo)
	}
	if ws.Grid != "--under 3f2a9c00000000000000000000000001" || !ws.GridSet {
		t.Errorf("Grid = %q set=%v", ws.Grid, ws.GridSet)
	}
	if len(ws.Tasks) != 1 {
		t.Fatalf("len(Tasks) = %d, want 1", len(ws.Tasks))
	}
	tk := ws.Tasks[0]
	if tk.Resume != ResumeContinue || tk.Runner != RunnerAssigned {
		t.Errorf("resume=%q runner=%q", tk.Resume, tk.Runner)
	}
	if len(tk.Forwards) != 2 || tk.Forwards[0] != "-L 3000:127.0.0.1:3000" {
		t.Errorf("Forwards = %q", tk.Forwards)
	}
	if names := f.Names(); len(names) != 2 || names[0] != "default" || names[1] != "phone" {
		t.Errorf("Names() = %q, want [default phone] in file order", names)
	}
}

func TestParseDefaults(t *testing.T) {
	f, err := Parse(strings.NewReader(
		"[workspace w]\n[workspace w task 3f2a9c00000000000000000000000001]\nforward = -L 1:127.0.0.1:1\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ws, _ := f.Workspace("w")
	if ws.Tasks[0].Resume != ResumeNo {
		t.Errorf("resume default = %q, want %q", ws.Tasks[0].Resume, ResumeNo)
	}
	if ws.Tasks[0].Runner != RunnerAssigned {
		t.Errorf("runner default = %q, want %q", ws.Tasks[0].Runner, RunnerAssigned)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "[workspace w]\nfowardd = -L 1:127.0.0.1:1\n",
		"unknown header":    "[profile w]\n",
		"bad resume":        "[workspace w]\n[workspace w task 3f2a9c00000000000000000000000001]\nresume = maybe\n",
		"bad runner":        "[workspace w]\n[workspace w task 3f2a9c00000000000000000000000001]\nrunner = whoever\n",
		"short task id":     "[workspace w]\n[workspace w task 3f2a9c]\n",
		"task before ws":    "[workspace w task 3f2a9c00000000000000000000000001]\n",
		"duplicate ws":      "[workspace w]\n[workspace w]\n",
		"key before header": "repo = /x\n",
		"no equals":         "[workspace w]\nrepo\n",
		"forward on ws":     "[workspace w]\nforward = -L 1:127.0.0.1:1\n",
		"psk is not a key":  "[workspace w]\npsk = hunter2\n",
	}
	for name, src := range cases {
		if _, err := Parse(strings.NewReader(src)); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
}

// A workspace's line span must stop at its own last line. Ending it at the next
// header would swallow the blank line and the comment that introduce the NEXT
// workspace, and Set (Task 4) would then delete them.
func TestWorkspaceSpanExcludesTheNextHeadersPreamble(t *testing.T) {
	f, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ws, _ := f.Workspace("default")
	last := f.lines[ws.end-1]
	if !strings.HasPrefix(strings.TrimSpace(last), "forward") {
		t.Errorf("span ends on %q, want the last forward line of the workspace", last)
	}
	for _, l := range f.lines[ws.end:] {
		if strings.Contains(l, "for the phone") {
			return // the comment is outside the span, as it must be
		}
	}
	t.Error("the next workspace's comment is inside the default workspace's span")
}

// ssh-gateway is a workspace-level key, beside grid: the gateway is
// process-scoped, not per-task, so a task block is the wrong home for it.
//
// Presence is its own bit for grid's exact reason — the empty value is a real
// instruction ("bind wherever the gateway defaults to") and an absent key means
// "do not touch the gateway at all". A plain string cannot hold both.
func TestParseSSHGateway(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantVal string
		wantSet bool
	}{
		{"absent", "[workspace w]\nrepo = /r\n", "", false},
		{"address", "[workspace w]\nssh-gateway = 127.0.0.1:2222\n", "127.0.0.1:2222", true},
		{"empty value means the default bind", "[workspace w]\nssh-gateway =\n", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			ws, ok := f.Workspace("w")
			if !ok {
				t.Fatal("workspace w not parsed")
			}
			if ws.SSHGateway != tc.wantVal || ws.SSHGatewaySet != tc.wantSet {
				t.Errorf("SSHGateway = %q set=%v, want %q set=%v",
					ws.SSHGateway, ws.SSHGatewaySet, tc.wantVal, tc.wantSet)
			}
		})
	}
}

// The unknown-key error names every legal key, because it is the only place an
// operator who mistyped one finds out what the right ones are.
func TestUnknownWorkspaceKeyNamesSSHGateway(t *testing.T) {
	_, err := Parse(strings.NewReader("[workspace w]\nsshgateway = x\n"))
	if err == nil {
		t.Fatal("want an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "ssh-gateway") {
		t.Errorf("error %q does not name the ssh-gateway key", err)
	}
}
