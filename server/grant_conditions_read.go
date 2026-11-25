package server

import (
	"fmt"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Grant conditions arrive on a client-created send stream that the five
// grant-writing requests name (grant conditions spec C6): the open (in
// OpenInteractiveExt), credential issue and rekey, set_caps and a schedule's
// spec. A body never travels inline, so no datagram carries one.

// grantConditionsMax bounds a body read: MaxGrantConditions conditions of the
// largest kind (an action_set of MaxConditionActions ids) is about 4.2 KiB.
const grantConditionsMax = 8 << 10

// BadGrantConditionsError is every refusal of a request's grant conditions:
// the stream could not be read, the body does not decode or is over a limit,
// it names a mask that is not a conditioned override of the request, a
// conditioned override has no body, a kind is on a bit it does not apply to,
// or the request carries a body for a scope it does not write. Each handler
// answers it with its own bad_grant_conditions status, and nothing is written.
type BadGrantConditionsError struct{ Err error }

func (e *BadGrantConditionsError) Error() string { return "grant conditions: " + e.Err.Error() }
func (e *BadGrantConditionsError) Unwrap() error { return e.Err }

// readGrantConditions reads the body a request named on streamID, bounded and
// cancelled past the bound as readExecBody's is. 0 is "none": the empty body,
// which conditionsOnto still checks against the request's markers.
func readGrantConditions(conn ConnHandle, streamID uint64) (protocol.GrantConditions, error) {
	if streamID == 0 {
		return protocol.GrantConditions{}, nil
	}
	raw, err := readAgentPayloadStream(conn, streamID, grantConditionsMax)
	if err != nil {
		return protocol.GrantConditions{}, &BadGrantConditionsError{Err: fmt.Errorf("the body stream could not be read: %w", err)}
	}
	var body protocol.GrantConditions
	if err := body.DecodeExact(raw); err != nil {
		return protocol.GrantConditions{}, &BadGrantConditionsError{Err: fmt.Errorf("the body does not decode: %w", err)}
	}
	return body, nil
}

// withGrantConditions runs then with the body the request named on streamID.
// With no stream it runs at once with the empty body. Otherwise the stream is
// read off the receive loop — its frames are delivered by the loop this runs
// on, as an open's prompt is — so the caller passes a request it owns, not the
// borrowed frame. A body that cannot be read is answered through refuse, and
// then does not run.
func withGrantConditions(conn ConnHandle, streamID uint64, refuse func(error), then func(protocol.GrantConditions)) {
	if streamID == 0 {
		then(protocol.GrantConditions{})
		return
	}
	go func() {
		body, err := readGrantConditions(conn, streamID)
		if err != nil {
			refuse(err)
			return
		}
		then(body)
	}()
}

// conditionsOnto attaches body to s, the scope a request converted with
// ScopeFromWire. written says whether the request writes that scope: a body
// for a scope it does not write is refused (it would condition nothing), and
// a written scope is checked whole by ConditionsFromWire, so a marker left
// without a body is refused even when no stream was named.
func conditionsOnto(s *authority.Scope, body protocol.GrantConditions, written bool) error {
	if !written {
		if len(body.Conditions) != 0 {
			return &BadGrantConditionsError{Err: fmt.Errorf("the request carries grant conditions but does not write a scope (add --scope / --scope-for)")}
		}
		return nil
	}
	if err := authority.ConditionsFromWire(body, s); err != nil {
		return &BadGrantConditionsError{Err: err}
	}
	return nil
}

// ownCopy re-decodes a borrowed request frame into a value that owns its
// bytes, for a handler that finishes off the receive loop.
func ownCopy[T any, P interface {
	*T
	MustAppend([]byte) []byte
	DecodeExact([]byte) error
}](req P) (P, error) {
	var out T
	p := P(&out)
	if err := p.DecodeExact(req.MustAppend(nil)); err != nil {
		return nil, fmt.Errorf("request copy: %w", err)
	}
	return p, nil
}

// discardStream cancels one named body stream, off the receive loop; 0 is none.
func discardStream(conn ConnHandle, id uint64) {
	if id != 0 {
		go discardPayloadStream(conn, id)
	}
}
