package server

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// feedFrames queues each payload as its own frame and waits for the ring to
// stop growing, so a test can reason about append indexes.
func feedFrames(t *testing.T, mux *SessionMux, runner *fakeBidiStream, payloads ...string) {
	t.Helper()
	for _, p := range payloads {
		fr := makeWireFrame(1, []byte(p))
		want := mux.RingAppendCount() + 1
		runner.QueueRead(fr)
		waitFor(t, func() bool { return mux.RingAppendCount() >= want })
	}
}

// When the ring still holds the ESC[?1049h that opened a finished episode, the
// episode is self-contained: a client replaying it enters the alternate buffer,
// the episode paints THAT buffer, and it leaves again. Nothing reaches the
// primary screen or the scrollback, so the history from before the episode must
// be kept rather than trimmed away.
func TestReplayKeepsHistoryWhenTheEntrySurvives(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	feedFrames(t, mux,
		runner,
		"BEFORE-EPISODE",
		"\x1b[?1049h",
		"EPISODE-CONTENT",
		"\x1b[?1049l",
		"AFTER-EPISODE",
	)

	got := mux.replaySnapshot()
	if !bytes.Contains(got, []byte("BEFORE-EPISODE")) {
		t.Errorf("history from before the episode was trimmed even though its entry "+
			"is still in the ring; replay = %q", got)
	}
	if !bytes.Contains(got, []byte("AFTER-EPISODE")) {
		t.Errorf("replay lost what came after the episode: %q", got)
	}
}

// When the entry has been evicted the ring OPENS inside the episode, so its
// first frames are absolute-cursor fragments with no ESC[?1049h ahead of them.
// Replayed as they are they paint the PRIMARY screen and scroll into the
// scrollback, which the repaint cannot reach. That case still trims.
func TestReplayTrimsWhenTheRingStartsInsideAnEpisode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	// Big enough for the last few frames, small enough to evict the entry.
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(48), SessionHooks{})

	feedFrames(t, mux,
		runner,
		"\x1b[?1049h",
		"EPISODE-FRAGMENT-AAAA",
		"EPISODE-FRAGMENT-BBBB",
		"\x1b[?1049l",
		"AFTER",
	)

	if got := mux.replaySnapshot(); bytes.Contains(got, []byte("EPISODE-FRAGMENT")) {
		t.Errorf("a ring that opens inside a finished episode replayed its fragments "+
			"onto the primary screen; replay = %q", got)
	}
}

// The predicate has to be about the ring's OLDEST frame, not about the most
// recent entry. Two episodes, with the ring opening inside the FIRST: the
// second episode's entry is still present, so "is the most recent entry in the
// ring" answers yes and trims nothing — and the first episode's fragments, which
// are the ones actually straddling the start, paint the primary screen.
func TestReplayTrimsWhenAnEarlierEpisodeStraddlesTheStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(128), SessionHooks{})

	feedFrames(t, mux,
		runner,
		"\x1b[?1049h",
		"E1-FRAGMENT-AAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"E1-FRAGMENT-BBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		"\x1b[?1049l",
		"\x1b[?1049h",
		"E2-CONTENT",
		"\x1b[?1049l",
	)

	// The naive predicate's input: E2's entry IS still in the window. Anything
	// asking "is the most recent alternate-screen entry still in the ring" would
	// answer yes here and decline to trim.
	whole := mux.ring.Snapshot()
	if !bytes.Contains(whole, []byte("E2-CONTENT")) {
		t.Fatalf("fixture broken: E2 is not in the ring at all, so this test is not "+
			"about competing predicates; ring = %q", whole)
	}
	if !bytes.Contains(whole, []byte("\x1b[?1049h")) {
		t.Fatalf("fixture broken: no alternate-screen entry survives in the ring, so the "+
			"naive predicate would agree with the sound one; ring = %q", whole)
	}

	// And the sound predicate disagrees with it, because the ring OPENS inside
	// E1 — which is the episode whose fragments would paint the primary screen.
	if !mux.altAtOldestSurviving() {
		t.Fatalf("fixture broken: the ring does not start inside an episode, so the two "+
			"predicates cannot differ here; ring = %q", whole)
	}
	if got := mux.replaySnapshot(); bytes.Contains(got, []byte("E1-FRAGMENT")) {
		t.Errorf("the straddling episode's fragments survived the trim; replay = %q", got)
	}
}
