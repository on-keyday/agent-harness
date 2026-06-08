package cli

import (
	"context"
	"fmt"
	"os"
	"unicode/utf8"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// notifyWireBudget is a conservative cap on the encoded TaskControlRequest
// carrying a NotifyRequest. RoundTripTaskControl sends it as a single objproto
// message, which on UDP transport is path-MTU-bound (trsf.DefaultInitialMTU =
// 1200). An oversize message does not arrive on UDP, so the bound is enforced
// here, client-side, before send. 1000 leaves headroom for objproto/trsf framing.
const notifyWireBudget = 1000

// parseNotifyLevel maps the CLI --level string to a NotifyLevel; empty → info.
func parseNotifyLevel(s string) (protocol.NotifyLevel, error) {
	switch s {
	case "", "info":
		return protocol.NotifyLevel_Info, nil
	case "warn":
		return protocol.NotifyLevel_Warn, nil
	case "error":
		return protocol.NotifyLevel_Error, nil
	default:
		return 0, fmt.Errorf("invalid --level %q (want info|warn|error)", s)
	}
}

// newNotifyRequestFromEnv builds a NotifyRequest, deriving origin from the
// HARNESS_* env set by the runner inside a worker. HARNESS_TASK_ID present →
// origin=worker with the WorkerInfo block; absent → origin=external.
func newNotifyRequestFromEnv(level protocol.NotifyLevel, title, text string) *protocol.NotifyRequest {
	nr := &protocol.NotifyRequest{Level: level}
	if taskID := os.Getenv("HARNESS_TASK_ID"); taskID != "" {
		nr.Origin = protocol.NotifyOrigin_Worker
		runnerID := os.Getenv("HARNESS_RUNNER_ID")
		repo := os.Getenv("HARNESS_REPO_PATH")
		host := os.Getenv("HARNESS_HOSTNAME")
		nr.SetWorker(protocol.WorkerInfo{
			TaskIdLen:   uint16(len(taskID)),
			TaskId:      []byte(taskID),
			RunnerIdLen: uint16(len(runnerID)),
			RunnerId:    []byte(runnerID),
			RepoLen:     uint16(len(repo)),
			Repo:        []byte(repo),
			HostnameLen: uint16(len(host)),
			Hostname:    []byte(host),
		})
	} else {
		nr.Origin = protocol.NotifyOrigin_External
	}
	nr.TitleLen = uint16(len(title))
	nr.Title = []byte(title)
	nr.TextLen = uint16(len(text))
	nr.Text = []byte(text)
	return nr
}

// encodedNotifyWireLen measures the wire size of the TaskControlRequest that
// would carry nr (AppKind byte + kind + request_id + NotifyRequest body).
func encodedNotifyWireLen(nr *protocol.NotifyRequest) int {
	tcr := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Notify}
	tcr.SetNotify(*nr)
	return len(tcr.MustAppend([]byte{byte(appwire.AppKind_TaskControl)}))
}

// mtuGuardNotify truncates nr.Text (rune-safe, with a trailing ellipsis) so the
// encoded message fits notifyWireBudget, and warns on stderr. No-op if it fits.
func mtuGuardNotify(nr *protocol.NotifyRequest) {
	if encodedNotifyWireLen(nr) <= notifyWireBudget {
		return
	}
	original := string(nr.Text)
	nr.Text = nil
	nr.TextLen = 0
	overhead := encodedNotifyWireLen(nr)
	const ell = "…"
	maxText := notifyWireBudget - overhead - len(ell)
	if maxText < 0 {
		maxText = 0
	}
	trimmed := truncateRunes(original, maxText) + ell
	nr.Text = []byte(trimmed)
	nr.TextLen = uint16(len(nr.Text))
	fmt.Fprintf(os.Stderr, "notify: text truncated %d→%d bytes to fit transport MTU\n", len(original), len(nr.Text))
}

// truncateRunes returns the longest prefix of s that is <= maxBytes and ends on
// a UTF-8 rune boundary.
func truncateRunes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	b := []byte(s)
	n := maxBytes
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	return string(b[:n])
}

// Notify sends a notification over an existing *Client. Long-lived consumers
// (TUI/WebUI) call this on their persistent client. level is "info|warn|error".
func (c *Client) Notify(ctx context.Context, level, title, text string) error {
	lvl, err := parseNotifyLevel(level)
	if err != nil {
		return err
	}
	nr := newNotifyRequestFromEnv(lvl, title, text)
	mtuGuardNotify(nr)

	tcr := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Notify}
	tcr.SetNotify(*nr)
	resp, err := c.RoundTripTaskControl(ctx, tcr)
	if err != nil {
		return err
	}
	if resp.Kind != protocol.TaskControlKind_Notify {
		return fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	out := resp.Notify()
	if out == nil {
		return fmt.Errorf("nil notify response")
	}
	switch out.Status {
	case protocol.NotifyStatus_NoHook:
		fmt.Fprintln(os.Stderr, "notify: server has no --notify-hook configured (recorded live-only, no external delivery)")
	case protocol.NotifyStatus_SpawnFailed:
		return fmt.Errorf("notify: server failed to spawn the configured hook")
	}
	return nil
}

// Notify (package-level) opens a fresh Client per call — for short-lived
// harness-cli. Long-lived consumers should hold a *Client and call the method.
func Notify(ctx context.Context, peerCID objproto.ConnectionID, level, title, text string) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Notify(ctx, level, title, text)
}
