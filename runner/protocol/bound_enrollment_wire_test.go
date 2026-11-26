package protocol

import (
	"reflect"
	"testing"
)

// Bound enrollment (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C1/C2): an enrolled key may be bound to a set of tasks, and every format
// that describes a key or a runner on one carries that set.

func boundIDs(n int) []TaskID {
	out := make([]TaskID, n)
	for i := range out {
		out[i].Id[0] = byte(i + 1)
		out[i].Id[15] = 0xb0
	}
	return out
}

func TestRunnerEnrollRequestBoundRoundTrip(t *testing.T) {
	for _, n := range []int{0, 16} {
		var req RunnerEnrollRequest
		req.Key[0] = 0x3f
		req.SetLabel([]byte("host1"))
		if !req.SetBound(boundIDs(n)) {
			t.Fatalf("SetBound(%d) refused", n)
		}
		raw, err := req.EncodeCopy(nil)
		if err != nil {
			t.Fatal(err)
		}
		var got RunnerEnrollRequest
		if err := got.DecodeExact(raw); err != nil {
			t.Fatal(err)
		}
		if len(got.Bound) != n || (n > 0 && !reflect.DeepEqual(got.Bound, req.Bound)) || string(got.Label) != "host1" {
			t.Fatalf("n=%d: round trip = %+v, want %+v", n, got, req)
		}
	}
}

func TestRunnerKeyRowBoundRoundTrip(t *testing.T) {
	var row RunnerKeyRow
	row.SetLabel([]byte("ctl"))
	row.SetBound(boundIDs(2))
	raw, err := row.EncodeCopy(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got RunnerKeyRow
	if err := got.DecodeExact(raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Bound, row.Bound) {
		t.Fatalf("bound = %v, want %v", got.Bound, row.Bound)
	}
}

func TestRunnerUnenrollBodyBoundRoundTrip(t *testing.T) {
	var body RunnerUnenrollBody
	body.SetBound(boundIDs(3))
	raw, err := body.EncodeCopy(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got RunnerUnenrollBody
	if err := got.DecodeExact(raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Bound, body.Bound) {
		t.Fatalf("bound = %v, want %v", got.Bound, body.Bound)
	}
}

func TestRunnerInfoBoundRoundTrip(t *testing.T) {
	var info RunnerInfo
	info.SetHostname([]byte("h"))
	info.SetBound(boundIDs(1))
	raw, err := info.EncodeCopy(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got RunnerInfo
	if err := got.DecodeExact(raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Bound, info.Bound) {
		t.Fatalf("bound = %v, want %v", got.Bound, info.Bound)
	}
}

// The three enroll refusals exist and carry their wire names.
func TestRunnerKeyStatusBoundValues(t *testing.T) {
	for st, want := range map[RunnerKeyStatus]string{
		RunnerKeyStatus_OutOfReach:     "out_of_reach",
		RunnerKeyStatus_BindingDiffers: "binding_differs",
		RunnerKeyStatus_BadBinding:     "bad_binding",
	} {
		if got := st.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", st, got, want)
		}
	}
}
