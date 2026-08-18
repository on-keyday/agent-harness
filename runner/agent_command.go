package runner

import "fmt"

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
		return expandAgentArgvTemplate(resumeTemplate, extra, prompt), nil
	}
	if len(template) == 0 {
		template = defaultOneshotArgvTemplate
	}
	if err := ValidateOneshotArgvTemplate(template); err != nil {
		return nil, err
	}
	return expandAgentArgvTemplate(template, extra, prompt), nil
}

// buildInteractiveArgs builds the argv for a PTY open. template is the FRESH
// open's argv template and resumeTemplate the --resume-conversation one; both
// are optional and both default to the identity `{args}` — an agent whose
// interactive entry point is the bare binary (claude, codex, agy, a shell)
// needs neither.
//
// The fresh template exists because for a while it did not: this path returned
// the extra args with no template at all, so it was the one launch mode a
// profile could not describe, and anything a profile encoded in its argv
// templates was silently absent here. That cost a wrong-binary bug (`session
// new --agent sandbox-bash` opening Claude Code, task row still reading
// sandbox-bash) that one-shot and direct invocation could not surface.
func buildInteractiveArgs(extra, template, resumeTemplate []string, resumeConversation bool) ([]string, error) {
	if !resumeConversation {
		if len(template) == 0 {
			return extra, nil
		}
		if err := ValidateInteractiveArgvTemplate(template); err != nil {
			return nil, err
		}
		return expandAgentArgvTemplate(template, extra, ""), nil
	}
	if len(resumeTemplate) == 0 {
		return withResumeConversationArgs(extra, true), nil
	}
	if err := ValidateResumeInteractiveArgvTemplate(resumeTemplate); err != nil {
		return nil, err
	}
	return expandAgentArgvTemplate(resumeTemplate, extra, ""), nil
}

func ValidateOneshotArgvTemplate(template []string) error {
	if len(template) == 0 {
		return nil
	}
	return validateAgentArgvTemplate(template, true)
}

// ValidateInteractiveArgvTemplate accepts the FRESH interactive template.
// {prompt} is rejected for the same reason as on the resume template: there is
// no prompt on a PTY open, so a template naming one could only expand to the
// empty string and look like a dropped argument.
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

func validateAgentArgvTemplate(template []string, allowPrompt bool) error {
	promptCount := 0
	for _, tok := range template {
		if tok == agentTemplatePrompt {
			promptCount++
			if !allowPrompt {
				return fmt.Errorf("%s is not valid in this template", agentTemplatePrompt)
			}
		}
	}
	if allowPrompt && promptCount != 1 {
		return fmt.Errorf("oneshot template must contain exactly one %s token", agentTemplatePrompt)
	}
	return nil
}

func expandAgentArgvTemplate(template, args []string, prompt string) []string {
	out := make([]string, 0, len(template)+len(args)+1)
	for _, tok := range template {
		switch tok {
		case agentTemplateArgs:
			out = append(out, args...)
		case agentTemplatePrompt:
			out = append(out, prompt)
		default:
			out = append(out, tok)
		}
	}
	return out
}
