package protocol

// AgentArgsFromStrings packs a []string into a AgentArgs wire value. Returns
// the zero AgentArgs (ArgsLen=0, Args=nil) when args is empty so encode
// emits a single u16(0) length prefix.
func AgentArgsFromStrings(args []string) AgentArgs {
	out := AgentArgs{}
	if len(args) == 0 {
		return out
	}
	cargs := make([]AgentArg, len(args))
	for i, a := range args {
		ca := AgentArg{}
		ca.SetArg([]byte(a))
		cargs[i] = ca
	}
	out.SetArgs(cargs)
	return out
}

// AsStrings unpacks a AgentArgs into a fresh []string. Returns nil when
// empty so callers can range over the result without a nil-check.
func (c AgentArgs) AsStrings() []string {
	if len(c.Args) == 0 {
		return nil
	}
	out := make([]string, len(c.Args))
	for i, a := range c.Args {
		out[i] = string(a.Arg)
	}
	return out
}
