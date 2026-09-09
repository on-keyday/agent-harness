package protocol

import (
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
