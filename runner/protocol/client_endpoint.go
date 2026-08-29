package protocol

// IsInProcess reports whether the client side of this forward lives inside a
// client process rather than behind an OS socket.
//
// Hand-written beside the generated code because `protoregen` owns message.go
// alone; the same arrangement task_kind.go and runner_id.go already use.
//
// **Use this instead of `== ClientEndpointKind_InProcess`.** That equality was
// correct while in-process had exactly one spelling and becomes a silent miss
// the moment it has several — and the misses are not cosmetic. The server
// refuses the unimplemented remote × in-process combination with such a check,
// so a specifically-named kind would have slipped past it and bound a runner
// listener nothing would ever answer; the JSON renderer's `default:` arm
// answered "os_socket" for anything it did not recognise, which would have made
// every new kind lie about the one property the field exists to report.
//
// Savability is the case that must NOT go through here and does not: a
// workspace writes a line only for `os_socket`, which is a positive test
// against the one member that has an address to write down. Phrasing it as
// "not in-process" would make a member added tomorrow savable by default, and
// the default has to be the safe one.
func (k ClientEndpointKind) IsInProcess() bool {
	switch k {
	case ClientEndpointKind_InProcess,
		ClientEndpointKind_InProcessStdio,
		ClientEndpointKind_InProcessHttp,
		ClientEndpointKind_InProcessPane,
		ClientEndpointKind_InProcessPreview,
		ClientEndpointKind_InProcessSshGateway:
		return true
	}
	return false
}
