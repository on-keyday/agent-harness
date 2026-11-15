//go:build js

package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Server-kept cron schedules on the WebUI (pillar S). Every call goes through
// cli's one builder (ScheduleSpecOptsFromAction, reached by ScheduleSpecWith)
// and the cli renderers the CLI and the TUI use, so a schedule made from the
// dialog's Compose fields, from the command line or from `harness-cli` is the
// same request and reads the same words.
//
// scheduleCreate and scheduleEdit take the verb's own argument line (the
// tokens after `schedule new` / `schedule edit`): the command line passes what
// was typed and Compose serializes its fields to the same grammar, so the
// parser stays in Go (checklist 34a). A rejection carries the cli status
// message unprefixed -- it already names the verb.

// schedulePromise is recordPromise without the verb prefix: cli's own errors
// read `schedule new: <status>: <message>`.
func schedulePromise(fn func(c *cli.Client) (any, error)) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve, reject := promiseArgs[0], promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, fmt.Errorf("schedule: %w", err))
				return
			}
			v, err := fn(c)
			if err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessScheduleCreate(tokens) → Promise<string>: `schedule new <tokens>`.
func harnessScheduleCreate(this js.Value, args []js.Value) any {
	return scheduleSpecPromise(args, false)
}

// harnessScheduleEdit(tokens) → Promise<string>: `schedule edit <tokens>`
// (the schedule id first).
func harnessScheduleEdit(this js.Value, args []js.Value) any {
	return scheduleSpecPromise(args, true)
}

func scheduleSpecPromise(args []js.Value, edit bool) any {
	var tokens []string
	if len(args) > 0 {
		tokens = commandFields(args[0])
	}
	return schedulePromise(func(c *cli.Client) (any, error) {
		parse, name := verb.ParseCmdScheduleNew, "schedule new"
		if edit {
			parse, name = verb.ParseCmdScheduleEdit, "schedule edit"
		}
		a, err := parse(verb.WebUI, tokens, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		var out bytes.Buffer
		if err := cli.ScheduleSpecWith(rootCtx, c, a, &out); err != nil {
			return nil, err
		}
		return out.String(), nil
	})
}

// harnessScheduleRun(id) / harnessScheduleDelete(id) → Promise<string>: the
// line `schedule run` / `schedule rm` prints, naming the schedule and the change.
func harnessScheduleRun(this js.Value, args []js.Value) any {
	return scheduleIDPromise(argString(args, 0), verb.SubRun)
}

func harnessScheduleDelete(this js.Value, args []js.Value) any {
	return scheduleIDPromise(argString(args, 0), verb.SubRm)
}

func scheduleIDPromise(id, sub string) any {
	return schedulePromise(func(c *cli.Client) (any, error) {
		var out bytes.Buffer
		if err := cli.ScheduleIDWith(rootCtx, c, verb.ScheduleIDAction{ID: id, Sub: sub}, &out); err != nil {
			return nil, err
		}
		return out.String(), nil
	})
}

// scheduleRowJS is one `schedule ls` row as the dialog reads it. Every cell is
// the cli cell the TUI table shows, and line is ScheduleRowLine (what the
// command line prints).
func scheduleRowJS(r protocol.ScheduleRow, now time.Time) map[string]any {
	taskID := ""
	if r.TaskState == protocol.ScheduleTaskState_Bound {
		taskID = hex.EncodeToString(r.TaskId.Id[:])
	}
	return map[string]any{
		"id":        hex.EncodeToString(r.Id.Id[:]),
		"cron":      string(r.Cron),
		"tz":        string(r.Tz),
		"next":      cli.ScheduleNextCell(r),
		"last":      cli.ScheduleLastCell(r, now),
		"task":      cli.ScheduleTaskCell(r),
		"taskState": cli.EnumWireName(r.TaskState.String()),
		"taskId":    taskID,
		"agent":     string(r.Agent),
		"repo":      string(r.Repo),
		"about":     cli.ScheduleAboutCell(r),
		"line":      cli.ScheduleRowLine(r, now),
	}
}

// harnessScheduleList({json}) → Promise<{rows, text}>: text is what
// `schedule ls` (or --json) prints, rows the same list for the dialog.
func harnessScheduleList(this js.Value, args []js.Value) any {
	asJSON := len(args) > 0 && args[0].Type() == js.TypeObject && args[0].Get("json").Truthy()
	return schedulePromise(func(c *cli.Client) (any, error) {
		rows, err := c.ScheduleList(rootCtx)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, scheduleRowJS(r, now))
		}
		var text bytes.Buffer
		if asJSON {
			// The one JSON document: its serializer is cli's.
			if err := cli.ScheduleListJSON(&text, rows, now); err != nil {
				return nil, err
			}
		} else {
			cli.ScheduleListText(&text, rows, now)
		}
		return map[string]any{"rows": out, "text": text.String()}, nil
	})
}

// harnessScheduleShow(id, {json}) → Promise<detail>: text is `schedule show`'s
// output; head / waits / historyTitle / history are its parts, wait statistics
// as numbers beside the line.
func harnessScheduleShow(this js.Value, args []js.Value) any {
	id := argString(args, 0)
	asJSON := len(args) > 1 && args[1].Type() == js.TypeObject && args[1].Get("json").Truthy()
	return schedulePromise(func(c *cli.Client) (any, error) {
		d, err := c.ScheduleShow(rootCtx, id)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		p := cli.ScheduleDetailParts(d, now)
		head := make([]any, 0, len(p.Head))
		for _, l := range p.Head {
			head = append(head, l)
		}
		hist := make([]any, 0, len(p.History))
		for _, l := range p.History {
			hist = append(hist, l)
		}
		n, median, max := cli.WaitStats(d.History)
		var text bytes.Buffer
		if asJSON {
			if err := cli.ScheduleDetailJSON(&text, d, now); err != nil {
				return nil, err
			}
		} else {
			text.WriteString(cli.ScheduleDetailText(d, now))
		}
		return map[string]any{
			"id":           hex.EncodeToString(d.Row.Id.Id[:]),
			"row":          scheduleRowJS(d.Row, now),
			"head":         head,
			"waits":        p.Waits,
			"waitN":        float64(n),
			"waitMedianMs": float64(median.Milliseconds()),
			"waitMaxMs":    float64(max.Milliseconds()),
			"historyTitle": p.HistoryTitle,
			"history":      hist,
			"text":         text.String(),
		}, nil
	})
}
