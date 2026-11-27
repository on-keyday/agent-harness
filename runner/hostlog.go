package runner

import (
	"context"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// handleHostLog answers one RunnerRequestType_HostLog on the stream the server
// allocated (pillar C, docs/superpowers/specs/2026-11-28-pillar-c-host-logs-design.md).
// Mirrors handleTranscript: wait for the stream, always write a HostLogResult
// header (plus raw bytes on ok), always half-close and close.
func (s *Session) handleHostLog(ctx context.Context, req *protocol.RunnerHostLogRequest) {
	log := s.logger()
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		log.Error("hostlog: stream not visible", "stream_id", req.StreamId)
		return
	}
	defer stream.CloseBoth()

	var (
		res  protocol.HostLogResult
		data []byte
	)
	if s.HostLog == nil {
		res = protocol.HostLogResult{Status: protocol.HostLogStatus_Unavailable}
		res.SetMessage([]byte("runner host log reader not configured"))
	} else {
		snap, err := s.HostLog.Read(req.Mode, req.Offset, req.MaxBytes)
		if err != nil {
			res = protocol.HostLogResult{Status: protocol.HostLogStatus_IoError}
			res.SetMessage([]byte(err.Error()))
		} else {
			res = protocol.HostLogResult{
				Status:        protocol.HostLogStatus_Ok,
				Source:        snap.Source,
				TotalSize:     snap.TotalSize,
				StartOffset:   snap.StartOffset,
				ReturnedBytes: uint32(len(snap.Data)),
			}
			data = snap.Data
		}
	}

	hdr, err := res.Append(nil)
	if err != nil {
		log.Error("hostlog: encode result", "err", err)
		return
	}
	if _, err := stream.Write(hdr); err != nil {
		log.Error("hostlog: write result header", "err", err)
		return
	}
	if len(data) > 0 {
		if _, err := stream.Write(data); err != nil {
			log.Error("hostlog: write log bytes", "err", err)
			return
		}
	}
	if err := stream.AppendData(true); err != nil {
		log.Error("hostlog: half-close", "err", err)
	}
}
