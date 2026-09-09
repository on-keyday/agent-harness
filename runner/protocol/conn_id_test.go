package protocol

import (
	"bytes"
	"testing"

	"github.com/on-keyday/objtrsf/objproto"
)

func TestConnIDRoundTrip(t *testing.T) {
	for _, s := range []string{"ws:127.0.0.1:8539-7", "udp:[::1]:9000-65535", "ws:0.0.0.0:1-0"} {
		cid, err := objproto.ParseConnectionID(s, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if got := ConnIDFromObjproto(cid).ToObjproto(); got.String() != cid.String() {
			t.Fatalf("round trip %q: got %q", s, got.String())
		}
	}
	// A zero ConnID must not panic and must be recognisably absent.
	var zero ConnID
	_ = zero.ToObjproto().String()
	if zero.TransportLen != 0 {
		t.Fatal("zero ConnID reports a transport")
	}
}

// ConnID and RunnerID used to encode identically, and a test here asserted it
// — that identity was what let the five address fields be re-typed without a
// wire break. It is deliberately false now: RunnerID is 16 opaque bytes and
// ConnID is still an address, so the assertion was deleted rather than relaxed.
// What replaces it is the shape check below: the two types must NOT be
// interchangeable any more, which is the property the decoupling actually
// depends on.
func TestConnIDIsNotInterchangeableWithRunnerID(t *testing.T) {
	cid, err := objproto.ParseConnectionID("ws:127.0.0.1:8539-7", 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := ConnIDFromObjproto(cid)
	cBytes, err := c.Append(nil)
	if err != nil {
		t.Fatalf("encode ConnID: %v", err)
	}
	var r RunnerID
	rBytes, err := r.Append(nil)
	if err != nil {
		t.Fatalf("encode RunnerID: %v", err)
	}
	if len(rBytes) != 16 {
		t.Fatalf("RunnerID encodes %d bytes, want 16 opaque ones", len(rBytes))
	}
	if bytes.Equal(cBytes, rBytes) {
		t.Fatal("ConnID and RunnerID still encode identically; the decoupling did not land")
	}
}
