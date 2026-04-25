package runner

import (
	"testing"
)

// TestConnSenderPublishCachesStreamPerTopic verifies that connSender creates one
// bidirectional stream per topic and caches it for subsequent Publish calls.
//
// Full verification is deferred because implementing faithful fakes for
// trsf.Transport (CreateBidirectionalStream, AcceptBidirectionalStream) and
// objproto.Connection (SendMessage) requires reproducing non-trivial internal
// trsf stream machinery. The behaviour is exercised end-to-end in Task 5.1.
func TestConnSenderPublishCachesStreamPerTopic(t *testing.T) {
	t.Skip("requires fake trsf.Transport stub — deferred to integration test in Task 5.1")
}
