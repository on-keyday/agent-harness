package runner

func withResumeConversationArgs(args []string, resumeConversation bool) []string {
	if !resumeConversation {
		return args
	}
	for _, arg := range args {
		if arg == "--continue" {
			return args
		}
	}
	out := append([]string{}, args...)
	return append(out, "--continue")
}
