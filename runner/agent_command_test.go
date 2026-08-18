package runner

import (
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
	got, err := buildInteractiveArgs([]string{"--foo"}, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--foo", "--continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildInteractiveArgsCodexResumeTemplate(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--search"}, nil, []string{"resume", "--last", agentTemplateArgs}, true)
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
	if err := ValidateResumeInteractiveArgvTemplate([]string{"resume", agentTemplatePrompt}); err == nil {
		t.Fatal("resume interactive template with {prompt}: expected error")
	}
}

// A FRESH interactive open is the one launch mode a profile could not describe:
// buildInteractiveArgs returned the extra args unchanged, with no template
// involved. That asymmetry is what let the podman wrapper's agent selector —
// which lived in the oneshot/resume templates — vanish on exactly that path,
// so `session new --agent sandbox-bash` opened Claude Code while the task row
// read sandbox-bash. These pin the fourth template.
func TestBuildInteractiveArgsFreshDefaultsToIdentity(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--foo", "--bar"}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--foo", "--bar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v (an empty template must stay the historical identity)", got, want)
	}
}

func TestBuildInteractiveArgsFreshTemplate(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--search"}, []string{"chat", agentTemplateArgs}, []string{"resume", agentTemplateArgs}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"chat", "--search"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	// The resume template must not leak into a fresh open, and vice versa.
	got, err = buildInteractiveArgs([]string{"--search"}, []string{"chat", agentTemplateArgs}, []string{"resume", agentTemplateArgs}, true)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"resume", "--search"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume got %#v, want %#v", got, want)
	}
}

func TestValidateInteractiveArgvTemplateRejectsPrompt(t *testing.T) {
	if err := ValidateInteractiveArgvTemplate([]string{agentTemplateArgs, agentTemplatePrompt}); err == nil {
		t.Fatal("interactive template with {prompt}: expected error (there is no prompt on this path)")
	}
	if err := ValidateInteractiveArgvTemplate([]string{"chat", agentTemplateArgs}); err != nil {
		t.Fatalf("valid interactive template rejected: %v", err)
	}
	if err := ValidateInteractiveArgvTemplate(nil); err != nil {
		t.Fatalf("empty template must be valid (means identity): %v", err)
	}
}
