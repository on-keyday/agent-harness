package protocol

// IsSessionKind reports whether a task kind is one the `session` verbs apply
// to. Both `interactive` and `stream` are opened by the same
// OpenInteractiveRequest, both run on a server-allocated bidi stream owned by
// the session mux for the task's whole life, and both are attachable; only the
// payload inside the frames differs.
//
// It exists so the several places that ask "is this a session" cannot answer it
// differently. Before the stream kind there was one answer and six literal
// `Kind == TaskKind_Interactive` comparisons; adding a second kind turned each
// of those into a decision, and a shared predicate is what keeps the ones that
// mean "a session" from drifting apart.
//
// It deliberately does NOT cover every such comparison. Two of them are about
// TERMINALS rather than sessions and must keep excluding the stream kind:
//
//   - the TUI grid renders PTY panes, and an event stream has no terminal grid
//   - the TUI's reattach action splices a PTY, and would paint NDJSON as
//     terminal bytes until the TUI has an event renderer
//
// Those read `== TaskKind_Interactive` on purpose, with a comment saying so.
func IsSessionKind(k TaskKind) bool {
	return k == TaskKind_Interactive || k == TaskKind_Stream
}

// IsPTYKind reports whether a task drives a real terminal. It is the other half
// of IsSessionKind, named so a caller has to say which one it means rather than
// writing a bare comparison whose intent the next kind will make ambiguous.
func IsPTYKind(k TaskKind) bool { return k == TaskKind_Interactive }
