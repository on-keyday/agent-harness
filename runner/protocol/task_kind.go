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
//
// A credential task is not a session either: it has no runner, no process and
// no stream, so no session verb applies to it.
func IsSessionKind(k TaskKind) bool {
	return k == TaskKind_Oneshot || k == TaskKind_Interactive || k == TaskKind_Stream
}

// IsPTYKind reports whether a task uses the terminal/pipe session mux (as
// opposed to the NDJSON event-stream adapter). Both interactive and oneshot
// ride that mux (oneshot defaults to pipes, interactive to a PTY, orthogonal
// via --pty / --no-pty).
func IsPTYKind(k TaskKind) bool {
	return k == TaskKind_Oneshot || k == TaskKind_Interactive
}
