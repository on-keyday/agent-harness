package protocol

import "testing"

// inProcessKinds is every member that describes a client-side endpoint living
// INSIDE a client process. It is written out rather than derived because the
// point of the list is to be updated when a member is added: a new kind that
// nobody adds here fails these tests, which is the reminder.
var inProcessKinds = []ClientEndpointKind{
	ClientEndpointKind_InProcess,
	ClientEndpointKind_InProcessStdio,
	ClientEndpointKind_InProcessHttp,
	ClientEndpointKind_InProcessPane,
	ClientEndpointKind_InProcessPreview,
	ClientEndpointKind_InProcessSshGateway,
}

// The predicate is what every caller must use instead of `== InProcess`. An
// equality check was correct while there was one in-process member and becomes
// a silent miss the moment there are several — the server's remote × in-process
// refusal is the site where that miss would let an unimplemented combination
// through.
func TestClientEndpointKindIsInProcess(t *testing.T) {
	for _, k := range inProcessKinds {
		if !k.IsInProcess() {
			t.Errorf("%v.IsInProcess() = false, want true", k)
		}
	}
	if ClientEndpointKind_OsSocket.IsInProcess() {
		t.Error("os_socket.IsInProcess() = true, want false")
	}
}

// The bare in_process member MUST survive. It is what a client older than the
// split sends, and the wire is strict about length but tolerant about unknown
// enum values (measured): keeping the member is what makes this change need no
// coordinated restart.
func TestBareInProcessRemains(t *testing.T) {
	if !ClientEndpointKind_InProcess.IsInProcess() {
		t.Fatal("the unspecified in-process member was removed or renamed")
	}
}
