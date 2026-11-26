package verb

import (
	"reflect"
	"strings"
	"testing"
)

const execTask = "0123456789abcdef0123456789abcdef"

// exec --action is the whole command and runs with no shell, terminal or stdin
// (grant conditions spec C17); naming those beside it is refused.
func TestExecVerbValidate(t *testing.T) {
	a, err := ParseCmdExec(CLI, []string{"--action", "deploy", "--value", "env=prod", "--value", "ref=a=b", execTask}, nil)
	if err != nil {
		t.Fatalf("exec --action: %v", err)
	}
	if a.Action != "deploy" || !reflect.DeepEqual(a.Values, map[string]string{"env": "prod", "ref": "a=b"}) || len(a.Argv) != 0 {
		t.Fatalf("parsed %+v", a)
	}
	for name, args := range map[string][]string{
		"argv beside --action":     {"--action", "deploy", execTask, "--", "echo"},
		"--value without --action": {"--value", "env=prod", execTask, "--", "echo"},
		"--shell beside --action":  {"--action", "deploy", "--shell", execTask},
		"--value given twice":      {"--action", "deploy", "--value", "env=a", "--value", "env=b", execTask},
		"neither":                  {execTask},
	} {
		if _, err := ParseCmdExec(CLI, args, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestActionVerbsParse(t *testing.T) {
	n, err := ParseCmdActionNew(CLI, []string{"--name", "deploy", "--param", "env=choice:staging,prod", "--param", "ref=token", "--",
		"./deploy.sh", "--env", "{env}", "--ref", "{ref}"}, nil)
	if err != nil {
		t.Fatalf("action new: %v", err)
	}
	if n.Name != "deploy" || len(n.Params) != 2 || strings.Join(n.Template, " ") != "./deploy.sh --env {env} --ref {ref}" {
		t.Fatalf("parsed %+v", n)
	}
	if _, err := ParseCmdActionNew(CLI, []string{"--", "true"}, nil); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("new without --name: %v", err)
	}
	if _, err := ParseCmdActionNew(CLI, []string{"--name", "x", "--param", "Bad=token", "--", "true"}, nil); err == nil {
		t.Fatal("a bad --param was accepted")
	}
	e, err := ParseCmdActionEdit(CLI, []string{"--revision", "3", "deploy", "--", "true"}, nil)
	if err != nil || e.Ref != "deploy" || e.Revision != 3 || e.DescriptionSet {
		t.Fatalf("action edit: %+v %v", e, err)
	}
	if _, err := ParseCmdActionEdit(CLI, []string{"deploy", "--", "true"}, nil); err == nil || !strings.Contains(err.Error(), "--revision") {
		t.Fatalf("edit without --revision: %v", err)
	}
}

// --json writes for a pipe; the TUI and WebUI answer into a results pane, so
// the flag is the CLI's alone and typing it elsewhere is refused, not ignored
// (final review: a typed option takes effect or errors).
func TestActionLsJSONIsCLIOnly(t *testing.T) {
	if _, err := ParseCmdActionLs(CLI, []string{"--json"}, nil); err != nil {
		t.Fatalf("CLI --json: %v", err)
	}
	for _, sf := range []Surface{TUI, WebUI} {
		if _, err := ParseCmdActionLs(sf, []string{"--json"}, nil); err == nil {
			t.Errorf("%v accepted --json it would ignore", sf)
		}
	}
}
