package protocol

// ClaudeArgsFromStrings packs a []string into a ClaudeArgs wire value. Returns
// the zero ClaudeArgs (ArgsLen=0, Args=nil) when args is empty so encode
// emits a single u16(0) length prefix.
func ClaudeArgsFromStrings(args []string) ClaudeArgs {
	out := ClaudeArgs{}
	if len(args) == 0 {
		return out
	}
	cargs := make([]ClaudeArg, len(args))
	for i, a := range args {
		ca := ClaudeArg{}
		ca.SetArg([]byte(a))
		cargs[i] = ca
	}
	out.SetArgs(cargs)
	return out
}

// AsStrings unpacks a ClaudeArgs into a fresh []string. Returns nil when
// empty so callers can range over the result without a nil-check.
func (c ClaudeArgs) AsStrings() []string {
	if len(c.Args) == 0 {
		return nil
	}
	out := make([]string, len(c.Args))
	for i, a := range c.Args {
		out[i] = string(a.Arg)
	}
	return out
}
