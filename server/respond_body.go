package server

import "log/slog"

// bodyEncoder is a <Response>Body format: the rows of a list-shaped
// response, which travel on a stream rather than in the response (see
// BoardTopicsResponse in message.bgn for why).
type bodyEncoder interface {
	EncodeCopy(reserved []byte) ([]byte, error)
}

// respondWithBody sends a response whose rows travel on a stream, then its
// rows. respond builds and sends the response carrying the stream id it is
// handed.
//
// Every such response goes through here — the board's on both faces, and an
// ambiguous spawn's candidates — because the order is the part that has to
// hold: the response first, then the body. The peer reads a stream only once
// it has been told about it, so a body written before the response lands only
// as far as the receive window, and past that the write never completes —
// board bodies carry up to the board's per-message limit per row. The write
// runs on its own goroutine: the handlers are driven from the connection's
// receive loop, and a peer that stops reading must not stall it.
//
// An encode or stream-allocation failure answers stream id 0, which readers
// report as an error rather than as an empty list.
func respondWithBody(conn ConnHandle, what string, body bodyEncoder, respond func(streamID uint64)) {
	b, err := body.EncodeCopy(nil)
	if err != nil {
		slog.Error("response body: encode", "what", what, "err", err)
		respond(0)
		return
	}
	stream := conn.CreateSendStream()
	if stream == nil {
		slog.Warn("response body: no send stream", "what", what)
		respond(0)
		return
	}
	respond(uint64(stream.ID()))
	go func() {
		if err := stream.AppendData(true, b); err != nil {
			slog.Warn("response body: write", "what", what, "stream", stream.ID(), "err", err)
		}
	}()
}
