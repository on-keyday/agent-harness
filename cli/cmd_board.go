package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
)

// boardMsToRFC3339 converts a Unix millisecond timestamp to an RFC3339 string.
func boardMsToRFC3339(ms uint64) string {
	return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
}

// boardAgentOrDash renders an unattributed sender profile as "-" so the column
// never collapses to an empty run of spaces.
func boardAgentOrDash(profile string) string {
	if profile == "" {
		return "-"
	}
	return profile
}

// RunBoardSubcmd handles the board sub-subcommands (topics, read, purge).
// verb is the first arg after "board"; rest is the remaining args.
// All output (including purge JSON) is written to out.
// The caller is responsible for routing unknown verbs and printing board usage.
// boardHostOrDash renders an empty hostname as "-". Empty is a real state: the
// task is registered on the board (its chat.<short-id> is seeded) but has not
// run a harness-cli command yet, which is what fills the hostname in.
func boardHostOrDash(h string) string {
	if h == "" {
		return "-"
	}
	return h
}

func RunBoardSubcmd(ctx context.Context, cid objproto.ConnectionID, verb string, rest []string, out io.Writer) error {
	switch verb {
	case "topics":
		rows, err := BoardTopics(ctx, cid)
		if err != nil {
			return err
		}
		// The listing is the UNION of topics that exist and patterns that are
		// subscribed. A topic only comes into existence when something is
		// published to it, so listing board topics alone hides the state most
		// worth seeing: subscribed, nothing published yet. One
		// BoardSubscribers("") call yields both the per-topic count and every
		// subscribed name, so this costs one extra round trip, not one per row.
		subs := map[string]int{}
		subsOK := true
		if srows, serr := BoardSubscribers(ctx, cid, ""); serr == nil {
			for _, sr := range srows {
				for _, pat := range sr.Patterns {
					subs[pat.Name]++
				}
			}
		} else {
			// Requires info_global; a caller without it still gets the topics.
			subsOK = false
			fmt.Fprintf(os.Stderr, "board topics: subscriber counts unavailable: %v\n", serr)
		}

		type topicRow struct {
			name      string
			msgs      int
			retracted int
			lastSeq   uint64
			lastMs    uint64
			published bool
		}
		byName := map[string]topicRow{}
		for _, r := range rows {
			byName[r.Name] = topicRow{r.Name, r.MsgCount, r.RetractedCount, r.LastSeq, r.LastPublishedAtMs, true}
		}
		for pat := range subs {
			if _, ok := byName[pat]; !ok {
				byName[pat] = topicRow{name: pat}
			}
		}
		names := make([]string, 0, len(byName))
		for n := range byName {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r := byName[n]
			subCol := ""
			if subsOK {
				subCol = fmt.Sprintf("  subs=%d", subs[n])
			}
			if !r.published {
				fmt.Fprintf(out, "%s  msgs=0%s  (nothing published yet)\n", r.name, subCol)
				continue
			}
			// retracted= appears only when the topic holds withdrawn messages.
			// It is NOT part of msgs=: msgs answers "how much would a
			// subscriber receive", so a topic whose ring emptied through
			// retraction must read msgs=0 while still showing what is there to
			// audit.
			retractedCol := ""
			if r.retracted > 0 {
				retractedCol = fmt.Sprintf("  retracted=%d", r.retracted)
			}
			fmt.Fprintf(out, "%s  msgs=%d%s%s  last_seq=%d  last=%s\n",
				r.name, r.msgs, retractedCol, subCol, r.lastSeq, boardMsToRFC3339(r.lastMs))
		}

	case "read":
		fs := flag.NewFlagSet("board read", flag.ContinueOnError)
		inReplyTo := fs.Uint64("in-reply-to", 0, "only show messages replying to this seq")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return fmt.Errorf("board read: missing <topic>")
		}
		topic := fs.Arg(0)
		msgs, found, err := BoardRead(ctx, cid, topic)
		if err != nil {
			return err
		}
		if !found {
			// Exit 0 per spec, but say which nothing this is: "no such topic"
			// and "topic with an empty ring" both print nothing on stdout, and
			// the exit code does not separate them either. Diagnostics go to
			// stderr so `board read T | ...` stays clean — the same split
			// cli/notify.go and cli/attach_native.go use.
			fmt.Fprintf(os.Stderr, "board read: topic %q is not on the board (never published, or evicted / purged)\n", topic)
			return nil
		}
		if len(msgs) == 0 {
			fmt.Fprintf(os.Stderr, "board read: topic %q is on the board but holds no messages\n", topic)
			return nil
		}
		// Who has actually been handed each of these. The subscriber rows carry
		// one watermark per (task, topic); ShownTo turns that into a per-message
		// answer, which is the question an operator reading a topic has. A
		// failure here is not fatal to the listing: the rows still print, with
		// shown_to=0/0, and the reason goes to stderr.
		subs, serr := BoardSubscribers(ctx, cid, topic)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "board read: delivery marks unavailable: %v\n", serr)
		}
		shown := 0
		for _, m := range msgs {
			if *inReplyTo != 0 && m.InReplyTo != *inReplyTo {
				continue
			}
			shown++
			// re= is printed only on replies: on a board where nothing replies
			// yet, a re=0 on every line is noise.
			re := ""
			if m.InReplyTo != 0 {
				re = fmt.Sprintf(" re=%d", m.InReplyTo)
			}
			// Same rule as re= above: the marker prints only when it applies.
			// A retracted= on every live line would be noise, and this row is
			// the only place a withdrawn message is visible at all — the agent
			// paths dropped it the moment its author called retract.
			retracted := ""
			if m.Retracted {
				retracted = fmt.Sprintf(" RETRACTED at=%s", boardMsToRFC3339(m.RetractedAtMs))
			}
			// Same rule again: only senders that declared a destination get
			// this. It answers "where did the answer to #N go", which nothing
			// else on the board can -- the routing is resolved server-side off
			// this row and appears in no message's text.
			replyTo := ""
			if m.ReplyToTopic != "" {
				replyTo = fmt.Sprintf(" reply-to=%s", m.ReplyToTopic)
			}
			fmt.Fprintf(out, "#%d%s%s from=%s host=%s agent=%s size=%d at=%s %s%s\n",
				m.Seq, re, replyTo, m.FromTaskHex, m.FromHostname, boardAgentOrDash(m.FromAgentProfile),
				len(m.Payload), boardMsToRFC3339(m.ReceivedAtMs),
				ShownToLabel(subs, topic, m.Seq), retracted)
			if json.Valid(m.Payload) {
				var buf bytes.Buffer
				_ = json.Indent(&buf, m.Payload, "", "  ")
				fmt.Fprintln(out, buf.String())
			} else {
				out.Write(m.Payload) //nolint:errcheck
				fmt.Fprintln(out)
			}
		}
		if shown == 0 {
			// Third way to print nothing: the topic has messages, none of them
			// a reply to the requested seq.
			fmt.Fprintf(os.Stderr, "board read: topic %q holds %d message(s), none replying to %d\n",
				topic, len(msgs), *inReplyTo)
		}

	case "subscribers":
		// Optional <topic>: with it, only the tasks a publish to that topic
		// would reach; without it, every task known to the board.
		topic := ""
		if len(rest) > 0 {
			topic = rest[0]
		}
		rows, err := BoardSubscribers(ctx, cid, topic)
		if err != nil {
			return err
		}
		for _, r := range rows {
			// shown / pending per topic: how far the automatic injection path
			// has reached for this task, and how much sits above it. This is
			// the answer to "did the agent actually get my message" that used
			// to require guessing at a cursor file on the runner host.
			pats := "-"
			if len(r.Patterns) > 0 {
				parts := make([]string, 0, len(r.Patterns))
				for _, p := range r.Patterns {
					parts = append(parts, fmt.Sprintf("%s(shown=%d pending=%d)", p.Name, p.Shown, p.Pending))
				}
				pats = strings.Join(parts, ",")
			}
			fmt.Fprintf(out, "%s host=%s agent=%s topics=%s\n",
				r.TaskHex, boardHostOrDash(r.Hostname), boardAgentOrDash(r.AgentProfile), pats)
		}

	case "purge":
		fs := flag.NewFlagSet("board purge", flag.ContinueOnError)
		seq := fs.Uint64("seq", 0, "drop only the retained message with this seq (0 = whole topic)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pargs := fs.Args()
		if len(pargs) == 0 {
			return fmt.Errorf("board purge: missing <topic>")
		}
		topic := pargs[0]
		purged, found, err := BoardPurge(ctx, cid, topic, *seq)
		if err != nil {
			return err
		}
		status := "ok"
		if !found {
			status = "not_found"
		}
		fmt.Fprintf(out, "{\"status\":%q,\"topic\":%q,\"purged\":%d}\n", status, topic, purged)

	default:
		return fmt.Errorf("unknown board subcommand: %q", verb)
	}
	return nil
}
