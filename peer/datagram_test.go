package peer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/objtrsf/trsf"
)

// dgTransport is a trsf.Transport that only answers the datagram half. The
// embedded interface is nil, so anything else panics rather than quietly
// returning a zero — the same shape runner/trsf_state_test.go's fake uses, and
// it keeps this test about the pump rather than about trsf.
type dgTransport struct {
	trsf.Transport
	in       chan []byte
	sent     chan []byte
	maxSize  int
	sendErr  error
	uncontrl atomic.Uint64
}

func (d *dgTransport) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case b := <-d.in:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *dgTransport) SendDatagram(b []byte) error {
	if d.sendErr != nil {
		return d.sendErr
	}
	d.sent <- append([]byte(nil), b...)
	return nil
}

func (d *dgTransport) SendDatagramUncontrolled(b []byte) error {
	d.uncontrl.Add(1)
	return d.SendDatagram(b)
}

func (d *dgTransport) MaxDatagramSize() int { return d.maxSize }

func newDatagramConn(t *testing.T) (*Conn, *dgTransport, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tr := &dgTransport{in: make(chan []byte, 4), sent: make(chan []byte, 4), maxSize: 1169}
	c := &Conn{
		trans:     tr,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		done:      make(chan struct{}),
		dgDone:    make(chan struct{}),
		pubTopics: map[string]*pubTopic{},
	}
	go c.pumpDatagrams(ctx)
	return c, tr, ctx
}

// A received datagram reaches the handler with the appwire kind split off,
// exactly as a control message does. That symmetry is deliberate: a caller
// moving a payload between the two send paths changes which method it calls and
// nothing about how it builds or reads the bytes.
func TestDatagramPumpSplitsKindAndPayload(t *testing.T) {
	c, tr, _ := newDatagramConn(t)

	kinds := make(chan appwire.AppKind, 1)
	bodies := make(chan string, 1)
	c.SetOnDatagram(func(k appwire.AppKind, payload []byte) {
		kinds <- k
		bodies <- string(payload)
	})

	tr.in <- append([]byte{byte(appwire.AppKind_ForwardDatagram)}, []byte("body")...)

	select {
	case k := <-kinds:
		if k != appwire.AppKind_ForwardDatagram {
			t.Fatalf("kind = %v, want forward_datagram", k)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pump delivered nothing within 3s")
	}
	if b := <-bodies; b != "body" {
		t.Fatalf("payload = %q, want %q", b, "body")
	}
}

// An empty datagram is dropped rather than indexed into. It is a legal payload
// on the wire -- trsf carries a zero-length body -- so this is a live path, not
// a defensive check.
func TestDatagramPumpIgnoresEmptyPayloads(t *testing.T) {
	c, tr, _ := newDatagramConn(t)
	var calls atomic.Uint64
	c.SetOnDatagram(func(appwire.AppKind, []byte) { calls.Add(1) })

	tr.in <- []byte{}
	tr.in <- append([]byte{byte(appwire.AppKind_ForwardDatagram)}, []byte("real")...)

	deadline := time.After(3 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the pump never delivered the non-empty datagram")
		default:
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1: the empty datagram must not reach it", got)
	}
}

// With no handler registered a datagram is dropped, not buffered and not
// panicked on. The pump runs from Start, which callers reach before wiring.
func TestDatagramPumpWithoutHandlerDropsQuietly(t *testing.T) {
	_, tr, _ := newDatagramConn(t)
	tr.in <- []byte{byte(appwire.AppKind_ForwardDatagram), 'x'}
	// Nothing to assert beyond not panicking; give the pump a turn to run.
	time.Sleep(50 * time.Millisecond)
}

// The pump ends with its context, which is what the connection's death
// cancels. A pump that outlived it would hold a goroutine per dead connection.
func TestDatagramPumpStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := &dgTransport{in: make(chan []byte), sent: make(chan []byte, 1), maxSize: 1169}
	c := &Conn{trans: tr, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		done: make(chan struct{}), dgDone: make(chan struct{}), pubTopics: map[string]*pubTopic{}}
	stopped := make(chan struct{})
	go func() { c.pumpDatagrams(ctx); close(stopped) }()

	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the datagram pump outlived its context")
	}
}

// The three send-side accessors pass straight through, including the error a
// refusal comes back as -- the caller is what decides whether a drop matters,
// so nothing here may swallow it.
func TestDatagramSendAccessorsPassThrough(t *testing.T) {
	c, tr, _ := newDatagramConn(t)

	if got := c.MaxDatagramSize(); got != 1169 {
		t.Errorf("MaxDatagramSize() = %d, want 1169", got)
	}
	if err := c.SendDatagram([]byte("x")); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	if got := string(<-tr.sent); got != "x" {
		t.Errorf("sent %q, want %q", got, "x")
	}
	if err := c.SendDatagramUncontrolled([]byte("y")); err != nil {
		t.Fatalf("SendDatagramUncontrolled: %v", err)
	}
	<-tr.sent
	if got := tr.uncontrl.Load(); got != 1 {
		t.Errorf("uncontrolled sends = %d, want 1: the two modes must not collapse into one", got)
	}

	tr.sendErr = errors.New("window closed")
	if err := c.SendDatagram([]byte("z")); err == nil {
		t.Error("SendDatagram swallowed a refusal; the caller cannot count a drop it never hears about")
	}
}
