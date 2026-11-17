package cli

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// OverviewText makes s one terminal line: control bytes escaped as
// EscapeForTerminal writes them, and the line break and tab it leaves alone
// escaped too. Topic names, hostnames, forward specs and error strings come
// from agents, runners and peers -- the server accepts a topic name holding
// ESC -- so every line the overview prints goes through this. Text already
// escaped (a board head) passes unchanged: an escape is printable.
func OverviewText(s string) string {
	return strings.NewReplacer("\n", `\x0a`, "\t", `\x09`).Replace(EscapeForTerminal([]byte(s)))
}

// overviewLine prints one formatted line through OverviewText.
func overviewLine(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, OverviewText(fmt.Sprintf(format, args...)))
}

// WriteOverview prints o as `harness-cli overview` shows it: the five sections
// in the spec's order, each header carrying its count (zeros included) and,
// when that section's source failed, the error on the next line.
func WriteOverview(w io.Writer, o Overview, now time.Time) {
	overviewLine(w, "needs you: %d", o.NeedsYouCount)
	writeOverviewTasks(w, o.NeedsYou)

	g := o.GroupCounts
	overviewLine(w, "tasks: %d ready, %d working, %d no report, %d queued, %d finished",
		g[groupReady], g[groupWorking], g[groupNoReport], g[groupQueued], g[groupFinished])
	writeOverviewTasks(w, o.Live)

	overviewLine(w, "hosts: %d (%d slots, %d operator clients)", o.HostCount, o.SlotCount, o.OperatorClients)
	writeOverviewErr(w, o.HostsErr)
	for _, h := range o.Hosts {
		overviewLine(w, "  %-16s %-8s idle %d busy %d offline %d  tasks %d/%d",
			h.Hostname, h.Goos, h.Idle, h.Busy, h.Offline, h.ActiveTasks, h.MaxTasks)
	}

	overviewLine(w, "forwards: %d", o.ForwardCount)
	writeOverviewErr(w, o.ForwardsErr)
	for _, f := range o.Forwards {
		overviewLine(w, "  %-3s %-36s %-12s %s", f.Dir, f.Spec, f.Repo, shortHex(f.TaskID))
	}

	overviewLine(w, "board: %d topics, %d messages", o.TopicCount, o.MsgTotal)
	writeOverviewErr(w, o.BoardErr)
	for _, t := range o.Topics {
		overviewLine(w, "  %-24s %d msgs  %-8s %s", t.Name, t.MsgCount, t.Age, t.Head)
	}
}

func writeOverviewTasks(w io.Writer, ts []OverviewTask) {
	for _, t := range ts {
		overviewLine(w, "  %-10s %-16s %s  %s", t.Badge, t.Repo, shortHex(t.ID), t.Now)
	}
}

func writeOverviewErr(w io.Writer, e string) {
	if e != "" {
		overviewLine(w, "  error: %s", e)
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
