package runner

import (
	"errors"
	"testing"
)

// The registry outlives the connection, which is the point — and it was also
// the hole. setSender installed a sender and nothing ever removed one, so
// after a disconnect the registry kept handing tasks a sender pointing at a
// socket nobody reads. A task finishing on the way out (cancelTasksUnlessHeld
// causes exactly that) wrote its TaskFinished into it and was told nothing had
// gone wrong.
//
// "Told nothing had gone wrong" is not a figure of speech: peerSender.Send is
// objproto SendMessage, and over UDP a datagram to a peer whose death has not
// been noticed yet returns no error at all — measured at ~68s to notice. An
// error-driven retry on this path would therefore never fire. The signal has
// to be the connection ending, not the send failing, which is what
// retireSender/clearSender supply.
func TestRetiredSenderReportsInsteadOfWritingIntoADeadConnection(t *testing.T) {
	reg := NewTaskRegistry()
	dead := &mockSender{}
	reg.setSender(dead)

	s := &Session{reg: reg, Sender: dead}
	s.retireSender()

	err := reg.sendWhenConnected([]byte("finished"), dead)
	if !errors.Is(err, ErrNoConnection) {
		t.Errorf("err = %v, want ErrNoConnection", err)
	}
	if n := len(dead.sent); n != 0 {
		t.Errorf("wrote %d message(s) into the retired connection; the whole "+
			"failure mode is that such a write looks like a success", n)
	}
}

// The fallback is what a single-shot Run and every bare-Session test rely on,
// and it is only correct while "no sender" means "none was ever installed".
// Conflating that with "the one we had is gone" would silently stop delivering
// for those callers.
func TestRegistryThatNeverHadASenderStillUsesTheFallback(t *testing.T) {
	reg := NewTaskRegistry()
	fallback := &mockSender{}

	if err := reg.sendWhenConnected([]byte("finished"), fallback); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if n := len(fallback.sent); n != 1 {
		t.Errorf("fallback got %d message(s), want 1", n)
	}
}

// A reconnect builds its Session — and installs its sender — while the
// previous connection is still unwinding, so the two orderings race. An
// unconditional clear retires the LIVE connection whenever the dead one's
// teardown runs second, which turns a routine reconnect into a runner that
// can no longer report anything.
func TestALateTeardownDoesNotRetireTheReconnectsSender(t *testing.T) {
	reg := NewTaskRegistry()
	old, fresh := &mockSender{}, &mockSender{}

	reg.setSender(old)
	reg.setSender(fresh) // the reconnect wins the race
	(&Session{reg: reg, Sender: old}).retireSender()

	if err := reg.sendWhenConnected([]byte("finished"), nil); err != nil {
		t.Fatalf("err = %v, want nil — the live connection was retired by the dead one", err)
	}
	if len(fresh.sent) != 1 || len(old.sent) != 0 {
		t.Errorf("delivered to fresh=%d old=%d, want 1 and 0", len(fresh.sent), len(old.sent))
	}
}

// Retiring is not permanent: the next connection's setSender must make the
// registry deliverable again, or a runner would report exactly one
// disconnect's worth of tasks and then go quiet for the rest of its life.
func TestTheNextConnectionMakesTheRegistryDeliverableAgain(t *testing.T) {
	reg := NewTaskRegistry()
	dead, next := &mockSender{}, &mockSender{}

	reg.setSender(dead)
	(&Session{reg: reg, Sender: dead}).retireSender()
	reg.setSender(next)

	if err := reg.sendWhenConnected([]byte("finished"), nil); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(next.sent) != 1 {
		t.Errorf("the new connection got %d message(s), want 1", len(next.sent))
	}
}
