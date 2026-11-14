package runner

import (
	"github.com/on-keyday/objtrsf/exec/frame"
	"github.com/on-keyday/objtrsf/trsf"
)

// writeEndFrame sends the session's last frame: why it is ending (objtrsf
// exec/frame End), so the client attached to stream — and the server's mux,
// which reads it before the close — can say so instead of seeing the stream
// just close. exitCode is -1 when no process ran. Best effort: a failed write
// leaves the close and the task's failure, which carry the reason anyway.
func writeEndFrame(stream trsf.BidirectionalStream, exitCode int32, reason string) {
	e := frame.End{ExitCode: exitCode}
	if len(reason) > 0xffff {
		reason = reason[:0xffff]
	}
	e.SetReason([]byte(reason))
	ctrl := frame.Control{Type: frame.ControlType_End}
	ctrl.SetEnd(e)
	enc, err := ctrl.Append(nil)
	if err != nil {
		return
	}
	hdr := frame.FrameHeader{Type: frame.FrameType_Control, Len: uint32(len(enc))}
	_ = stream.AppendData(false, hdr.MustAppend(nil), enc)
}
