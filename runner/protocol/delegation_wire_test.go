package protocol

import (
	"bytes"
	"testing"
)

// delegScope is a non-default bound: every TaskScope axis away from its zero.
func delegScope() TaskScope {
	s := TaskScope{Base: ScopeBase_None, VisBase: ScopeBase_Global}
	s.SetVisBasePresent(true)
	s.SetExcludeSelf(true)
	var id TaskID
	id.Id[0] = 0xab
	s.SetIds([]TaskID{id})
	return s
}

func sameScope(a, b TaskScope) bool {
	return bytes.Equal(a.MustAppend(nil), b.MustAppend(nil))
}

// The delegation bound rides on every request that can write it and every
// response that reports it, and the interactive request carries it BEFORE
// its conditional x11 / detached blocks.
func TestDelegationWireRoundTrip(t *testing.T) {
	t.Run("SubmitRequest", func(t *testing.T) {
		var in SubmitRequest
		in.DelegateCaps = Capability_FileRead
		in.DelegateScope = delegScope()
		in.SetDelegateCapsPresent(true)
		in.SetDelegateScopePresent(true)
		var got SubmitRequest
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if got.DelegateCaps != Capability_FileRead || !sameScope(got.DelegateScope, in.DelegateScope) ||
			!got.DelegateCapsPresent() || !got.DelegateScopePresent() {
			t.Errorf("round trip lost the bound: %+v", got)
		}
	})
	t.Run("OpenInteractiveRequest", func(t *testing.T) {
		var in OpenInteractiveRequest
		in.DelegateCaps = Capability_Cancel
		in.DelegateScope = delegScope()
		in.SetDelegateCapsPresent(true)
		in.SetDelegateScopePresent(true)
		in.SetX11Enabled(true)
		x11 := X11Forward{Display: 10}
		x11.SetCookie([]uint8{1, 2, 3})
		in.SetX11(x11)
		in.SetDetached(true)
		in.SetInitialRows(24)
		in.SetInitialCols(80)
		var got OpenInteractiveRequest
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if got.DelegateCaps != Capability_Cancel || !sameScope(got.DelegateScope, in.DelegateScope) ||
			!got.DelegateCapsPresent() || !got.DelegateScopePresent() ||
			got.X11() == nil || got.X11().Display != 10 || len(got.X11().Cookie) != 3 ||
			got.InitialRows() == nil || *got.InitialRows() != 24 || *got.InitialCols() != 80 {
			t.Errorf("round trip = %+v", got)
		}
	})
	t.Run("OpenInteractiveResponse", func(t *testing.T) {
		in := OpenInteractiveResponse{Status: OpenInteractiveStatus_AmbiguousRunner}
		in.SetAttached(true)
		in.SetStreamId(7)
		in.SetCandidatesStreamId(9)
		in.SetErrorMsg([]byte("outside what you may delegate"))
		var got OpenInteractiveResponse
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if string(got.ErrorMsg) != "outside what you may delegate" ||
			got.StreamId() == nil || *got.StreamId() != 7 || got.CandidatesStreamId() == nil || *got.CandidatesStreamId() != 9 {
			t.Errorf("round trip = %+v", got)
		}
	})
	t.Run("SetCapsRequest", func(t *testing.T) {
		var in SetCapsRequest
		in.DelegateCaps = Capability_FileRead
		in.DelegateScope = delegScope()
		in.SetDelegateCapsPresent(true)
		in.SetDelegateScopePresent(true)
		in.SetCascade(true)
		var got SetCapsRequest
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if got.DelegateCaps != Capability_FileRead || !sameScope(got.DelegateScope, in.DelegateScope) ||
			!got.DelegateCapsPresent() || !got.DelegateScopePresent() || !got.Cascade() {
			t.Errorf("round trip = %+v", got)
		}
	})
	t.Run("TaskInfo", func(t *testing.T) {
		var in TaskInfo
		in.DelegateCaps = Capability_FileRead
		in.DelegateScope = delegScope()
		var got TaskInfo
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if got.DelegateCaps != Capability_FileRead || !sameScope(got.DelegateScope, in.DelegateScope) {
			t.Errorf("round trip = %+v", got)
		}
	})
	t.Run("WhoAmIResponse", func(t *testing.T) {
		var in WhoAmIResponse
		in.DelegateCaps = Capability_All
		in.DelegateScope = TaskScope{Base: ScopeBase_Global}
		var got WhoAmIResponse
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if got.DelegateCaps != Capability_All || got.DelegateScope.Base != ScopeBase_Global {
			t.Errorf("round trip = %+v", got)
		}
	})
}
