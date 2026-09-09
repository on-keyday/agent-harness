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

// The split is only safe because the two formats encode identically: an address
// field re-typed from RunnerID to ConnID must stay readable by a peer built
// before the change, and vice versa. Assert it directly rather than inferring
// it from the two definitions looking alike.
func TestConnIDWireIdenticalToRunnerID(t *testing.T) {
	for _, s := range []string{"ws:127.0.0.1:8539-7", "udp:[::1]:9000-65535"} {
		cid, err := objproto.ParseConnectionID(s, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		c := ConnIDFromObjproto(cid)
		cBytes, err := c.Append(nil)
		if err != nil {
			t.Fatalf("encode ConnID: %v", err)
		}
		r := ConnIDToRunnerID(cid)
		rBytes, err := r.Append(nil)
		if err != nil {
			t.Fatalf("encode RunnerID: %v", err)
		}
		if !bytes.Equal(cBytes, rBytes) {
			t.Fatalf("%q: ConnID encodes %x, RunnerID encodes %x", s, cBytes, rBytes)
		}
		// Old peer reads a new field: decode ConnID bytes as a RunnerID.
		var asRunner RunnerID
		if _, err := asRunner.Decode(cBytes); err != nil {
			t.Fatalf("decode ConnID bytes as RunnerID: %v", err)
		}
		if got := RunnerIDToConnID(asRunner).String(); got != cid.String() {
			t.Fatalf("cross-decode: got %q want %q", got, cid.String())
		}
		// New peer reads an old field: decode RunnerID bytes as a ConnID.
		var asConn ConnID
		if _, err := asConn.Decode(rBytes); err != nil {
			t.Fatalf("decode RunnerID bytes as ConnID: %v", err)
		}
		if got := asConn.ToObjproto().String(); got != cid.String() {
			t.Fatalf("cross-decode back: got %q want %q", got, cid.String())
		}
	}
	// The absent form matters most: the data-plane client branches on
	// TransportLen != 0, so a zero of either type must be the same byte.
	var zeroConn ConnID
	var zeroRunner RunnerID
	zc, _ := zeroConn.Append(nil)
	zr, _ := zeroRunner.Append(nil)
	if !bytes.Equal(zc, zr) {
		t.Fatalf("absent form differs: ConnID %x, RunnerID %x", zc, zr)
	}
}
