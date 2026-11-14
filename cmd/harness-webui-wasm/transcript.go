//go:build js

package main

import (
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Agent transcripts on the WebUI (pillar B): the task sheet lists a task's
// agent sessions, opens one to list its files, and downloads a file or the
// whole session as a tar. Errors name the verb, as recordPromise's do.

// transcriptSessionJS is one session as the sheet reads it; times in unix ms.
func transcriptSessionJS(s protocol.TranscriptSession) map[string]any {
	ms := func(ns uint64) float64 { return float64(ns / uint64(time.Millisecond)) }
	return map[string]any{
		"agent":     string(s.Agent),
		"id":        string(s.Id),
		"createdMs": ms(s.CreatedNs),
		"updatedMs": ms(s.UpdatedNs),
		"size":      float64(s.Size),
		"sizeKnown": s.SizeKnown(),
		"shared":    s.Shared(),
		"row":       cli.TranscriptSessionRow(s),
	}
}

// harnessTranscriptSessions(taskId) → Promise<{sessions, note}>: note names
// the stores that could not be read, and the sheet shows it.
func harnessTranscriptSessions(this js.Value, args []js.Value) any {
	task := argString(args, 0)
	return recordPromise("transcript ls", func(c *cli.Client) (any, error) {
		ss, note, err := c.TranscriptSessions(rootCtx, task)
		if err != nil {
			return nil, err
		}
		rows := make([]any, 0, len(ss))
		for _, s := range ss {
			rows = append(rows, transcriptSessionJS(s))
		}
		return map[string]any{"sessions": rows, "note": note}, nil
	})
}

// harnessTranscriptFiles(taskId, session) → Promise<Array<file>>.
func harnessTranscriptFiles(this js.Value, args []js.Value) any {
	task, session := argString(args, 0), argString(args, 1)
	return recordPromise("transcript ls", func(c *cli.Client) (any, error) {
		fs, err := c.TranscriptFiles(rootCtx, task, session)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(fs))
		for _, f := range fs {
			out = append(out, map[string]any{
				"name":      string(f.Name),
				"size":      float64(f.Size),
				"sizeKnown": f.SizeKnown(),
				"row":       cli.TranscriptFileRow(f),
			})
		}
		return out, nil
	})
}

// jsChunks hands each write to JS as its own Uint8Array, so a large
// transcript or tar is never held whole in the Go heap (Review Focus 4); the
// page makes one Blob of the parts.
type jsChunks struct{ parts []any }

func (w *jsChunks) Write(p []byte) (int, error) {
	u8 := js.Global().Get("Uint8Array").New(len(p))
	js.CopyBytesToJS(u8, p)
	w.parts = append(w.parts, u8)
	return len(p), nil
}

// harnessTranscriptGet(taskId, session, file) → Promise<Array<Uint8Array>>:
// one file as the agent stored it, or with file "" the whole session as a tar.
func harnessTranscriptGet(this js.Value, args []js.Value) any {
	task, session, file := argString(args, 0), argString(args, 1), argString(args, 2)
	return recordPromise("transcript get", func(c *cli.Client) (any, error) {
		var w jsChunks
		if err := cli.TranscriptGetWith(rootCtx, c, task, session, file, &w); err != nil {
			return nil, err
		}
		return w.parts, nil
	})
}
