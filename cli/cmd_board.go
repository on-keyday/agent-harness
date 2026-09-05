package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/cli/verb"
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

// RunBoardSubcmd handles the board sub-subcommands (topics, read, retract,
// purge).
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

// emitBoardMessageJSON writes one JSON-Lines record for a retained board
// message. It mirrors the `agent inbox --json` record shape where the fields
// overlap — seq, in_reply_to, topic, from{...}, payload_b64, and payload (the
// body embedded raw only when it is valid JSON) — so the two feeds parse the
// same way, and carries the operator-only columns board read's text row shows:
// reply_to_topic, received_at (epoch-ms + RFC3339), shown_to (delivery marks,
// derived from subs exactly as the text row is, so they cannot drift), and the
// retracted trio. payload_b64 always holds
// the exact bytes. A BoardMessage exposes its sender as a task hex string with
// no RunnerID, so from omits runner_id rather than emit an empty one; agent is
// left as the raw profile ("" = server could not attribute a runtime), not the
// "-" the text view substitutes.
func emitBoardMessageJSON(out io.Writer, topic string, m BoardMessage, subs []BoardSubscriberRow) {
	shownN, shownTotal := ShownTo(subs, topic, m.Seq)
	rec := map[string]any{
		"seq":            m.Seq,
		"in_reply_to":    m.InReplyTo,
		"topic":          topic,
		"reply_to_topic": m.ReplyToTopic,
		"shown_to":       map[string]any{"shown": shownN, "total": shownTotal},
		"received_at_ms": m.ReceivedAtMs,
		"received_at":    boardMsToRFC3339(m.ReceivedAtMs),
		"retracted":      m.Retracted,
		"from": map[string]any{
			"task_id":  m.FromTaskHex,
			"hostname": m.FromHostname,
			"agent":    m.FromAgentProfile,
		},
		"payload_b64": base64.StdEncoding.EncodeToString(m.Payload),
	}
	// retracted_at_ms is emitted only when the message is withdrawn: a 0 epoch
	// timestamp on every live line reads as "retracted at 1970", not "not
	// retracted". retracted (the bool) is always present for addressability.
	if m.Retracted {
		rec["retracted_at_ms"] = m.RetractedAtMs
		rec["retracted_by"] = RetractedByLabel(m)
	}
	if len(m.Payload) > 0 && json.Valid(m.Payload) {
		rec["payload"] = json.RawMessage(m.Payload)
	}
	line, _ := json.Marshal(rec)
	fmt.Fprintln(out, string(line))
}

// RunBoardSubcmd parses one `board <sub>` line from the declaration
// (cli/verb) and runs it. The sub-verb parameter is named sub rather than verb
// because the package it now parses through carries that name.
func RunBoardSubcmd(ctx context.Context, cid objproto.ConnectionID, sub string, rest []string, out io.Writer) error {
	sp, ok := verb.Lookup("board", sub)
	if !ok {
		return fmt.Errorf("board: unknown sub-verb %q", sub)
	}
	sp = sp.For(verb.CLI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bnd, perr := sp.Parse(fs, rest)
	if perr != nil {
		return perr
	}
	act, berr := sp.BuildFunc()(bnd)
	if berr != nil {
		return berr
	}
	return RunBoardAction(ctx, cid, act.(verb.BoardAction), out)
}

// RunBoardAction is RunBoardSubcmd for a caller that already has the parsed
// action -- the generated CLI dispatch, which parses from the declaration
// itself rather than handing this function a sub-verb string to look up.
func RunBoardAction(ctx context.Context, cid objproto.ConnectionID, ba verb.BoardAction, out io.Writer) error {
	switch ba.Sub {
	case verb.SubTopics:
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
		if ba.JSON {
			// One object per topic, same fields the text form shows. subs is
			// omitted rather than zeroed when the count is unavailable: a
			// caller without board_observe would otherwise read "0
			// subscribers" as a measurement.
			enc := json.NewEncoder(out)
			for _, n := range names {
				r := byName[n]
				rec := map[string]any{
					"topic": r.name, "msgs": r.msgs, "published": r.published,
					"retracted": r.retracted, "last_seq": r.lastSeq,
					"last": boardMsToRFC3339(r.lastMs),
				}
				if subsOK {
					rec["subs"] = subs[n]
				}
				if err := enc.Encode(rec); err != nil {
					return err
				}
			}
			return nil
		}
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

	case verb.SubRead:
		inReplyTo, asJSON := &ba.InReplyTo, &ba.JSON
		topic := ba.Topic
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
			if *asJSON {
				emitBoardMessageJSON(out, topic, m, subs)
				continue
			}
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
				// by= is part of the marker, not an extra: with two verbs able
				// to withdraw a message, "RETRACTED" alone would read as the
				// author having done it whoever actually did.
				retracted = fmt.Sprintf(" RETRACTED at=%s by=%s",
					boardMsToRFC3339(m.RetractedAtMs), RetractedByLabel(m))
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

	case verb.SubSubscribers:
		// Optional <topic>: with it, only the tasks a publish to that topic
		// would reach; without it, every task known to the board.
		topic := ba.Topic
		rows, err := BoardSubscribers(ctx, cid, topic)
		if err != nil {
			return err
		}
		if ba.JSON {
			// The patterns stay STRUCTURED here rather than collapsing into
			// the text form's `name(shown=N pending=M)`, which a consumer
			// would have to parse back out.
			enc := json.NewEncoder(out)
			for _, r := range rows {
				pats := make([]map[string]any, 0, len(r.Patterns))
				for _, p := range r.Patterns {
					pats = append(pats, map[string]any{
						"topic": p.Name, "shown": p.Shown, "pending": p.Pending,
					})
				}
				if err := enc.Encode(map[string]any{
					"task_id": r.TaskHex, "host": r.Hostname,
					"agent": r.AgentProfile, "topics": pats,
				}); err != nil {
					return err
				}
			}
			return nil
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

	case verb.SubRetract:
		// --seq required and non-zero is enforced in the verb's Build:
		// deliberately NOT purge's "0 means the whole topic", because
		// withdrawing a topic-full of other agents' messages on a mistyped flag
		// is exactly the accident this verb should not be able to have.
		topic, seq := ba.Topic, &ba.Seq
		ok, err := BoardRetract(ctx, cid, topic, *seq)
		if err != nil {
			return err
		}
		status := "ok"
		if !ok {
			// One answer for: no such topic, no such seq, and a seq already
			// withdrawn. The server does not tell them apart on purpose.
			status = "not_found"
		}
		fmt.Fprintf(out, "{\"status\":%q,\"topic\":%q,\"seq\":%d}\n", status, topic, *seq)

	case verb.SubPurge:
		// Permuted, and load-bearing rather than convenient: with stdlib
		// parsing `board purge <topic> --seq N` -- the form the usage line
		// prints -- left --seq unread and fell through to seq 0, which is the
		// WHOLE TOPIC. Measured on a live board 2026-08-28: two messages, one
		// named, both destroyed. The declaration marks --seq WidensIfUnset and
		// an invariant test refuses to let this verb stop permuting.
		topic, seq := ba.Topic, &ba.Seq
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
		return fmt.Errorf("unknown board subcommand: %q", ba.Sub)
	}
	return nil
}
