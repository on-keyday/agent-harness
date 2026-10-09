package cli

import (
	"context"
	"errors"

	"github.com/on-keyday/objtrsf/trsf"
)

// decodableRecord is a self-delimiting wire record behind a pointer.
type decodableRecord[R any] interface {
	*R
	Decode(buf []byte) ([]byte, error)
}

// recordReader turns a tap stream's byte chunks back into records.
//
// The stream is a CONCATENATION of self-delimiting records with no length
// prefix — a prefix would be a wire byte the schema does not describe — so a
// chunk can end mid-record and a chunk can hold several. push accumulates and
// decodes as far as it can, keeping the remainder.
type recordReader[R any, P decodableRecord[R]] struct {
	buf []byte
}

func (r *recordReader[R, P]) push(chunk []byte) ([]P, error) {
	r.buf = append(r.buf, chunk...)
	var out []P
	for len(r.buf) > 0 {
		rec := P(new(R))
		rest, err := rec.Decode(r.buf)
		if err != nil {
			// Short read: wait for more bytes. A truncated record and a
			// malformed one look the same here, and treating an incomplete tail
			// as an error would break every split chunk.
			break
		}
		out = append(out, rec)
		if len(rest) == len(r.buf) {
			return out, errors.New("tap: decoder made no progress")
		}
		r.buf = rest
	}
	return out, nil
}

// streamTapRecords reads records off st and hands them to onBatch, one call per
// chunk that completed at least one record, until the stream ends, ctx is
// cancelled (not a failure — the operator stopped it), or onBatch fails.
// Per chunk rather than per record so a UI that turns each call into a redraw
// redraws once for a burst.
func streamTapRecords[R any, P decodableRecord[R]](ctx context.Context, st trsf.BidirectionalStream, onBatch func([]P) error) error {
	var reader recordReader[R, P]
	for {
		data, eof, rerr := st.ReadDirectContext(ctx, 64*1024)
		if len(data) > 0 {
			recs, derr := reader.push(data)
			if derr != nil {
				return derr
			}
			if len(recs) > 0 {
				if err := onBatch(recs); err != nil {
					return err
				}
			}
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return rerr
		}
		if eof {
			return nil
		}
	}
}
