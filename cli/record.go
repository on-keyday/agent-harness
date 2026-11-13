package cli

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// PTY records (pillar A spec, A1): the five record verbs. Every one names a
// task and needs a bit within that bit's scope — exec_view to read,
// record_persist to keep, record_delete to withhold or delete.

func recordStatusError(verb string, st protocol.RecordStatus) error {
	switch st {
	case protocol.RecordStatus_Ok:
		return nil
	case protocol.RecordStatus_NotFound:
		return fmt.Errorf("record %s: not_found: no such task, run or record — or it is outside your scope", verb)
	case protocol.RecordStatus_NotRecordable:
		return fmt.Errorf("record %s: not_recordable: the task has no live interactive session (a stream session is never recorded)", verb)
	}
	return fmt.Errorf("record %s: %s", verb, EnumWireName(st.String()))
}

func (c *Client) recordRoundTrip(ctx context.Context, kind protocol.TaskControlKind, set func(*protocol.TaskControlRequest)) (*protocol.TaskControlResponse, error) {
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: kind}}
	set(req)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Kind != kind {
		return nil, fmt.Errorf("record: unexpected response kind=%v", resp.Kind)
	}
	return resp, nil
}

func recordRef(ref string) (protocol.RecordRef, error) {
	task, run, err := ParseRecordRef(ref)
	if err != nil {
		return protocol.RecordRef{}, err
	}
	tid, err := parseTaskIDHex(task)
	if err != nil {
		return protocol.RecordRef{}, err
	}
	return protocol.RecordRef{TaskId: tid, Run: run}, nil
}

// RecordList lists the records of one task, or every record in the caller's
// exec_view scope when taskIDHex is "".
func (c *Client) RecordList(ctx context.Context, taskIDHex string) ([]protocol.RecordInfo, error) {
	var lr protocol.RecordListRequest
	if taskIDHex != "" {
		tid, err := parseTaskIDHex(taskIDHex)
		if err != nil {
			return nil, err
		}
		lr.SetTaskPresent(true)
		lr.SetTaskId(tid)
	}
	resp, err := c.recordRoundTrip(ctx, protocol.TaskControlKind_RecordList, func(r *protocol.TaskControlRequest) { r.SetRecordList(lr) })
	if err != nil {
		return nil, err
	}
	r := resp.RecordList()
	if r == nil {
		return nil, fmt.Errorf("record ls: empty response")
	}
	if err := recordStatusError("ls", r.Status); err != nil {
		return nil, err
	}
	var body protocol.RecordListBody
	if err := ReadResponseBody(ctx, c.Transport(), r.StreamId, &body); err != nil {
		return nil, err
	}
	return body.Records, nil
}

// streamReader reads a receive stream as an io.Reader.
type streamReader struct {
	ctx context.Context
	st  trsf.ReceiveStream
	buf []byte
	eof bool
}

func (r *streamReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		b, eof, err := r.st.ReadDirect(64 << 10)
		if err != nil {
			return 0, err
		}
		r.buf, r.eof = b, eof
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// RecordGet streams one run's record (ref is `<task-id>` for the latest run
// or `<task-id>#<n>`) and calls fn for each entry, oldest first.
func (c *Client) RecordGet(ctx context.Context, ref string, fn func(protocol.RecordEntry) error) error {
	rr, err := recordRef(ref)
	if err != nil {
		return err
	}
	resp, err := c.recordRoundTrip(ctx, protocol.TaskControlKind_RecordGet, func(r *protocol.TaskControlRequest) {
		r.SetRecordGet(protocol.RecordGetRequest{Ref: rr})
	})
	if err != nil {
		return err
	}
	g := resp.RecordGet()
	if g == nil {
		return fmt.Errorf("record get: empty response")
	}
	if err := recordStatusError("get", g.Status); err != nil {
		return err
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(g.StreamId))
	if st == nil {
		return fmt.Errorf("record get: stream %d not visible after the response", g.StreamId)
	}
	rd := bufio.NewReaderSize(&streamReader{ctx: ctx, st: st}, 64<<10)
	for {
		var e protocol.RecordEntry
		if err := e.Read(rd); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("record get: %w", err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
}

// RecordDelete deletes one run's record, or every run's with allRuns.
func (c *Client) RecordDelete(ctx context.Context, ref string, allRuns bool) error {
	rr, err := recordRef(ref)
	if err != nil {
		return err
	}
	resp, err := c.recordRoundTrip(ctx, protocol.TaskControlKind_RecordDelete, func(r *protocol.TaskControlRequest) {
		d := protocol.RecordDeleteRequest{Ref: rr}
		d.SetAllRuns(allRuns)
		r.SetRecordDelete(d)
	})
	if err != nil {
		return err
	}
	if d := resp.RecordDelete(); d == nil {
		return fmt.Errorf("record rm: empty response")
	} else {
		return recordStatusError("rm", d.Status)
	}
}

// RecordKeep changes a run's retention: forever, or ttl from the run's end.
func (c *Client) RecordKeep(ctx context.Context, ref string, forever bool, ttl time.Duration) error {
	rr, err := recordRef(ref)
	if err != nil {
		return err
	}
	resp, err := c.recordRoundTrip(ctx, protocol.TaskControlKind_RecordKeep, func(r *protocol.TaskControlRequest) {
		k := protocol.RecordKeepRequest{Ref: rr, TtlMs: uint64(ttl.Milliseconds())}
		k.SetForever(forever)
		r.SetRecordKeep(k)
	})
	if err != nil {
		return err
	}
	if k := resp.RecordKeep(); k == nil {
		return fmt.Errorf("record keep: empty response")
	} else {
		return recordStatusError("keep", k.Status)
	}
}

// RecordSwitch switches a live session's recording on or off.
func (c *Client) RecordSwitch(ctx context.Context, taskIDHex string, on bool) error {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return err
	}
	word := "off"
	if on {
		word = "on"
	}
	resp, err := c.recordRoundTrip(ctx, protocol.TaskControlKind_RecordSwitch, func(r *protocol.TaskControlRequest) {
		s := protocol.RecordSwitchRequest{TaskId: tid}
		s.SetOn(on)
		r.SetRecordSwitch(s)
	})
	if err != nil {
		return err
	}
	if s := resp.RecordSwitch(); s == nil {
		return fmt.Errorf("record %s: empty response", word)
	} else {
		return recordStatusError(word, s.Status)
	}
}

// --- text surfaces (CLI, the TUI command line) ------------------------------

func recordTime(ns uint64) string {
	if ns == 0 {
		return "-"
	}
	return time.Unix(0, int64(ns)).Local().Format("2006-01-02 15:04:05")
}

// RecordRow is one record as `record ls` prints it.
func RecordRow(r protocol.RecordInfo) string {
	id := hex.EncodeToString(r.TaskId.Id[:])
	ended := recordTime(r.EndedNs)
	if r.Recording() {
		ended = "recording"
	}
	expires := "forever"
	if !r.Forever() {
		expires = "at run end + ttl"
		if r.ExpiresNs != 0 {
			expires = recordTime(r.ExpiresNs)
		}
	}
	end := "-"
	if r.EndReason != protocol.EndReason_None {
		end = EnumWireName(r.EndReason.String())
	}
	row := fmt.Sprintf("%s#%d  started=%s  ended=%s  end=%s  size=%s (raw %s)  expires=%s",
		id, r.Run, recordTime(r.StartedNs), ended, end, humanBytes(r.DiskBytes), humanBytes(r.EntryBytes), expires)
	if r.TruncatedFromNs != 0 {
		row += "  truncated-from=" + recordTime(r.TruncatedFromNs)
	}
	return row
}

func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// RecordLsWith prints the records of one task, or all in scope.
func RecordLsWith(ctx context.Context, c *Client, taskIDHex string, out io.Writer) error {
	rs, err := c.RecordList(ctx, taskIDHex)
	if err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Fprintln(out, "no records")
		return nil
	}
	for _, r := range rs {
		fmt.Fprintln(out, RecordRow(r))
	}
	return nil
}

// RecordGetWith writes one run's record to out: the raw output (what
// `session snapshot --raw` prints), or asciicast v2 with format "asciicast".
func RecordGetWith(ctx context.Context, c *Client, ref, format string, withSynth bool, out io.Writer) error {
	switch format {
	case "", "raw":
		return c.RecordGet(ctx, ref, func(e protocol.RecordEntry) error {
			if isOutput(e, withSynth) {
				_, err := out.Write(e.Payload)
				return err
			}
			return nil
		})
	case "asciicast":
		var es []protocol.RecordEntry
		if err := c.RecordGet(ctx, ref, func(e protocol.RecordEntry) error {
			if !withSynth && frameTypeIsSynth(e) {
				return nil
			}
			es = append(es, e)
			return nil
		}); err != nil {
			return err
		}
		b, err := RenderAsciicast(es)
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err
	}
	return fmt.Errorf("record get: --format %q: want raw or asciicast", format)
}

// CheckRecordRm refuses `record rm <task-id>` with no run and no --all-runs:
// deleting every run of a task is said out loud, never implied.
func CheckRecordRm(ref string, allRuns bool) error {
	_, run, err := ParseRecordRef(ref)
	if err != nil {
		return err
	}
	if run == 0 && !allRuns {
		return fmt.Errorf("record rm %s: name a run (<task-id>#<n>) or pass --all-runs", ref)
	}
	return nil
}

// RecordRmWith deletes and says so.
func RecordRmWith(ctx context.Context, c *Client, ref string, allRuns bool, out io.Writer) error {
	if err := c.RecordDelete(ctx, ref, allRuns); err != nil {
		return err
	}
	what := ref
	if allRuns {
		what = strings.SplitN(ref, "#", 2)[0] + " (every run)"
	}
	fmt.Fprintf(out, "record rm: deleted %s\n", what)
	return nil
}

// RecordKeepWith changes a retention and says so.
func RecordKeepWith(ctx context.Context, c *Client, ref string, forever bool, ttl time.Duration, out io.Writer) error {
	if err := c.RecordKeep(ctx, ref, forever, ttl); err != nil {
		return err
	}
	if forever {
		fmt.Fprintf(out, "record keep: %s is kept forever\n", ref)
	} else {
		fmt.Fprintf(out, "record keep: %s is kept for %s after its run ends\n", ref, ttl)
	}
	return nil
}

// RecordSwitchWith switches recording and says so.
func RecordSwitchWith(ctx context.Context, c *Client, taskIDHex string, on bool, out io.Writer) error {
	if err := c.RecordSwitch(ctx, taskIDHex, on); err != nil {
		return err
	}
	if on {
		fmt.Fprintf(out, "record on: %s is being recorded\n", taskIDHex)
	} else {
		fmt.Fprintf(out, "record off: %s is no longer recorded (its record stays)\n", taskIDHex)
	}
	return nil
}

// recordDial opens a fresh client for one CLI call.
func recordDial(ctx context.Context, peerCID objproto.ConnectionID, fn func(*Client) error) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

// RecordLs, RecordGetOut, RecordRm, RecordKeepOut and RecordSwitchOut are the
// fresh-dial forms for the CLI.
func RecordLs(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex string, out io.Writer) error {
	return recordDial(ctx, peerCID, func(c *Client) error { return RecordLsWith(ctx, c, taskIDHex, out) })
}

func RecordGetOut(ctx context.Context, peerCID objproto.ConnectionID, ref, format string, withSynth bool, out io.Writer) error {
	return recordDial(ctx, peerCID, func(c *Client) error { return RecordGetWith(ctx, c, ref, format, withSynth, out) })
}

func RecordRm(ctx context.Context, peerCID objproto.ConnectionID, ref string, allRuns bool, out io.Writer) error {
	return recordDial(ctx, peerCID, func(c *Client) error { return RecordRmWith(ctx, c, ref, allRuns, out) })
}

func RecordKeepOut(ctx context.Context, peerCID objproto.ConnectionID, ref string, forever bool, ttl time.Duration, out io.Writer) error {
	return recordDial(ctx, peerCID, func(c *Client) error { return RecordKeepWith(ctx, c, ref, forever, ttl, out) })
}

func RecordSwitchOut(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex string, on bool, out io.Writer) error {
	return recordDial(ctx, peerCID, func(c *Client) error { return RecordSwitchWith(ctx, c, taskIDHex, on, out) })
}
