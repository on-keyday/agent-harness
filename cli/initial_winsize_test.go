package cli

import (
	"errors"
	"testing"
)

type fakeWinsizeSetter struct {
	calls [][4]uint16
	err   error
}

func (f *fakeWinsizeSetter) SetTerminalWindowSize(rows, columns, width, height uint16) error {
	f.calls = append(f.calls, [4]uint16{rows, columns, width, height})
	return f.err
}

// A detached session's PTY has no size until a client attaches, and a spawner
// holding only `spawn` can never attach to fix it (AttachSession needs
// exec_attach). So the one chance to size it is the stream OpenInteractive
// hands back, before it is closed — which is what these pin.
func TestApplyInitialWindowSize(t *testing.T) {
	tests := []struct {
		name      string
		opts      SessionOpts
		wantCalls int
	}{
		{"both set sends one frame", SessionOpts{InitialRows: 40, InitialCols: 150}, 1},
		{"unset sends nothing", SessionOpts{}, 0},
		{"rows only sends nothing", SessionOpts{InitialRows: 40}, 0},
		{"cols only sends nothing", SessionOpts{InitialCols: 150}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeWinsizeSetter{}
			if err := applyInitialWindowSize(f, tc.opts); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(f.calls) != tc.wantCalls {
				t.Fatalf("got %d frames, want %d (%v)", len(f.calls), tc.wantCalls, f.calls)
			}
			if tc.wantCalls == 1 {
				want := [4]uint16{40, 150, 0, 0}
				if f.calls[0] != want {
					t.Fatalf("frame = %v, want %v (rows, columns, width, height)", f.calls[0], want)
				}
			}
		})
	}
}

func TestApplyInitialWindowSizePropagatesError(t *testing.T) {
	sentinel := errors.New("stream is dead")
	f := &fakeWinsizeSetter{err: sentinel}
	err := applyInitialWindowSize(f, SessionOpts{InitialRows: 24, InitialCols: 80})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error not propagated: %v", err)
	}
}
