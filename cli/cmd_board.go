package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
)

// boardMsToRFC3339 converts a Unix millisecond timestamp to an RFC3339 string.
func boardMsToRFC3339(ms uint64) string {
	return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
}

// RunBoardSubcmd handles the board sub-subcommands (topics, read, purge).
// verb is the first arg after "board"; rest is the remaining args.
// All output (including purge JSON) is written to out.
// The caller is responsible for routing unknown verbs and printing board usage.
func RunBoardSubcmd(ctx context.Context, cid objproto.ConnectionID, verb string, rest []string, out io.Writer) error {
	switch verb {
	case "topics":
		rows, err := BoardTopics(ctx, cid)
		if err != nil {
			return err
		}
		for _, r := range rows {
			fmt.Fprintf(out, "%s  msgs=%d  last_seq=%d  last=%s\n",
				r.Name, r.MsgCount, r.LastSeq, boardMsToRFC3339(r.LastPublishedAtMs))
		}

	case "read":
		if len(rest) == 0 {
			return fmt.Errorf("board read: missing <topic>")
		}
		topic := rest[0]
		msgs, found, err := BoardRead(ctx, cid, topic)
		if err != nil {
			return err
		}
		if !found {
			// Topic does not exist — print nothing, exit 0 per spec.
			return nil
		}
		for _, m := range msgs {
			fmt.Fprintf(out, "#%d from=%s host=%s size=%d at=%s\n",
				m.Seq, m.FromTaskHex, m.FromHostname, len(m.Payload),
				boardMsToRFC3339(m.ReceivedAtMs))
			if json.Valid(m.Payload) {
				var buf bytes.Buffer
				_ = json.Indent(&buf, m.Payload, "", "  ")
				fmt.Fprintln(out, buf.String())
			} else {
				out.Write(m.Payload) //nolint:errcheck
				fmt.Fprintln(out)
			}
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
