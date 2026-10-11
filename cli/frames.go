package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/on-keyday/agent-harness/pubsub"
)

// readFrames calls each with every message on a subscriber stream, in order,
// and returns when the stream ends (nil) or fails. A payload each cannot use is
// each's to report; the next message is unaffected, since frames delimit them.
func readFrames(st pubsub.StreamReader, topic string, each func(payload []byte)) error {
	fr := pubsub.NewFrameReader(st)
	for {
		payload, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			slog.Warn("subscription stream", "topic", topic, "err", err)
			return err
		}
		each(payload)
	}
}

// watchFrames runs each reader and blocks until ctx is done or a reader fails,
// returning that failure. A reader that returns nil — its stream ended cleanly
// — is not a failure; the watch goes on until ctx.
func watchFrames(ctx context.Context, readers ...func() error) error {
	errCh := make(chan error, len(readers))
	for _, read := range readers {
		go func() {
			if err := read(); err != nil {
				errCh <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
