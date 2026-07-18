package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestAckErrorMessages(t *testing.T) {
	cases := []struct {
		op     string
		status protocol.FileTransferStatus
		want   string // substring
	}{
		{"push", protocol.FileTransferStatus_NotFound, "parent directory does not exist (use -p/--parents"},
		{"push --recursive", protocol.FileTransferStatus_NotFound, "parent directory does not exist"},
		{"pull", protocol.FileTransferStatus_NotFound, "not found"},
		{"mkdir", protocol.FileTransferStatus_NotFound, "parent directory does not exist (use -p/--parents)"},
		{"mkdir", protocol.FileTransferStatus_AlreadyExists, "directory already exists"},
		{"mkdir", protocol.FileTransferStatus_NotADirectory, "exists and is not a directory"},
	}
	for _, tc := range cases {
		err := ackError(tc.op, &protocol.FileTransferAck{Status: tc.status})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ackError(%q,%v) = %v, want substring %q", tc.op, tc.status, err, tc.want)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	err := ackError("push", &protocol.FileTransferAck{Status: protocol.FileTransferStatus_NotFound})
	if !IsNotFound(err) {
		t.Error("IsNotFound(not_found ack) = false")
	}
	if IsNotFound(ackError("push", &protocol.FileTransferAck{Status: protocol.FileTransferStatus_IoError})) {
		t.Error("IsNotFound(io_error ack) = true")
	}
}
