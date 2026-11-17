package cli

import (
	"fmt"
	"io"
	"time"
)

// WriteOverview prints o as `harness-cli overview` shows it: the five sections
// in the spec's order, each header carrying its count (zeros included) and,
// when that section's source failed, the error on the next line.
func WriteOverview(w io.Writer, o Overview, now time.Time) {
	fmt.Fprintf(w, "needs you: %d\n", o.NeedsYouCount)
	writeOverviewTasks(w, o.NeedsYou)

	g := o.GroupCounts
	fmt.Fprintf(w, "tasks: %d ready, %d working, %d no report, %d queued, %d finished\n",
		g[groupReady], g[groupWorking], g[groupNoReport], g[groupQueued], g[groupFinished])
	writeOverviewTasks(w, o.Live)

	fmt.Fprintf(w, "hosts: %d (%d slots, %d operator clients)\n", o.HostCount, o.SlotCount, o.OperatorClients)
	writeOverviewErr(w, o.HostsErr)
	for _, h := range o.Hosts {
		fmt.Fprintf(w, "  %-16s %-8s idle %d busy %d offline %d  tasks %d/%d\n",
			h.Hostname, h.Goos, h.Idle, h.Busy, h.Offline, h.ActiveTasks, h.MaxTasks)
	}

	fmt.Fprintf(w, "forwards: %d\n", o.ForwardCount)
	writeOverviewErr(w, o.ForwardsErr)
	for _, f := range o.Forwards {
		fmt.Fprintf(w, "  %-3s %-36s %-12s %s\n", f.Dir, f.Spec, f.Repo, shortHex(f.TaskID))
	}

	fmt.Fprintf(w, "board: %d topics, %d messages\n", o.TopicCount, o.MsgTotal)
	writeOverviewErr(w, o.BoardErr)
	for _, t := range o.Topics {
		fmt.Fprintf(w, "  %-24s %d msgs  %-8s %s\n", t.Name, t.MsgCount, OverviewAge(t.LastPublishMs, now), t.Head)
	}
}

func writeOverviewTasks(w io.Writer, ts []OverviewTask) {
	for _, t := range ts {
		fmt.Fprintf(w, "  %-10s %-16s %s  %s\n", t.Badge, t.Repo, shortHex(t.ID), t.Now)
	}
}

func writeOverviewErr(w io.Writer, e string) {
	if e != "" {
		fmt.Fprintf(w, "  error: %s\n", e)
	}
}

func shortHex(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// OverviewAge is the age of a unix-ms instant as AgeStr writes it; "-" for 0.
func OverviewAge(atMs uint64, now time.Time) string {
	if atMs == 0 {
		return "-"
	}
	n := now.UnixMilli()
	if int64(atMs) >= n {
		return AgeStr(0)
	}
	return AgeStr(uint64(n - int64(atMs)))
}
