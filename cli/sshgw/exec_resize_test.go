//go:build !js

package sshgw

import (
	"sync"
	"testing"
)

// window-change can arrive before ExecRun has opened the stream (the client
// resizes while the runner is still starting the child). The size must not be
// dropped: the LAST one sent is applied when the stream is set.
func TestExecResizerKeepsTheLastSizeUntilSet(t *testing.T) {
	var r execResizer
	r.apply(ptyDims{Rows: 10, Cols: 20})
	r.apply(ptyDims{Rows: 40, Cols: 120})
	var mu sync.Mutex
	var got [][2]uint16
	r.set(func(rows, cols, _, _ uint16) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, [2]uint16{rows, cols})
		return nil
	})
	r.apply(ptyDims{Rows: 50, Cols: 130})
	if len(got) != 2 || got[0] != [2]uint16{40, 120} || got[1] != [2]uint16{50, 130} {
		t.Fatalf("resizes = %v, want [40x120 50x130]", got)
	}
}
