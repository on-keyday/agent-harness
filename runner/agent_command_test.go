package runner

import (
	"reflect"
	"testing"
)

func TestBuildOneshotArgsDefaultClaudeCompatible(t *testing.T) {
	got, err := buildOneshotArgs(nil, []string{"--dangerously-skip-permissions"}, "hello")
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
		[]string{"--search"},
		"hello",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--search", "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildInteractiveArgsDefaultResumeConversation(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--foo"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--foo", "--continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildInteractiveArgsCodexResumeTemplate(t *testing.T) {
	got, err := buildInteractiveArgs([]string{"--search"}, []string{"resume", "--last", agentTemplateArgs}, true)
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
