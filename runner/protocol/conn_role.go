package protocol

// ConnRoleForClientKind is what a connection's ROLE is, given the kind its
// client announced in the handshake.
//
// One mapping, because two parties derive it and they must agree about the
// same connection: the server stamps it onto every conn row, and a client
// stamps it onto the row it reports about ITSELF when asked for its transport
// state. That second one hard-coded Cli, so a TUI asking about its own
// connection was listed as `tui` by the server and `cli` by itself — the same
// connection, two answers, which is the whole failure a shared mapping removes.
//
// Unspecified means the handshake has not completed; the caller decides
// whether that is "not yet identified" or an error.
func ConnRoleForClientKind(k ClientKind) ConnRole {
	switch k {
	case ClientKind_Cli:
		return ConnRole_Cli
	case ClientKind_Tui:
		return ConnRole_Tui
	case ClientKind_Webui:
		return ConnRole_Webui
	case ClientKind_Agent:
		return ConnRole_Agent
	default:
		// Unspecified (the handshake has not completed) and data_plane, which
		// is deliberate: a data-plane connection speaks for one request rather
		// than for a task, so it is not a principal and has no operator role
		// to show.
		return ConnRole_Unspecified
	}
}
