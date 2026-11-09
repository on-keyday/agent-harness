package protocol

import "testing"

func limitN(n uint32) SpawnLimit {
	var l SpawnLimit
	l.SetLimited(true)
	l.SetValue(n)
	return l
}

func sameLimit(a, b SpawnLimit) bool { return a.Limited() == b.Limited() && a.Value() == b.Value() }

// The two spawn limits ride on every request that can write them and every
// response that reports them; the interactive request carries them BEFORE its
// conditional x11 block.
func TestSpawnLimitWireRoundTrip(t *testing.T) {
	depth, max := limitN(2), limitN(7)

	t.Run("SubmitRequest", func(t *testing.T) {
		var in SubmitRequest
		in.SpawnDepth, in.MaxDescendants = depth, max
		in.SetSpawnDepthPresent(true)
		in.SetMaxDescendantsPresent(true)
		var got SubmitRequest
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if !sameLimit(got.SpawnDepth, depth) || !sameLimit(got.MaxDescendants, max) ||
			!got.SpawnDepthPresent() || !got.MaxDescendantsPresent() {
			t.Errorf("round trip lost the limits: %+v", got)
		}
	})
	t.Run("OpenInteractiveRequest", func(t *testing.T) {
		var in OpenInteractiveRequest
		in.SpawnDepth, in.MaxDescendants = depth, max
		in.SetSpawnDepthPresent(true)
		in.SetMaxDescendantsPresent(true)
		in.SetX11Enabled(true)
		x11 := X11Forward{Display: 10}
		x11.SetCookie([]uint8{1, 2, 3})
		in.SetX11(x11)
		var got OpenInteractiveRequest
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if !sameLimit(got.SpawnDepth, depth) || !sameLimit(got.MaxDescendants, max) ||
			!got.SpawnDepthPresent() || !got.MaxDescendantsPresent() || got.X11() == nil {
			t.Errorf("round trip lost the limits or the x11 block: %+v", got)
		}
	})
	t.Run("SetCapsRequest", func(t *testing.T) {
		var in SetCapsRequest
		in.SpawnDepth, in.MaxDescendants = depth, max
		in.SetSpawnDepthPresent(true)
		in.SetMaxDescendantsPresent(true)
		var got SetCapsRequest
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if !sameLimit(got.SpawnDepth, depth) || !sameLimit(got.MaxDescendants, max) ||
			!got.SpawnDepthPresent() || !got.MaxDescendantsPresent() {
			t.Errorf("round trip lost the limits: %+v", got)
		}
	})
	t.Run("TaskInfo", func(t *testing.T) {
		var in TaskInfo
		in.SpawnDepth, in.MaxDescendants, in.Descendants = depth, max, 5
		var got TaskInfo
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if !sameLimit(got.SpawnDepth, depth) || !sameLimit(got.MaxDescendants, max) || got.Descendants != 5 {
			t.Errorf("round trip lost the limits: %+v", got)
		}
	})
	t.Run("WhoAmIResponse", func(t *testing.T) {
		var in WhoAmIResponse
		in.SpawnDepth, in.MaxDescendants, in.Descendants, in.SpawnRoom = depth, max, 5, limitN(2)
		var got WhoAmIResponse
		if err := got.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if !sameLimit(got.SpawnDepth, depth) || !sameLimit(got.MaxDescendants, max) ||
			got.Descendants != 5 || !sameLimit(got.SpawnRoom, limitN(2)) {
			t.Errorf("round trip lost the limits: %+v", got)
		}
	})
	t.Run("statuses", func(t *testing.T) {
		if SubmitStatus_SpawnLimit.String() != "spawn_limit" || OpenInteractiveStatus_SpawnLimit.String() != "spawn_limit" {
			t.Error("spawn_limit status missing")
		}
	})
}
