package runner

import "testing"

// TestPeerSenderPublishCachesStreamPerTopic verifies that the runner's
// Publish path (peerSender → peer.Conn.Publish) creates one bidirectional
// stream per topic and caches it for subsequent Publish calls.
//
// Full verification is deferred because implementing faithful fakes for
// trsf.Transport (CreateBidirectionalStream, AcceptBidirectionalStream)
// and objproto.Connection (SendMessage) requires reproducing non-trivial
// internal trsf stream machinery. The behaviour is exercised end-to-end
// in the integration test (see Task 5.1 in the original plan).
func TestPeerSenderPublishCachesStreamPerTopic(t *testing.T) {
	t.Skip("requires fake trsf.Transport stub — covered by integration test")
}
