package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/hostlog"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// handleHostLog answers TaskControlKind_HostLog (pillar C,
// docs/superpowers/specs/2026-11-28-pillar-c-host-logs-design.md).
//
// Capability_HostLog is enforced centrally via authority.CentralCap (no scope,
// G19). Server logs are read directly from h.HostLog and written to the
// client's stream; runner logs are forwarded to the target runner via
// RunnerRequestType_HostLog and spliced back with spliceBidiHalfClose, matching
// handleTranscript and handleGitQuery.
func (h *TaskHandler) handleHostLog(_ objproto.ConnectionID, conn ConnHandle, req *protocol.TaskControlRequest) {
	r := req.HostLog()
	if r == nil {
		slog.Error("TaskHandler: HostLog variant is nil")
		return
	}
	var out protocol.HostLogResponse
	switch r.Target {
	case protocol.HostLogTarget_Server:
		out = h.serveServerHostLog(conn, r.Mode, r.Offset, r.MaxBytes)
	case protocol.HostLogTarget_Runner:
		out = h.forwardRunnerHostLog(conn, r.RunnerId, r.Mode, r.Offset, r.MaxBytes)
	default:
		out = protocol.HostLogResponse{Status: protocol.HostLogStatus_InternalError}
	}
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_HostLog, RequestId: req.Header.RequestId}
	resp.SetHostLog(out)
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

func (h *TaskHandler) serveServerHostLog(conn ConnHandle, mode protocol.HostLogMode, offset uint64, maxBytes uint32) protocol.HostLogResponse {
	if h.HostLog == nil {
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_Unavailable}
	}
	if conn == nil {
		slog.Error("hostlog: nil client conn (programmer error)")
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_InternalError}
	}
	clientStream := conn.CreateBidirectionalStream()
	if clientStream == nil {
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_InternalError}
	}
	reader := h.HostLog
	go func() {
		defer clientStream.CloseBoth()
		res, data := readHostLogResult(reader, mode, offset, maxBytes)
		hdr, err := res.Append(nil)
		if err != nil {
			slog.Error("hostlog: encode result", "err", err)
			return
		}
		if _, err := clientStream.Write(hdr); err != nil {
			slog.Error("hostlog: write result header", "err", err)
			return
		}
		if len(data) > 0 {
			if _, err := clientStream.Write(data); err != nil {
				slog.Error("hostlog: write log bytes", "err", err)
				return
			}
		}
		if err := clientStream.AppendData(true); err != nil {
			slog.Error("hostlog: half-close", "err", err)
		}
	}()
	return protocol.HostLogResponse{Status: protocol.HostLogStatus_Ok, StreamId: uint64(clientStream.ID())}
}

func (h *TaskHandler) forwardRunnerHostLog(conn ConnHandle, runnerID protocol.RunnerID, mode protocol.HostLogMode, offset uint64, maxBytes uint32) protocol.HostLogResponse {
	runner, ok := h.Registry.GetByIdentity(runnerID)
	if !ok || runner.Conn == nil {
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_RunnerOffline}
	}
	if !runner.HostLogs {
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_RunnerUnsupported}
	}
	if conn == nil {
		slog.Error("hostlog: nil client conn (programmer error)")
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_InternalError}
	}
	clientStream := conn.CreateBidirectionalStream()
	if clientStream == nil {
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_InternalError}
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = clientStream.CloseBoth()
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_InternalError}
	}
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_HostLog}
	rreq.SetHostLog(protocol.RunnerHostLogRequest{
		StreamId: uint64(runnerStream.ID()),
		Mode:     mode,
		Offset:   offset,
		MaxBytes: maxBytes,
	})
	if err := sendRunnerRequest(runner.Conn, &rreq); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("hostlog: send to runner failed", "runner_id", hex.EncodeToString(runnerID.Id[:]), "err", err)
		return protocol.HostLogResponse{Status: protocol.HostLogStatus_InternalError}
	}
	go spliceBidiHalfClose(clientStream, runnerStream, "hostlog:"+hex.EncodeToString(runnerID.Id[:4]))
	return protocol.HostLogResponse{Status: protocol.HostLogStatus_Ok, StreamId: uint64(clientStream.ID())}
}

func readHostLogResult(rd *hostlog.Reader, mode protocol.HostLogMode, offset uint64, maxBytes uint32) (protocol.HostLogResult, []byte) {
	if rd == nil {
		res := protocol.HostLogResult{Status: protocol.HostLogStatus_Unavailable}
		res.SetMessage([]byte("server host log reader not configured"))
		return res, nil
	}
	snap, err := rd.Read(mode, offset, maxBytes)
	if err != nil {
		res := protocol.HostLogResult{Status: protocol.HostLogStatus_IoError}
		res.SetMessage([]byte(err.Error()))
		return res, nil
	}
	return protocol.HostLogResult{
		Status:        protocol.HostLogStatus_Ok,
		Source:        snap.Source,
		TotalSize:     snap.TotalSize,
		StartOffset:   snap.StartOffset,
		ReturnedBytes: uint32(len(snap.Data)),
	}, snap.Data
}
