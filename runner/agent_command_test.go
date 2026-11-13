package runner

import (
	"errors"
	"reflect"
	"testing"
)

func TestBuildOneshotArgsDefaultClaudeCompatible(t *testing.T) {
	got, err := buildOneshotArgs(nil, nil, []string{"--dangerously-skip-permissions"}, "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dangerously-skip-permissions", "-p", "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildOneshotArgsCodexTemplate(t *testing.T) {
	got, err := buildOneshotArgs(
		[]string{"exec", agentTemplateArgs, agentTemplatePrompt},
		nil,
		[]string{"--search"},
		"hello",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--search", "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildOneshotArgsResumeConversation(t *testing.T) {
	got, err := buildOneshotArgs(nil, nil, []string{"--dangerously-skip-permissions"}, "hello", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dangerously-skip-permissions", "--continue", "-p", "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildOneshotArgsResumeConversationTemplate(t *testing.T) {
	got, err := buildOneshotArgs(
		[]string{"exec", agentTemplateArgs, agentTemplatePrompt},
		[]string{"exec", "resume", "--last", agentTemplateArgs, agentTemplatePrompt},
		[]string{"--json"},
		"hello",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "resume", "--last", "--json", "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildInteractiveArgsDefaultResumeConversation(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--foo"}, nil, nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--foo", "--continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildInteractiveArgsCodexResumeTemplate(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--search"}, nil, []string{"resume", "--last", agentTemplateArgs}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"resume", "--last", "--search"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestAgentArgvTemplateValidation(t *testing.T) {
	if err := ValidateOneshotArgvTemplate([]string{agentTemplateArgs}); err == nil {
		t.Fatal("oneshot template without {prompt}: expected error")
	}
	if err := ValidateOneshotArgvTemplate([]string{agentTemplatePrompt, agentTemplatePrompt}); err == nil {
		t.Fatal("oneshot template with two {prompt}: expected error")
	}
	if err := ValidateResumeInteractiveArgvTemplate([]string{"resume", agentTemplatePrompt, agentTemplatePrompt}); err == nil {
		t.Fatal("resume interactive template with two {prompt}: expected error")
	}
}

// A FRESH interactive open is the one launch mode a profile could not describe:
// buildInteractiveArgs returned the extra args unchanged, with no template
// involved. That asymmetry is what let the podman wrapper's agent selector —
// which lived in the oneshot/resume templates — vanish on exactly that path,
// so `session new --agent sandbox-bash` opened Claude Code while the task row
// read sandbox-bash. These pin the fourth template.
func TestBuildInteractiveArgsFreshDefaultsToIdentity(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--foo", "--bar"}, nil, nil, false, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--foo", "--bar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v (an empty template must stay the historical identity)", got, want)
	}
}

func TestBuildInteractiveArgsFreshTemplate(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--search"}, []string{"chat", agentTemplateArgs}, []string{"resume", agentTemplateArgs}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"chat", "--search"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	// The resume template must not leak into a fresh open, and vice versa.
	got, err = buildInteractiveArgs([]string{"--search"}, []string{"chat", agentTemplateArgs}, []string{"resume", agentTemplateArgs}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"resume", "--search"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume got %#v, want %#v", got, want)
	}
}

// An interactive open takes its initial prompt through {prompt} (pillar A
// spec A-D3): one token at most, whole or embedded in a flag.
func TestInteractiveTemplateAcceptsOnePromptToken(t *testing.T) {
	for _, tpl := range [][]string{
		{agentTemplateArgs, agentTemplatePrompt},
		{agentTemplateArgs, "--prompt=" + agentTemplatePrompt},
		{"chat", agentTemplateArgs},
		nil,
	} {
		if err := ValidateInteractiveArgvTemplate(tpl); err != nil {
			t.Errorf("%q rejected: %v", tpl, err)
		}
	}
}

func TestInteractiveTemplateRejectsTwoPromptTokens(t *testing.T) {
	if err := ValidateInteractiveArgvTemplate([]string{agentTemplatePrompt, "--x=" + agentTemplatePrompt}); err == nil {
		t.Fatal("two {prompt} tokens accepted")
	}
}

func TestEmbeddedPromptTokenDroppedWhenEmpty(t *testing.T) {
	tpl := []string{agentTemplateArgs, "--prompt-interactive=" + agentTemplatePrompt}
	got, err := buildInteractiveArgs([]string{"-v"}, tpl, nil, false, "")
	if err != nil || !reflect.DeepEqual(got, []string{"-v"}) {
		t.Fatalf("empty prompt: %#v, %v", got, err)
	}
	got, err = buildInteractiveArgs([]string{"-v"}, tpl, nil, false, "triage it")
	if err != nil || !reflect.DeepEqual(got, []string{"-v", "--prompt-interactive=triage it"}) {
		t.Fatalf("prompt: %#v, %v", got, err)
	}
	whole := []string{agentTemplateArgs, "--", agentTemplatePrompt}
	got, err = buildInteractiveArgs(nil, whole, nil, false, "-starts with a dash")
	if err != nil || !reflect.DeepEqual(got, []string{"--", "-starts with a dash"}) {
		t.Fatalf("whole token: %#v, %v", got, err)
	}
}

// A prompt with no {prompt} to carry it is refused, never dropped — the
// identity template (no template) included.
func TestBuildInteractiveArgsPromptUnsupported(t *testing.T) {
	for name, c := range map[string]struct {
		fresh, resume []string
		resumeConv    bool
	}{
		"no template":          {nil, nil, false},
		"template no token":    {[]string{"chat", agentTemplateArgs}, nil, false},
		"default resume":       {[]string{agentTemplateArgs, agentTemplatePrompt}, nil, true},
		"resume without token": {[]string{agentTemplateArgs, agentTemplatePrompt}, []string{agentTemplateArgs, "--continue"}, true},
	} {
		if _, err := buildInteractiveArgs(nil, c.fresh, c.resume, c.resumeConv, "p"); !errors.Is(err, errPromptUnsupported) {
			t.Errorf("%s: err = %v, want errPromptUnsupported", name, err)
		}
	}
}

// A `--` that separates a whole {prompt} from the flags goes with the prompt
// when there is none: a stream adapter appends its own flags after the argv,
// and a stray `--` would turn them into positionals (runner review I3).
func TestDashBeforePromptDroppedWhenEmpty(t *testing.T) {
	tpl := []string{agentTemplateArgs, "--", agentTemplatePrompt}
	got, err := buildInteractiveArgs([]string{"-v"}, tpl, nil, false, "")
	if err != nil || !reflect.DeepEqual(got, []string{"-v"}) {
		t.Fatalf("empty prompt: %#v, %v", got, err)
	}
	got, err = buildInteractiveArgs([]string{"-v"}, tpl, nil, false, "- fix the bug")
	if err != nil || !reflect.DeepEqual(got, []string{"-v", "--", "- fix the bug"}) {
		t.Fatalf("prompt: %#v, %v", got, err)
	}
	// A `--` the args supplied is theirs, not the template's.
	got, err = buildInteractiveArgs([]string{"--"}, []string{agentTemplateArgs, agentTemplatePrompt}, nil, false, "")
	if err != nil || !reflect.DeepEqual(got, []string{"--"}) {
		t.Fatalf("args' own --: %#v, %v", got, err)
	}
}
