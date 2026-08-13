package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// fakeSetParentClient captures the outgoing request and answers with a canned
// SetParentResponse.
type fakeSetParentClient struct {
	lastRequest *protocol.TaskControlRequest
	resp        protocol.SetParentResponse
}

func (f *fakeSetParentClient) RoundTripTaskControl(_ context.Context, req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	f.lastRequest = req
	resp := &protocol.TaskControlResponse{Kind: protocol.TaskControlKind_SetParent, RequestId: req.RequestId}
	resp.SetSetParent(f.resp)
	return resp, nil
}

const (
	spTaskB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	spTaskA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	spTaskP = "cccccccccccccccccccccccccccccccc"
)

func TestSetParentRequestConstruction(t *testing.T) {
	cases := []struct {
		name       string
		opts       SetParentOpts
		wantParent string // hex of the wire ParentId; "" = zero
		wantSwap   bool
	}{
		{"repoint", SetParentOpts{TaskID: spTaskB, ParentID: spTaskA}, spTaskA, false},
		{"detach", SetParentOpts{TaskID: spTaskB}, "", false},
		{"swap", SetParentOpts{TaskID: spTaskB, Swap: true}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeSetParentClient{resp: protocol.SetParentResponse{Status: protocol.SetParentStatus_Ok}}
			if _, err := SetParentWith(context.Background(), fc, tc.opts); err != nil {
				t.Fatal(err)
			}
			sp := fc.lastRequest.SetParent()
			if sp == nil {
				t.Fatal("no SetParent variant on the request")
			}
			wantTask, _ := parseTaskIDHex(spTaskB)
			if sp.TaskId != wantTask {
				t.Fatalf("TaskId = %x", sp.TaskId.Id)
			}
			var wantP protocol.TaskID
			if tc.wantParent != "" {
				wantP, _ = parseTaskIDHex(tc.wantParent)
			}
			if sp.ParentId != wantP {
				t.Fatalf("ParentId = %x, want %x", sp.ParentId.Id, wantP.Id)
			}
			if sp.Swap() != tc.wantSwap {
				t.Fatalf("Swap = %v, want %v", sp.Swap(), tc.wantSwap)
			}
		})
	}
}

func TestSetParentRejectsSwapWithParent(t *testing.T) {
	fc := &fakeSetParentClient{}
	_, err := SetParentWith(context.Background(), fc, SetParentOpts{TaskID: spTaskB, ParentID: spTaskA, Swap: true})
	if err == nil || !strings.Contains(err.Error(), "swap") {
		t.Fatalf("err = %v, want a client-side swap/parent conflict error", err)
	}
	if fc.lastRequest != nil {
		t.Fatal("a rejected combination still went on the wire")
	}
}

func TestSetParentMessage(t *testing.T) {
	cases := []struct {
		name string
		opts SetParentOpts
		res  SetParentResult
		want string
	}{
		{"repoint", SetParentOpts{TaskID: spTaskB},
			SetParentResult{OldParent: spTaskA, NewParent: spTaskP},
			"set-parent bbbbbbbb: parent=aaaaaaaa → cccccccc"},
		{"detach", SetParentOpts{TaskID: spTaskB},
			SetParentResult{OldParent: spTaskA},
			"set-parent bbbbbbbb: parent=aaaaaaaa → (root)"},
		{"swap-to-root", SetParentOpts{TaskID: spTaskB, Swap: true},
			SetParentResult{OldParent: spTaskA, SwappedID: spTaskA},
			"set-parent bbbbbbbb --swap: bbbbbbbb now under (root), aaaaaaaa now under bbbbbbbb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SetParentMessage(tc.opts, tc.res); got != tc.want {
				t.Fatalf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestSetParentStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		status protocol.SetParentStatus
		frag   string
	}{
		{protocol.SetParentStatus_WouldCycle, "descendant"},
		{protocol.SetParentStatus_NoParent, "no parent"},
		{protocol.SetParentStatus_NotOperator, "operator"},
		{protocol.SetParentStatus_ParentNotFound, "parent"},
		{protocol.SetParentStatus_NotFound, "no such task"},
	} {
		fc := &fakeSetParentClient{resp: protocol.SetParentResponse{Status: tc.status}}
		_, err := SetParentWith(context.Background(), fc, SetParentOpts{TaskID: spTaskB})
		if err == nil || !strings.Contains(err.Error(), tc.frag) {
			t.Fatalf("status %v: err = %v, want fragment %q", tc.status, err, tc.frag)
		}
	}
}
