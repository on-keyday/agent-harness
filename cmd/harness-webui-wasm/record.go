//go:build js

package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// PTY records on the WebUI (pillar A spec, A-D28): the task sheet lists a
// task's records with size and expiry, downloads one (raw or asciicast,
// rendered here by the CLI's own renderers), deletes it, and switches a live
// session's recording.

// recordPromise runs fn off the JS thread and settles a Promise with its
// result.
func recordPromise(what string, fn func(c *cli.Client) (any, error)) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve, reject := promiseArgs[0], promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			v, err := fn(c)
			if err != nil {
				rejectErr(reject, fmt.Errorf("%s: %w", what, err))
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

func argBool(args []js.Value, i int) bool {
	return i < len(args) && args[i].Type() == js.TypeBoolean && args[i].Bool()
}

// recordInfoJS is one record as the sheet reads it; times in unix ms, 0 = none.
func recordInfoJS(r protocol.RecordInfo) map[string]any {
	ms := func(ns uint64) float64 { return float64(ns / uint64(time.Millisecond)) }
	end := ""
	if r.EndReason != protocol.EndReason_None {
		end = cli.EnumWireName(r.EndReason.String())
	}
	return map[string]any{
		"taskId":          hex.EncodeToString(r.TaskId.Id[:]),
		"run":             float64(r.Run),
		"startedMs":       ms(r.StartedNs),
		"endedMs":         ms(r.EndedNs),
		"endReason":       end,
		"truncatedFromMs": ms(r.TruncatedFromNs),
		"entryBytes":      float64(r.EntryBytes),
		"diskBytes":       float64(r.DiskBytes),
		"forever":         r.Forever(),
		"recording":       r.Recording(),
		"expiresMs":       ms(r.ExpiresNs),
		"row":             cli.RecordRunRow(r),
	}
}

// harnessRecordList(taskId) → Promise<Array<record>>.
func harnessRecordList(this js.Value, args []js.Value) any {
	task := argString(args, 0)
	return recordPromise("record ls", func(c *cli.Client) (any, error) {
		rs, err := c.RecordList(rootCtx, task)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(rs))
		for _, r := range rs {
			out = append(out, recordInfoJS(r))
		}
		return out, nil
	})
}

// harnessRecordGet(ref, format, withSynth) → Promise<Uint8Array>: the raw
// output or asciicast v2, rendered by the CLI's code so a download is
// byte-for-byte what `record get` writes.
func harnessRecordGet(this js.Value, args []js.Value) any {
	ref, format, withSynth := argString(args, 0), argString(args, 1), true
	if len(args) > 2 && args[2].Type() == js.TypeBoolean {
		withSynth = args[2].Bool()
	}
	return recordPromise("record get", func(c *cli.Client) (any, error) {
		var buf bytes.Buffer
		if err := cli.RecordGetWith(rootCtx, c, ref, format, withSynth, &buf); err != nil {
			return nil, err
		}
		u8 := js.Global().Get("Uint8Array").New(buf.Len())
		js.CopyBytesToJS(u8, buf.Bytes())
		return u8, nil
	})
}

// harnessRecordDelete(ref, allRuns) → Promise<void>.
func harnessRecordDelete(this js.Value, args []js.Value) any {
	ref, all := argString(args, 0), argBool(args, 1)
	return recordPromise("record rm", func(c *cli.Client) (any, error) {
		if err := cli.CheckRecordRm(ref, all); err != nil {
			return nil, err
		}
		return js.Undefined(), c.RecordDelete(rootCtx, ref, all)
	})
}

// harnessRecordKeep(ref, forever, ttlMs) → Promise<void>.
func harnessRecordKeep(this js.Value, args []js.Value) any {
	ref, forever := argString(args, 0), argBool(args, 1)
	var ttl time.Duration
	if len(args) > 2 && args[2].Type() == js.TypeNumber {
		ttl = time.Duration(args[2].Int()) * time.Millisecond
	}
	return recordPromise("record keep", func(c *cli.Client) (any, error) {
		return js.Undefined(), c.RecordKeep(rootCtx, ref, forever, ttl)
	})
}

// harnessRecordSwitch(taskId, on) → Promise<void>.
func harnessRecordSwitch(this js.Value, args []js.Value) any {
	task, on := argString(args, 0), argBool(args, 1)
	return recordPromise("record switch", func(c *cli.Client) (any, error) {
		return js.Undefined(), c.RecordSwitch(rootCtx, task, on)
	})
}

// recordFromOpts reads a session open's `record` option: "default" (or
// absent) says nothing, "on" / "off" say it (A-D22).
func recordFromOpts(opts js.Value, f *cli.RunModeFlags) error {
	switch v := optString(opts, "record"); v {
	case "", "default":
	case "on", "off":
		f.RecordSet, f.Record = true, v == "on"
	default:
		return fmt.Errorf("record %q: want default, on or off", v)
	}
	return nil
}
