package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/on-keyday/agent-harness/cli"
)

// gridPane is one cell of the grid. A PTY session is a PaneStreamer (a VT
// screen); an event-stream session is a StreamPane (a transcript driven by the
// chat's keys). The grid tiles, focuses and routes keys without knowing which.
type gridPane interface {
	TaskID() string
	// Render returns at most height rows, none wider than width.
	Render(width, height int) string
	Err() error
	Stop()
	ScrollBy(delta int)
	ScrollHBy(delta int)
	ResetScroll()
	ScrollOffset() int
	ColOffset() int
	DiagLine() string
	// Key is one keystroke in the grid's input mode (everything but Ctrl+],
	// which the grid keeps to leave input mode).
	Key(msg tea.KeyMsg)
	// SetInputActive tells the pane whether the grid's input mode is on it.
	SetInputActive(on bool)
	// Badge is what the cell header shows after the task id.
	Badge() string
	// OpensChat reports that ⏎ / v on this pane open the chat overlay rather
	// than attaching a terminal: an event-stream session has no terminal.
	OpensChat() bool
}

// Key forwards a keystroke to the PTY as raw bytes.
func (p *PaneStreamer) Key(msg tea.KeyMsg) {
	if b := keyToBytes(msg); len(b) > 0 {
		p.SendInput(b)
	}
}

func (p *PaneStreamer) SetInputActive(bool) {}
func (p *PaneStreamer) Badge() string       { return "" }
func (p *PaneStreamer) OpensChat() bool     { return false }

// StreamPane is a grid cell over an event-stream session: the chat's transcript
// in a cell, driven in input mode by the chat's own keys (streamDriver), so an
// approval or a question is answered here exactly as it is in the chat.
//
// Its attach is a cowrite StreamSession with the same capped replay and
// reattach-with-backoff as a PaneStreamer's, for the same reasons: a grid pulls
// N replays over one client, and the server drops an observer that falls
// behind.
type StreamPane struct {
	taskID string

	// startDelay staggers the first attach; see PaneStreamer.startDelay.
	startDelay time.Duration

	mu      sync.Mutex
	d       streamDriver
	err     error
	cancel  context.CancelFunc
	stopped bool
	active  bool
	// Diagnostics for DiagLine.
	attaches int
	rxLines  int
}

func NewStreamPane(taskID string) *StreamPane {
	p := &StreamPane{taskID: taskID}
	p.d.reset()
	return p
}

func (p *StreamPane) TaskID() string { return p.taskID }

func (p *StreamPane) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *StreamPane) setErr(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

func (p *StreamPane) Start(ctx context.Context, c *cli.Client) {
	cctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()
	go p.pump(cctx, c)
}

// pump attaches and folds lines into the driver, reattaching with backoff when
// the stream ends without Stop. A reattach replays the ring again, so the
// transcript is rebuilt from it; the pending request is NOT cleared, because
// the replay re-delivers it and an answer half-built for it should survive
// (streamDriver.applyLine keeps it for the same request id).
func (p *StreamPane) pump(ctx context.Context, c *cli.Client) {
	if p.startDelay > 0 && !sleepCtx(ctx, p.startDelay) {
		return
	}
	backoff := paneReattachBase
	for {
		if ctx.Err() != nil {
			return
		}
		attachCtx, cancelAttach := context.WithTimeout(ctx, paneAttachTimeout)
		sess, err := c.OpenStreamSessionWithReplayLimit(attachCtx, p.taskID, gridReplayLimit)
		cancelAttach()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.setErr(err)
			if cli.IsAttachPermanent(err) || errors.Is(err, cli.ErrAttachWrongKind) {
				return
			}
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			_ = sess.Close()
			return
		}
		p.d.sess = sess
		p.d.lines = nil
		p.d.scroll = 0
		p.err = nil
		p.attaches++
		p.mu.Unlock()

		start := time.Now()
		go p.readStderr(sess)
		rerr := p.readLines(sess)
		_ = sess.Close()

		p.mu.Lock()
		p.d.sess = nil
		stopped := p.stopped
		p.mu.Unlock()
		if stopped || ctx.Err() != nil {
			return
		}
		p.setErr(rerr)
		if time.Since(start) > 5*time.Second {
			backoff = paneReattachBase
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func (p *StreamPane) readLines(sess *cli.StreamSession) error {
	for {
		line, err := sess.ReadLine()
		if line.Raw != nil || line.Decoded {
			p.mu.Lock()
			p.rxLines++
			p.d.applyLine(line)
			p.mu.Unlock()
		}
		if err != nil {
			return err
		}
	}
}

// readStderr folds the agent's stderr in, as the chat does; draining it is
// also what keeps it from backpressuring the stream.
func (p *StreamPane) readStderr(sess *cli.StreamSession) {
	for {
		line, err := sess.ReadStderrLine()
		if line.Raw != nil {
			p.mu.Lock()
			p.d.applyLine(line)
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *StreamPane) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	sess := p.d.sess
	p.cancel = nil
	p.d.sess = nil
	p.stopped = true
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if sess != nil {
		_ = sess.Close()
	}
}

// Key drives the session with the chat's keys. PgUp/PgDn scroll, as in the
// chat; the driver's returned command is dropped — the grid repaints on its
// own tick, and elapsed time is read off the clock.
func (p *StreamPane) Key(msg tea.KeyMsg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch msg.String() {
	case "pgup":
		p.d.scrollBy(chatPage)
		return
	case "pgdown":
		p.d.scrollBy(-chatPage)
		return
	}
	p.d.key(msg)
}

func (p *StreamPane) SetInputActive(on bool) {
	p.mu.Lock()
	p.active = on
	p.mu.Unlock()
}

func (p *StreamPane) Badge() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.d.pending != nil {
		return " ‹stream› ⚑"
	}
	return " ‹stream›"
}

func (p *StreamPane) OpensChat() bool { return true }

func (p *StreamPane) ScrollBy(delta int) {
	p.mu.Lock()
	p.d.scrollBy(delta)
	p.mu.Unlock()
}

// ScrollHBy is a no-op: transcript lines wrap, so nothing is cut off to the
// right.
func (p *StreamPane) ScrollHBy(int) {}

func (p *StreamPane) ResetScroll() {
	p.mu.Lock()
	p.d.scroll = 0
	p.mu.Unlock()
}

func (p *StreamPane) ScrollOffset() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.d.scroll
}

func (p *StreamPane) ColOffset() int { return 0 }

func (p *StreamPane) DiagLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	errS := "-"
	if p.err != nil {
		errS = p.err.Error()
	}
	return fmt.Sprintf("stream at=%d lines=%d attached=%t err=%s", p.attaches, p.rxLines, p.d.sess != nil, errS)
}

// Render lays the cell out bottom-up: the input row while the grid's input
// mode is on this pane (or while something typed is waiting in it), the status
// row when there is a status, and the transcript with the pending block above
// them. Every row is clipped to width: the input's prompt varies by sub-editor
// and the grid's box would wrap anything wider.
func (p *StreamPane) Render(width, height int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if width <= 0 || height <= 0 {
		return ""
	}
	p.d.width, p.d.gutter = width, 0

	var tail []string
	if st := p.d.statusText(); st != "" {
		tail = append(tail, MutedStyle.Render(runewidth.Truncate(st, width, "…")))
	}
	if p.active || p.d.input.Value() != "" || p.d.mode != chatModeNormal {
		in := p.d.input
		in.Width = p.d.inputWidth()
		if !p.active {
			in.Blur()
		}
		tail = append(tail, in.View())
	}
	body := height - len(tail)
	if body < 1 {
		body, tail = height, nil
	}
	empty := "(attaching…)"
	if p.d.sess != nil {
		empty = "(nothing on the stream yet)"
	}
	var out []string
	for _, r := range p.d.bodyRows(body, body*2/3, empty) {
		out = append(out, r.style.Render(r.text))
	}
	out = append(out, tail...)
	clip := lipgloss.NewStyle().MaxWidth(width)
	for i := range out {
		out[i] = clip.Render(out[i])
	}
	return strings.Join(out, "\n")
}

// GridOpenChatMsg asks the App to open the chat overlay on TaskID: ⏎ / v on an
// event-stream pane, which has no terminal to attach.
type GridOpenChatMsg struct{ TaskID string }
