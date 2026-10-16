package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/objtrsf/trsf"
)

// BodyDecoder is a <Response>Body format: the rows a list-shaped board
// response carries on a stream rather than in the response itself (see
// BoardTopicsResponse in runner/protocol/message.bgn for why).
type BodyDecoder interface {
	DecodeExact(buf []byte) error
}

// ReadResponseBody reads the stream a list-shaped board response names and
// decodes it into body. Every reader of those responses, on both faces, goes
// through here.
//
// A zero stream id is an error, not an empty list: the server answers 0 only
// when it could not encode the rows or allocate the stream, and an empty list
// still arrives as a body with no rows. Callers check the response's own
// status first — a not_found carries no stream.
func ReadResponseBody(ctx context.Context, tr trsf.Transport, streamID uint64, body BodyDecoder) error {
	if streamID == 0 {
		return fmt.Errorf("server sent no body stream (it could not encode the rows or allocate a stream; see its log)")
	}
	st := waitForReceiveStream(ctx, tr, trsf.StreamID(streamID))
	if st == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("body stream %d not visible after the response", streamID)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return fmt.Errorf("body stream %d read: %w", streamID, err)
		}
		raw = append(raw, data...)
		if eof {
			break
		}
	}
	if err := body.DecodeExact(raw); err != nil {
		return fmt.Errorf("decode body (%d bytes): %w", len(raw), err)
	}
	return nil
}
