package runner

import (
	"errors"
	"fmt"
	"strings"
)

const (
	agentTemplateArgs   = "{args}"
	agentTemplatePrompt = "{prompt}"
)

var defaultOneshotArgvTemplate = []string{agentTemplateArgs, "-p", agentTemplatePrompt}
var defaultResumeOneshotArgvTemplate = []string{agentTemplateArgs, "--continue", "-p", agentTemplatePrompt}

func buildOneshotArgs(template, resumeTemplate, extra []string, prompt string, resumeConversation bool) ([]string, error) {
	if resumeConversation {
		if len(resumeTemplate) == 0 {
			resumeTemplate = defaultResumeOneshotArgvTemplate
		}
		if err := ValidateOneshotArgvTemplate(resumeTemplate); err != nil {
			return nil, err
		}
		return expandAgentArgvTemplate(resumeTemplate, extra, prompt, false), nil
	}
	if len(template) == 0 {
		template = defaultOneshotArgvTemplate
	}
	if err := ValidateOneshotArgvTemplate(template); err != nil {
		return nil, err
	}
	return expandAgentArgvTemplate(template, extra, prompt, false), nil
}

// errPromptUnsupported: a prompt was given for a profile whose chosen
// interactive template has no {prompt} to carry it (pillar A spec, A-D3).
// Refused, never dropped and never typed into the PTY.
var errPromptUnsupported = errors.New("no {prompt} in the interactive template")

// buildInteractiveArgs builds the argv for a PTY open. template is the FRESH
// open's argv template and resumeTemplate the --resume-conversation one; both
// are optional and both default to the identity `{args}` — an agent whose
// interactive entry point is the bare binary (claude, codex, agy, a shell)
// needs neither.
//
// prompt is the session's initial prompt (A-D3): it fills the template's
// {prompt} token, and an empty prompt drops every token that holds {prompt}
// (a flag-borne `--prompt={prompt}` goes with it). A non-empty prompt with no
// {prompt} in the chosen template — the identity case included — is
// errPromptUnsupported.
//
// The fresh template exists because for a while it did not: this path returned
// the extra args with no template at all, so it was the one launch mode a
// profile could not describe, and anything a profile encoded in its argv
// templates was silently absent here. That cost a wrong-binary bug (`session
// new --agent sandbox-bash` opening Claude Code, task row still reading
// sandbox-bash) that one-shot and direct invocation could not surface.
func buildInteractiveArgs(extra, template, resumeTemplate []string, resumeConversation bool, prompt string) ([]string, error) {
	if !resumeConversation {
		if len(template) == 0 {
			if prompt != "" {
				return nil, errPromptUnsupported
			}
			return extra, nil
		}
		if err := ValidateInteractiveArgvTemplate(template); err != nil {
			return nil, err
		}
		if prompt != "" && !hasPromptToken(template) {
			return nil, errPromptUnsupported
		}
		return expandAgentArgvTemplate(template, extra, prompt, true), nil
	}
	if len(resumeTemplate) == 0 {
		if prompt != "" {
			return nil, errPromptUnsupported
		}
		return withResumeConversationArgs(extra, true), nil
	}
	if err := ValidateResumeInteractiveArgvTemplate(resumeTemplate); err != nil {
		return nil, err
	}
	if prompt != "" && !hasPromptToken(resumeTemplate) {
		return nil, errPromptUnsupported
	}
	return expandAgentArgvTemplate(resumeTemplate, extra, prompt, true), nil
}

func ValidateOneshotArgvTemplate(template []string) error {
	if len(template) == 0 {
		return nil
	}
	return validateAgentArgvTemplate(template, true)
}

// ValidateInteractiveArgvTemplate accepts the FRESH interactive template. At
// most one token may hold {prompt} — the whole token, or embedded in a flag
// such as `--prompt={prompt}` (A-D3).
func ValidateInteractiveArgvTemplate(template []string) error {
	if len(template) == 0 {
		return nil
	}
	return validateAgentArgvTemplate(template, false)
}

func ValidateResumeInteractiveArgvTemplate(template []string) error {
	if len(template) == 0 {
		return nil
	}
	return validateAgentArgvTemplate(template, false)
}

// validateAgentArgvTemplate: a oneshot template holds exactly one whole
// {prompt} token; an interactive one at most one token holding {prompt},
// whole or embedded.
func validateAgentArgvTemplate(template []string, oneshot bool) error {
	whole, holding := 0, 0
	for _, tok := range template {
		if tok == agentTemplatePrompt {
			whole++
		}
		if strings.Contains(tok, agentTemplatePrompt) {
			holding++
		}
	}
	if oneshot {
		if whole != 1 || holding != 1 {
			return fmt.Errorf("oneshot template must contain exactly one %s token", agentTemplatePrompt)
		}
		return nil
	}
	if holding > 1 {
		return fmt.Errorf("at most one token may hold %s", agentTemplatePrompt)
	}
	return nil
}

func hasPromptToken(template []string) bool {
	for _, tok := range template {
		if strings.Contains(tok, agentTemplatePrompt) {
			return true
		}
	}
	return false
}

// expandAgentArgvTemplate replaces {args} with the args and {prompt} — a whole
// token or inside one — with the prompt. With dropEmpty (an interactive open),
// an empty prompt drops every token holding {prompt}, so neither "" nor a
// bare flag reaches the agent; a oneshot keeps its historical "" argument.
//
// A template `--` directly before a whole {prompt} goes with it: it is there
// to keep a dash-leading prompt (or one that names a subcommand) from being
// parsed as an option, and with no prompt it would turn whatever follows the
// argv — a stream adapter appends its own flags — into positionals.
func expandAgentArgvTemplate(template, args []string, prompt string, dropEmpty bool) []string {
	out := make([]string, 0, len(template)+len(args)+1)
	for i, tok := range template {
		switch {
		case tok == agentTemplateArgs:
			out = append(out, args...)
		case tok == "--" && dropEmpty && prompt == "" && i+1 < len(template) && template[i+1] == agentTemplatePrompt:
			continue
		case strings.Contains(tok, agentTemplatePrompt):
			if prompt == "" && dropEmpty {
				continue
			}
			out = append(out, strings.ReplaceAll(tok, agentTemplatePrompt, prompt))
		default:
			out = append(out, tok)
		}
	}
	return out
}
