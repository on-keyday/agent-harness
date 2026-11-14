package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeAttached is an attached stream after its session returned.
type fakeAttached struct {
	detached bool
	reason   string
	exit     int32
	ok       bool
}

func (f fakeAttached) DetachedByKey() bool              { return f.detached }
func (f fakeAttached) EndReason() (string, int32, bool) { return f.reason, f.exit, f.ok }
func (f fakeAttached) Stdout() io.Reader                { return strings.NewReader("") }

var errSendClosed = errors.New("trsf: stream direction already finished")

const unsupported = "prompt_unsupported: profile claude has no {prompt} in its interactive template"

// A runner refusing the open sends End (exit -1: nothing ran) ahead of the
// close. However the attach ended — cleanly, or failing to send on the
// closed stream — the open failed with that reason.
func TestAttachRefusedReportsTheReason(t *testing.T) {
	for _, shellErr := range []error{nil, errSendClosed} {
		var out strings.Builder
		err := finishAttach(fakeAttached{reason: unsupported, exit: -1, ok: true}, shellErr, &out)
		if err == nil || err.Error() != "interactive session_ended: "+unsupported {
			t.Fatalf("shell err %v: got %v", shellErr, err)
		}
	}
}

// A session that ran and then ended with a reason says so, and is not a
// failure of the open.
func TestAttachEndedAfterRunningPrintsTheReason(t *testing.T) {
	var out strings.Builder
	if err := finishAttach(fakeAttached{reason: "agent exited", exit: 0, ok: true}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "harness-cli: session ended: agent exited\n" {
		t.Fatalf("got %q", out.String())
	}
}

// A detach, or a far side that sent no End, changes nothing: the shell's own
// result stands and nothing is printed.
func TestAttachWithoutAnEndKeepsTheShellResult(t *testing.T) {
	for _, s := range []fakeAttached{{detached: true}, {}, {detached: true, reason: "x", exit: -1, ok: true}} {
		var out strings.Builder
		if err := finishAttach(s, nil, &out); err != nil || out.Len() != 0 {
			t.Fatalf("%+v: err %v out %q", s, err, out.String())
		}
		if err := finishAttach(s, errSendClosed, &out); !errors.Is(err, errSendClosed) {
			t.Fatalf("%+v: err %v, want the shell's", s, err)
		}
	}
}

// The TUI reads the same answer.
func TestAttachEndReasonForTheTUI(t *testing.T) {
	if r, ok := AttachEndReason(fakeAttached{reason: unsupported, exit: -1, ok: true}, errSendClosed); !ok || r != unsupported {
		t.Fatalf("got %q %v", r, ok)
	}
	if _, ok := AttachEndReason(fakeAttached{detached: true, reason: "x", ok: true}, nil); ok {
		t.Fatal("a detach reported an end")
	}
}
