package runner

import "fmt"

const (
	agentTemplateArgs   = "{args}"
	agentTemplatePrompt = "{prompt}"
)

var defaultOneshotArgvTemplate = []string{agentTemplateArgs, "-p", agentTemplatePrompt}

func buildOneshotArgs(template, extra []string, prompt string) ([]string, error) {
	if len(template) == 0 {
		template = defaultOneshotArgvTemplate
	}
	if err := ValidateOneshotArgvTemplate(template); err != nil {
		return nil, err
	}
	return expandAgentArgvTemplate(template, extra, prompt), nil
}

func buildInteractiveArgs(extra, resumeTemplate []string, resumeConversation bool) ([]string, error) {
	if !resumeConversation {
		return extra, nil
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
