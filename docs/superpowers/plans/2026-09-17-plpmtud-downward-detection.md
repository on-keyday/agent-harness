# PLPMTUD Downward Detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `trsf`'s MTU estimate able to fall when a path stops carrying a size it had already proven, so a connection whose path narrows mid-life degrades visibly instead of answering small calls and hanging on large ones.

**Architecture:** `MTUTracker` gains one downward transition fed by two detectors — non-probe packet loss (works while traffic flows) and a validation probe at the current size (works while idle) — plus a wake deadline so the machine runs at all on an idle connection. `sendStream`'s retransmit branch gains the ability to re-split a chunk captured at a larger budget, without which lowering the estimate changes nothing for data already in flight.

**Tech Stack:** Go. Two repositories: `github.com/on-keyday/objtrsf` (checkout at `/home/kforfk/workspace/objtrsf`) for Tasks 1–5, this harness repo for Tasks 6–7. `scripts/netem-lab` for the end-to-end arms.

**Spec:** `docs/superpowers/specs/2026-09-17-plpmtud-downward-detection-design.md`

## Global Constraints

- Tasks 1–5 are entirely inside `objtrsf` and are tested by `go test ./trsf/...` in that checkout. **Do not add a `replace` directive to the harness `go.mod`.** The harness does not need to build against the new code until Task 6.
- **`t.mtu` must be assigned in exactly two places when this is done:** `OnACK`'s existing increase, and the single named fallback method of Task 1. A grep for `t.mtu =` is the check. Do not add a third.
- **Line numbers in this plan are against `objtrsf` `cc8d0928a19e` and harness `552ba933`.** Re-locate by content if they have moved.
- `trsf/mtu` must not import `trsf/congestion`. The RTT is supplied as a `func() time.Duration`.
- Existing `TrsfCounterKey` ordinals are never renumbered. New members append.
- Comments in this codebase explain *why*, at the density of the surrounding file. Match it; do not add a comment per line.

---

### Task 1: `MTUTracker` — separated loss counters and the fallback transition

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/mtu/plpmtud.go`
- Test: `/home/kforfk/workspace/objtrsf/trsf/mtu/plpmtud_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func (t *MTUTracker) fallBackToBase()` (unexported, called only from within the package in later tasks); `func (t *MTUTracker) Fallbacks() uint64`; fields `searchLossCount`, `validationLossCount`, `fallbacks` on `MTUTracker`.

The existing `lossCount` is shared. Once validation probes exist (Task 3) two lost search probes followed by one lost validation would trip the three-strike rule, and that mixed sequence is ordinary. Split it now, before anything can depend on the shared one.

- [ ] **Step 1: Write the failing tests**

Add to `trsf/mtu/plpmtud_test.go`:

```go
// newTestTracker is the tracker every test in this file drives. 1200/1452 are
// the production defaults; a 30s re-probe period keeps the timer out of the
// way of tests that are not about it.
func newTestTracker() *MTUTracker {
	return NewMTUTracker(1200, 1452, 30*time.Second)
}

// converge drives the binary search to completion against a path that carries
// everything up to pathMTU, and returns the estimate it settled on.
func converge(t *testing.T, tr *MTUTracker, pathMTU int, now time.Time) int {
	t.Helper()
	for i := 0; i < 64; i++ {
		size := tr.Probe(now)
		if size == -1 {
			return tr.CurrentMTU()
		}
		if size <= pathMTU {
			tr.OnACK(now)
		} else {
			// Three losses is what the tracker requires to rule a size out.
			tr.OnLost(now)
			tr.OnLost(now)
			tr.OnLost(now)
		}
	}
	t.Fatalf("search did not terminate in 64 probes")
	return 0
}

func TestFallbackLandsOnBaseAndReopensTheSearch(t *testing.T) {
	now := time.Now()
	tr := newTestTracker()
	got := converge(t, tr, 1300, now)
	if got <= 1200 || got > 1300 {
		t.Fatalf("converged at %d, want something in (1200,1300]", got)
	}

	tr.fallBackToBase()

	if tr.CurrentMTU() != 1200 {
		t.Errorf("CurrentMTU = %d after fallback, want the base 1200", tr.CurrentMTU())
	}
	if tr.Fallbacks() != 1 {
		t.Errorf("Fallbacks = %d, want 1", tr.Fallbacks())
	}
	// Reopened, not converged: the next Probe must issue immediately rather
	// than waiting on the re-probe backoff, which is only consulted when
	// low > high.
	if size := tr.Probe(now); size <= 1200 {
		t.Errorf("Probe after fallback = %d, want a size above the base", size)
	}
}

// A probe issued before a fallback can be acknowledged after it. OnACK raises
// the estimate on `lastProbe > mtu` and reads lastProbe unconditionally, so
// without re-pointing it the late ACK restores the very estimate the fallback
// ruled out -- and announces it through onMTUUpdate as an increase.
func TestLateACKOfAPreFallbackProbeDoesNotRestoreTheEstimate(t *testing.T) {
	now := time.Now()
	tr := newTestTracker()
	converge(t, tr, 1300, now)

	inFlight := tr.Probe(now) // issued at some size above the estimate
	if inFlight == -1 {
		t.Fatalf("expected a probe to be outstanding for this test")
	}
	tr.fallBackToBase()
	tr.OnACK(now) // the pre-fallback probe lands late

	if tr.CurrentMTU() != 1200 {
		t.Errorf("CurrentMTU = %d, want 1200: a probe from before the fallback must not raise the estimate", tr.CurrentMTU())
	}
}

// The OnLost twin of the same race: a late loss must not shrink the range the
// fallback just reopened.
func TestLateLossOfAPreFallbackProbeDoesNotShrinkTheReopenedRange(t *testing.T) {
	now := time.Now()
	tr := newTestTracker()
	converge(t, tr, 1300, now)

	if tr.Probe(now) == -1 {
		t.Fatalf("expected a probe to be outstanding for this test")
	}
	tr.fallBackToBase()
	tr.OnLost(now)
	tr.OnLost(now)
	tr.OnLost(now)

	if tr.MaxCandidate() < tr.MinCandidate() {
		t.Errorf("search range inverted after a late loss: low=%d high=%d",
			tr.MinCandidate(), tr.MaxCandidate())
	}
}

func TestTwoFallbacksInARowLeaveAConsistentState(t *testing.T) {
	now := time.Now()
	tr := newTestTracker()
	converge(t, tr, 1300, now)

	tr.fallBackToBase()
	tr.fallBackToBase()

	if tr.CurrentMTU() != 1200 {
		t.Errorf("CurrentMTU = %d, want 1200", tr.CurrentMTU())
	}
	if tr.MinCandidate() > tr.MaxCandidate() {
		t.Errorf("search range inverted: low=%d high=%d", tr.MinCandidate(), tr.MaxCandidate())
	}
	if tr.Fallbacks() != 2 {
		t.Errorf("Fallbacks = %d, want 2", tr.Fallbacks())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/mtu/ -run 'Fallback|LateACK|LateLoss' -v`
Expected: FAIL to compile — `tr.fallBackToBase undefined`, `tr.Fallbacks undefined`.

- [ ] **Step 3: Split the counter and add the fallback**

In `trsf/mtu/plpmtud.go`, replace the `lossCount int` field with two, add the counter, and add the method:

```go
type MTUTracker struct {
	// ... existing fields, with lossCount replaced by:

	// Two counters, not one. A search probe and a validation probe answer
	// different questions -- "is this larger size reachable" versus "is the
	// size we already use still reachable" -- and sharing a counter means two
	// lost search probes followed by one lost validation trips the
	// three-strike rule on evidence about two different sizes.
	searchLossCount     int
	validationLossCount int

	fallbacks atomic.Uint64
}

// fallBackToBase is the ONLY place in this package where the estimate
// decreases. Everything else raises it or narrows the search range, and that
// asymmetry is what makes this file readable: a reader asking "where can the
// MTU go down" gets one answer from one grep.
//
// lastProbe is re-pointed at the new estimate, and it is load-bearing. OnACK
// raises the estimate on `lastProbe > mtu` and reads lastProbe
// unconditionally, so an in-flight probe acknowledged after this call would
// otherwise restore exactly the size we just ruled out. Clearing probeSent
// lets the next Probe issue a fresh one instead of waiting for a verdict that
// no longer applies, and deriving the late OnLost from the same lastProbe
// keeps it from shrinking the range this just reopened -- no epoch counter
// needed for either.
//
// The caller must hold t.m.
func (t *MTUTracker) fallBackToBase() {
	t.mtu = t.min
	t.low = t.min + 1
	t.high = t.max
	t.lastProbe = t.mtu
	t.probeSent = false
	t.searchLossCount = 0
	t.validationLossCount = 0
	t.lastProbeConverged = time.Time{}
	t.fallbacks.Add(1)
	if t.onMTUUpdate != nil {
		t.onMTUUpdate(t.mtu)
	}
}

func (t *MTUTracker) Fallbacks() uint64 { return t.fallbacks.Load() }
```

The tests call `fallBackToBase` without holding the lock, so give it a locking wrapper for test and future use:

```go
// FallBackToBase is fallBackToBase with the lock taken.
func (t *MTUTracker) FallBackToBase() {
	t.m.Lock()
	defer t.m.Unlock()
	t.fallBackToBase()
}
```

Update the four existing `t.lossCount` sites to `t.searchLossCount`:
`NewMTUTracker`'s initialiser, `Probe`'s reset inside the re-probe branch, `OnACK`'s reset, and `OnLost`'s increment/threshold. Add `"sync/atomic"` to the imports.

In the tests above, change `tr.fallBackToBase()` to `tr.FallBackToBase()`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/... -run 'Fallback|LateACK|LateLoss|MTUD' -v`
Expected: PASS, including the pre-existing `TestMTUD`.

- [ ] **Step 5: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/mtu/plpmtud.go trsf/mtu/plpmtud_test.go
git commit -m "feat(mtu): one place where the estimate can go down

Two lost search probes followed by one lost validation would have tripped
the three-strike rule on evidence about two different sizes, so the shared
lossCount splits before validation probes exist to share it.

fallBackToBase re-points lastProbe at the new estimate. OnACK raises on
lastProbe > mtu and reads it unconditionally, so a probe acknowledged
after a fallback would otherwise restore the size the fallback just ruled
out and announce it as an increase."
```

---

### Task 2: `MTUTracker` — the data-loss verdict, with T from the RTT estimate

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/mtu/plpmtud.go`
- Test: `/home/kforfk/workspace/objtrsf/trsf/mtu/plpmtud_test.go`

**Interfaces:**
- Consumes: `fallBackToBase()` from Task 1.
- Produces: `NewMTUTracker(min, max int, reprobePeriod time.Duration, srtt func() time.Duration) *MTUTracker` — **the constructor signature changes**; `func (t *MTUTracker) OnLargePacketACKed(size int, now time.Time)`; `func (t *MTUTracker) OnLargePacketLost(size int, now time.Time)`.

A consecutive-loss count cannot work: congestion drops whole bursts, so three large losses in a row is ordinary. What distinguishes a black hole is that *nothing* large gets through while the connection stays alive on small packets.

- [ ] **Step 1: Write the failing tests**

```go
// fixedSRTT is what the tracker is given in these tests. 20 ms with k=10
// puts T at 200 ms, below the 1 s floor, so these tests run against the
// floor -- which is deliberate: the floor is the value most connections on
// a LAN will actually use.
func newVerdictTracker(srtt time.Duration) *MTUTracker {
	return NewMTUTracker(1200, 1452, 30*time.Second, func() time.Duration { return srtt })
}

// The guard that matters most. A path that is simply carrying traffic, with
// ordinary bursty loss, must never fall back.
func TestHealthyPathNeverFallsBack(t *testing.T) {
	now := time.Now()
	tr := newVerdictTracker(20 * time.Millisecond)
	converge(t, tr, 1300, now)

	for i := 0; i < 2000; i++ {
		now = now.Add(10 * time.Millisecond)
		// Every twentieth round loses a burst of large packets, as a
		// congestion event does, and the rest get through.
		if i%20 == 0 {
			for j := 0; j < 5; j++ {
				tr.OnLargePacketLost(1280, now)
			}
		}
		tr.OnLargePacketACKed(1280, now)
	}
	if tr.Fallbacks() != 0 {
		t.Fatalf("Fallbacks = %d on a healthy path: the detector fires on ordinary congestion",
			tr.Fallbacks())
	}
}

func TestLargeLostAndNoneAckedForTFallsBack(t *testing.T) {
	now := time.Now()
	tr := newVerdictTracker(20 * time.Millisecond)
	converge(t, tr, 1300, now)
	tr.OnLargePacketACKed(1280, now)

	// Large packets keep being lost; small ones keep being acknowledged, which
	// is what the fourth clause reads and what a shrunk path looks like.
	for i := 0; i < 200; i++ {
		now = now.Add(50 * time.Millisecond)
		tr.OnLargePacketLost(1280, now)
		tr.OnSmallPacketACKed(now)
	}
	if tr.Fallbacks() == 0 {
		t.Fatalf("no fallback after %v of large loss with the connection alive", 10*time.Second)
	}
	if tr.CurrentMTU() != 1200 {
		t.Errorf("CurrentMTU = %d, want the base 1200", tr.CurrentMTU())
	}
}

func TestALargeACKInsideTPreventsTheVerdict(t *testing.T) {
	now := time.Now()
	tr := newVerdictTracker(20 * time.Millisecond)
	converge(t, tr, 1300, now)

	for i := 0; i < 200; i++ {
		now = now.Add(50 * time.Millisecond)
		tr.OnLargePacketLost(1280, now)
		tr.OnSmallPacketACKed(now)
		// One large packet gets through every 500 ms: loss, but not a hole.
		if i%10 == 0 {
			tr.OnLargePacketACKed(1280, now)
		}
	}
	if tr.Fallbacks() != 0 {
		t.Fatalf("Fallbacks = %d: a large packet is getting through, so this is loss and not a black hole",
			tr.Fallbacks())
	}
}

// A connection with no ACKs at all is dead, not black-holed. Falling back
// there is harmless but the verdict would stop meaning what it is named, and
// the counter would stop being readable as a false-positive rate.
func TestNoACKsAtAllDoesNotFallBack(t *testing.T) {
	now := time.Now()
	tr := newVerdictTracker(20 * time.Millisecond)
	converge(t, tr, 1300, now)

	for i := 0; i < 200; i++ {
		now = now.Add(50 * time.Millisecond)
		tr.OnLargePacketLost(1280, now)
	}
	if tr.Fallbacks() != 0 {
		t.Fatalf("Fallbacks = %d on a connection receiving nothing: that is a dead path, not a hole",
			tr.Fallbacks())
	}
}

// size <= min is not evidence: BASE-sized packets cross a shrunk path, which
// is the whole reason BASE is the fallback target.
func TestPacketsAtOrBelowBaseAreNotEvidence(t *testing.T) {
	now := time.Now()
	tr := newVerdictTracker(20 * time.Millisecond)
	converge(t, tr, 1300, now)

	for i := 0; i < 200; i++ {
		now = now.Add(50 * time.Millisecond)
		tr.OnLargePacketLost(1100, now) // below min
		tr.OnSmallPacketACKed(now)
	}
	if tr.Fallbacks() != 0 {
		t.Fatalf("Fallbacks = %d: a loss at or below the base says nothing about size", tr.Fallbacks())
	}
}

func TestBlackHoleTimeoutScalesWithSRTTBetweenFloorAndCap(t *testing.T) {
	for _, tc := range []struct {
		srtt time.Duration
		want time.Duration
	}{
		{50 * time.Microsecond, time.Second},       // loopback: the floor
		{500 * time.Millisecond, 5 * time.Second},  // k * srtt
		{10 * time.Second, 30 * time.Second},       // the cap
	} {
		tr := newVerdictTracker(tc.srtt)
		if got := tr.blackHoleTimeout(); got != tc.want {
			t.Errorf("blackHoleTimeout at srtt=%v = %v, want %v", tc.srtt, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/mtu/ -run 'Healthy|LargeLost|LargeACKInside|NoACKs|AtOrBelowBase|BlackHoleTimeout' -v`
Expected: FAIL to compile — `NewMTUTracker` takes 3 arguments, and `OnLargePacketACKed`, `OnLargePacketLost`, `OnSmallPacketACKed`, `blackHoleTimeout` are undefined.

- [ ] **Step 3: Implement the verdict**

```go
// The black-hole timeout is k round trips, bounded.
//
// What T has to outlast is a congestion episode, and an episode is measured in
// round trips -- a constant is far too long on a LAN and far too short on a
// satellite path. srtt is well defined at exactly the moment this verdict is
// evaluated, and that is not incidental: the verdict already requires that
// ACKs are arriving, so the estimate is live off the small packets still
// getting through.
//
// PTO would fold in RTT variance for free and is still the wrong unit: it
// backs off exponentially under sustained loss, which is the state this is
// trying to recognise, so N*PTO stretches away from detection exactly when it
// is needed.
//
// The floor exists because k*srtt on loopback is microseconds. The cap bounds
// worst-case detection latency on a very slow path. All three are calibrated
// by the lossy and bufferbloat arms of the plan, not by argument.
const (
	blackHoleRTTs  = 10
	blackHoleFloor = 1 * time.Second
	blackHoleCap   = 30 * time.Second
)

func (t *MTUTracker) blackHoleTimeout() time.Duration {
	var srtt time.Duration
	if t.srtt != nil {
		srtt = t.srtt()
	}
	d := time.Duration(blackHoleRTTs) * srtt
	if d < blackHoleFloor {
		return blackHoleFloor
	}
	if d > blackHoleCap {
		return blackHoleCap
	}
	return d
}

// OnLargePacketACKed records that a packet larger than the base crossed the
// path. Sizes at or below min are not evidence about size, because those cross
// a shrunk path too -- which is the reason min is the fallback target.
func (t *MTUTracker) OnLargePacketACKed(size int, now time.Time) {
	t.m.Lock()
	defer t.m.Unlock()
	t.lastAnyACK = now
	if size <= t.min {
		return
	}
	t.lastLargeACK = now
	t.largeLostSeen = false
}

// OnSmallPacketACKed records liveness without size evidence.
func (t *MTUTracker) OnSmallPacketACKed(now time.Time) {
	t.m.Lock()
	defer t.m.Unlock()
	t.lastAnyACK = now
}

// OnLargePacketLost is the data-loss half of the detector and the half that
// works while a transfer is running. It is deliberately NOT a consecutive-loss
// count: congestion drops whole bursts, so three in a row is an ordinary
// Tuesday. What distinguishes a black hole is that nothing large gets through
// while the connection is otherwise alive.
func (t *MTUTracker) OnLargePacketLost(size int, now time.Time) {
	t.m.Lock()
	defer t.m.Unlock()
	if size <= t.min {
		return
	}
	t.largeLostSeen = true
	t.evaluateBlackHole(now)
}

// evaluateBlackHole holds t.m.
func (t *MTUTracker) evaluateBlackHole(now time.Time) {
	if t.mtu <= t.min {
		return // already at the base; there is nowhere to fall
	}
	if !t.largeLostSeen {
		return
	}
	T := t.blackHoleTimeout()
	if t.lastLargeACK.IsZero() || now.Sub(t.lastLargeACK) <= T {
		return // something large is still getting through
	}
	if t.lastAnyACK.IsZero() || now.Sub(t.lastAnyACK) > T {
		return // nothing at all is arriving: a dead path, not a hole
	}
	t.fallBackToBase()
}
```

Add the fields and change the constructor:

```go
type MTUTracker struct {
	// ... existing fields, plus:
	srtt          func() time.Duration
	lastLargeACK  time.Time
	lastAnyACK    time.Time
	largeLostSeen bool
}

// NewMTUTracker takes srtt as a function rather than a *congestion.RTTStats so
// this package does not import congestion; the estimate is read live because
// the black-hole timeout is derived from it.
func NewMTUTracker(min, max int, reprobePeriod time.Duration, srtt func() time.Duration) *MTUTracker {
	return &MTUTracker{
		mtu:                   min,
		min:                   min,
		max:                   max,
		low:                   min + 1,
		high:                  max,
		reProbeAfterConverged: reprobePeriod,
		srtt:                  srtt,
	}
}
```

`fallBackToBase` must also clear the new state, or the connection re-falls immediately on the next large loss. Add to it:

```go
	t.largeLostSeen = false
	t.lastLargeACK = time.Time{}
```

Fix the one production call site so the package compiles — `trsf/conn.go:1279` — by passing `nil` for now; Task 4 supplies the real function:

```go
		mtu: mtu.NewMTUTracker(initialMTU, maxMTU, 30*time.Second, nil),
```

Update `newTestTracker` from Task 1 to pass `nil` as well.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/... -v -run 'Healthy|LargeLost|LargeACKInside|NoACKs|AtOrBelowBase|BlackHoleTimeout|Fallback|LateACK|LateLoss|MTUD'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/mtu/plpmtud.go trsf/mtu/plpmtud_test.go trsf/conn.go
git commit -m "feat(mtu): a black hole is nothing large getting through, not three losses

A consecutive-loss count cannot separate a shrunk path from congestion --
congestion drops whole bursts. The verdict is instead: something large was
lost, nothing large has been acknowledged for T, and the connection is
still receiving something.

T is k round trips, floored and capped, rather than a constant: an episode
is measured in round trips. srtt is well defined at exactly this moment
because the verdict already requires ACKs to be arriving. PTO is rejected
despite folding in variance -- it backs off under sustained loss, so it
stretches away from detection precisely during a black hole."
```

---

### Task 3: `MTUTracker` — validation probes, `NextDeadline`, and BASE unusable

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/mtu/plpmtud.go`
- Test: `/home/kforfk/workspace/objtrsf/trsf/mtu/plpmtud_test.go`

**Interfaces:**
- Consumes: `fallBackToBase()`, `blackHoleTimeout()`.
- Produces: `func (t *MTUTracker) NextDeadline(now time.Time) (time.Time, bool)`; `func (t *MTUTracker) BaseUnusable() uint64`.

The data-loss detector needs traffic. This is the half that runs when there is none.

- [ ] **Step 1: Write the failing tests**

```go
func TestValidationProbeIsIssuedAtTheCurrentEstimateWhenConverged(t *testing.T) {
	now := time.Now()
	tr := newVerdictTracker(20 * time.Millisecond)
	settled := converge(t, tr, 1300, now)

	// Before the validation interval there is nothing to do.
	if size := tr.Probe(now); size != -1 {
		t.Fatalf("Probe = %d immediately after convergence, want -1", size)
	}
	now = now.Add(61 * time.Second)
	if size := tr.Probe(now); size != settled {
		t.Errorf("Probe = %d, want a validation probe at the current estimate %d", size, settled)
	}
}

func TestThreeLostValidationProbesFallBack(t *testing.T) {
	now := time.Now()
	tr := newVerdictTracker(20 * time.Millisecond)
	converge(t, tr, 1300, now)

	for i := 0; i < 3; i++ {
		now = now.Add(61 * time.Second)
		if tr.Probe(now) == -1 {
			t.Fatalf("no validation probe issued on round %d", i)
		}
		tr.OnLost(now)
	}
	if tr.Fallbacks() != 1 {
		t.Fatalf("Fallbacks = %d after three lost validation probes, want 1", tr.Fallbacks())
	}
}

func TestNextDeadlineIsSilentWhileAProbeIsOutstanding(t *testing.T) {
	now := time.Now()
	tr := newVerdictTracker(20 * time.Millisecond)
	converge(t, tr, 1300, now)
	now = now.Add(61 * time.Second)
	if tr.Probe(now) == -1 {
		t.Fatalf("expected a validation probe")
	}
	// A deadline here is a past timestamp the run loop wakes on, cannot act
	// on, and immediately sees again -- the 0-delay spin documented in
	// conn.go's nextWakeDeadline. The loss detection timer already covers an
	// outstanding probe.
	if _, ok := tr.NextDeadline(now); ok {
		t.Error("NextDeadline reported a deadline while a probe is outstanding")
	}
}

func TestNextDeadlineIsSilentWhenMinEqualsMax(t *testing.T) {
	tr := NewMTUTracker(16384, 16384, 30*time.Second, func() time.Duration { return 0 })
	if _, ok := tr.NextDeadline(time.Now()); ok {
		t.Error("NextDeadline reported a deadline for a transport with nothing to discover")
	}
}

// A connection converged at the ceiling must still validate, or the ceiling is
// the one place a shrink can never be noticed.
func TestConvergedAtMaxStillValidatesButDoesNotSearchUp(t *testing.T) {
	now := time.Now()
	tr := NewMTUTracker(1200, 1452, 30*time.Second, func() time.Duration { return 0 })
	converge(t, tr, 9000, now) // a path wider than max: converges at max
	if tr.CurrentMTU() != 1452 {
		t.Fatalf("CurrentMTU = %d, want the ceiling 1452", tr.CurrentMTU())
	}
	now = now.Add(61 * time.Second)
	if size := tr.Probe(now); size != 1452 {
		t.Errorf("Probe = %d at the ceiling, want a validation probe at 1452", size)
	}
}

// The search collapsing with the base itself still being lost is the one state
// nothing below is attempted from (RFC 9000 s14). It is counted, not repaired.
func TestBaseUnusableIsCountedWhenTheSearchCollapsesAtMin(t *testing.T) {
	now := time.Now()
	tr := NewMTUTracker(1200, 1452, 30*time.Second, func() time.Duration { return 0 })
	// A path that carries nothing at all: every probe, including validation at
	// the base, is lost.
	for i := 0; i < 256 && tr.BaseUnusable() == 0; i++ {
		now = now.Add(61 * time.Second)
		if tr.Probe(now) == -1 {
			continue
		}
		tr.OnLost(now)
	}
	if tr.BaseUnusable() == 0 {
		t.Fatal("BaseUnusable never rose on a path that carries nothing")
	}
	if tr.CurrentMTU() != 1200 {
		t.Errorf("CurrentMTU = %d, want the base 1200: nothing below it is attempted", tr.CurrentMTU())
	}
}

func TestTrackerInvariantsHoldForAnyConfiguredBounds(t *testing.T) {
	for _, tc := range []struct{ min, max int }{
		{1200, 1452}, // production udp
		{16384, 16384}, // a stream transport: nothing to discover
		{1200, 1201}, // a degenerate one-step search
		{576, 1452},  // min below the QUIC base: a caller's default, not a constant
		{68, 1452},   // the IPv4 minimum
	} {
		now := time.Now()
		tr := NewMTUTracker(tc.min, tc.max, 30*time.Second, func() time.Duration { return 0 })
		got := converge(t, tr, 1300, now)
		if got < tc.min || got > tc.max {
			t.Errorf("(%d,%d): converged at %d, outside the bounds", tc.min, tc.max, got)
		}
		tr.FallBackToBase()
		if tr.CurrentMTU() != tc.min {
			t.Errorf("(%d,%d): fallback landed on %d, want min", tc.min, tc.max, tr.CurrentMTU())
		}
		if d, ok := tr.NextDeadline(now); ok && d.Before(now) {
			t.Errorf("(%d,%d): NextDeadline is in the past", tc.min, tc.max)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/mtu/ -run 'Validation|NextDeadline|ConvergedAtMax|BaseUnusable|Invariants' -v`
Expected: FAIL — `NextDeadline` and `BaseUnusable` undefined; the validation tests fail because `Probe` returns -1 once converged at or above max.

- [ ] **Step 3: Implement validation and the deadline**

```go
// validationInterval is how often a converged tracker re-proves the size it is
// already using. Separate from reProbeAfterConverged on purpose: that one
// backs off to ~32 min so a path which is not changing is not re-searched
// forever, and a detector on that schedule would take half an hour to notice a
// wedge.
const validationInterval = 60 * time.Second

// probesDiscoverable reports whether this transport has a path MTU at all.
// A stream transport is constructed with min == max (peer.MTUForTransport
// returns StreamMTU for both on ws/wss): its size is a framing choice, so
// there is nothing to discover and nothing that can shrink. Gating on the
// VALUES rather than on a transport name is deliberate -- the values are what
// make the question meaningless.
func (t *MTUTracker) probesDiscoverable() bool { return t.min < t.max }

// NextDeadline is when this tracker next has something to do, for the run
// loop's wake computation. Reporting nothing is always safe: it only means the
// loop will not wake on our account.
func (t *MTUTracker) NextDeadline(now time.Time) (time.Time, bool) {
	t.m.Lock()
	defer t.m.Unlock()
	if !t.probesDiscoverable() || t.probeSent {
		return time.Time{}, false
	}
	if t.low <= t.high {
		return now, true // searching: there is a probe to issue right now
	}
	if t.lastProbeConverged.IsZero() {
		return time.Time{}, false
	}
	next := t.lastProbeConverged.Add(validationInterval)
	if reprobe := t.lastProbeConverged.Add(
		t.reProbeAfterConverged * time.Duration(1<<t.reprobeBackoffCount)); reprobe.Before(next) {
		next = reprobe
	}
	return next, true
}

func (t *MTUTracker) BaseUnusable() uint64 { return t.baseUnusable.Load() }
```

Rewrite `Probe`'s converged branch so the ceiling case validates instead of stopping:

```go
func (t *MTUTracker) Probe(now time.Time) int {
	t.m.Lock()
	defer t.m.Unlock()
	if t.probeSent || !t.probesDiscoverable() {
		return -1
	}
	if t.low > t.high {
		reprobeDue := now.After(t.lastProbeConverged.Add(
			t.reProbeAfterConverged * time.Duration(1<<t.reprobeBackoffCount)))
		validateDue := now.After(t.lastProbeConverged.Add(validationInterval))
		switch {
		case reprobeDue && t.mtu < t.max:
			// Reopen the search upward, exactly as before.
			t.low = t.mtu + 1
			t.high = t.max
			t.lastProbeConverged = time.Time{}
			t.searchLossCount = 0
			if t.reprobeBackoffCount < maxReprobeBackoff {
				t.reprobeBackoffCount++
			}
		case validateDue:
			// Re-prove the size already in use. This is the ONLY detector that
			// works on a connection with no traffic, and it must survive the
			// mtu >= max case: the ceiling is otherwise the one place a shrink
			// can never be noticed.
			t.probeSent = true
			t.validating = true
			t.lastProbe = t.mtu
			t.lastProbeConverged = now
			return t.lastProbe
		default:
			return -1
		}
	}
	t.probeSent = true
	t.validating = false
	t.lastProbe = (t.low + t.high + 1) / 2
	return t.lastProbe
}
```

Route `OnLost` by which kind of probe is outstanding:

```go
func (t *MTUTracker) OnLost(now time.Time) {
	t.m.Lock()
	defer t.m.Unlock()
	wasValidating := t.validating
	t.probeSent = false
	t.validating = false

	if wasValidating {
		t.validationLossCount++
		if t.validationLossCount < 3 {
			return
		}
		t.validationLossCount = 0
		if t.mtu <= t.min {
			// The base itself is not crossing. Nothing below it is attempted:
			// RFC 9000 s14 -- "QUIC MUST NOT be used if the network path
			// cannot support a maximum datagram size of at least 1200 bytes."
			// Counted so the row explains a connection that answers small
			// calls and hangs on large ones.
			t.baseUnusable.Add(1)
			return
		}
		t.fallBackToBase()
		return
	}

	t.searchLossCount++
	if t.searchLossCount >= 3 {
		t.high = t.lastProbe - 1
		t.searchLossCount = 0
		t.mayDetectConverged(now)
	}
}
```

`OnACK` must not treat a validation probe as a search result — it neither raises the estimate nor advances `low`:

```go
func (t *MTUTracker) OnACK(now time.Time) {
	t.m.Lock()
	defer t.m.Unlock()
	wasValidating := t.validating
	t.probeSent = false
	t.validating = false

	if wasValidating {
		// The size we already use still crosses. That is the whole answer.
		t.validationLossCount = 0
		return
	}

	t.searchLossCount = 0
	if t.lastProbe > t.mtu {
		t.mtu = t.lastProbe
		t.reprobeBackoffCount = 0
		if t.onMTUUpdate != nil {
			t.onMTUUpdate(t.mtu)
		}
	}
	t.low = t.lastProbe + 1
	t.mayDetectConverged(now)
}
```

Add the fields `validating bool` and `baseUnusable atomic.Uint64`, and clear `validating` and `validationLossCount` in `fallBackToBase`.

`mayDetectConverged` stamps `lastProbeConverged` only when it is zero; the validation branch above sets it on every validation probe so the next one is an interval later. Leave `mayDetectConverged` as it is.

- [ ] **Step 4: Run the whole package**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/... -count=1`
Expected: PASS. If `TestPeakGoroutines` fails, check the printed peak — near the limit is host load and not this change; five hundred is a real regression.

- [ ] **Step 5: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/mtu/plpmtud.go trsf/mtu/plpmtud_test.go
git commit -m "feat(mtu): validate the size in use, and say when the base itself will not cross

The data-loss detector needs traffic. This is the half that runs when there
is none: once converged, re-prove the current estimate every 60s. Separate
from the re-probe backoff, which reaches ~32 min by design and would make
detection take half an hour.

mtu >= max no longer stops probing, only searching up -- the ceiling was
otherwise the one place a shrink could never be noticed. Three lost
validations at the base raise baseUnusable instead of falling further:
RFC 9000 s14 says nothing below is attempted, so this is counted, not
repaired.

NextDeadline reports nothing while a probe is outstanding (that deadline is
a past timestamp and the loop would spin on it) and nothing when min == max
(a stream transport has no path MTU, so every idle ws connection stays
parked)."
```

---

### Task 4: Wire the tracker into `Streams`

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/conn.go` (`nextWakeDeadline` at :653, `NewStreams` at :1256, `GetInternalState` at :341, the stream-data `SentPacket` construction around :940-1000)
- Modify: `/home/kforfk/workspace/objtrsf/trsf/api.go` (the `InternalState` struct)
- Test: `/home/kforfk/workspace/objtrsf/trsf/mtu_wiring_test.go` (create)

**Interfaces:**
- Consumes: `NextDeadline`, `OnLargePacketACKed`, `OnSmallPacketACKed`, `OnLargePacketLost`, `Fallbacks`, `BaseUnusable`.
- Produces: `InternalState.MTUFallbacks uint64`, `InternalState.MTUBaseUnusable uint64`.

- [ ] **Step 1: Write the failing test**

Create `trsf/mtu_wiring_test.go`:

```go
package trsf

import (
	"testing"
	"time"
)

// An idle UDP-shaped connection must have a wake deadline, or the validation
// probe of the previous task never runs: Probe() is only reached when the loop
// happens to wake, and an idle connection's loss timer is disarmed and its
// pacer is skipped.
func TestIdleConnectionHasAnMTUWakeDeadline(t *testing.T) {
	s := newStreamsForTest(t, DefaultInitialMTU, DefaultMaxMTU)
	// Drive the tracker to a converged state so the validation interval
	// governs, then ask what the loop would wait on.
	deadline, ok := s.nextWakeDeadline()
	if !ok && deadline.IsZero() {
		t.Fatal("an idle udp connection has no wake deadline at all: Probe() will never be reached")
	}
}

// A stream transport is constructed min == max. Adding a periodic wake for
// every idle WebSocket connection in a fleet would be a regression, and the
// gate that prevents it is on the values, not on a transport name.
func TestIdleStreamTransportHasNoMTUWakeDeadline(t *testing.T) {
	s := newStreamsForTest(t, 16384, 16384)
	if d, ok := s.mtu.NextDeadline(time.Now()); ok {
		t.Errorf("a min==max transport asked for a wake at %v", d)
	}
}

func TestInternalStateCarriesTheMTUCounters(t *testing.T) {
	s := newStreamsForTest(t, DefaultInitialMTU, DefaultMaxMTU)
	s.mtu.FallBackToBase()
	st := s.GetInternalState()
	if st.MTUFallbacks != 1 {
		t.Errorf("InternalState.MTUFallbacks = %d, want 1", st.MTUFallbacks)
	}
}
```

Add the helper in the same file, following how the existing `trsf` tests build a `Streams` (read `trsf/throughput_test.go`'s `mockPair` for the established pattern and reuse its constructor call rather than inventing one):

```go
func newStreamsForTest(t *testing.T, initialMTU, maxMTU int) *Streams {
	t.Helper()
	tr := NewStreams(t.Context(), false, initialMTU, maxMTU, newTestPacketNumberIssuer(), slog.New(slog.DiscardHandler))
	s, ok := tr.(*Streams)
	if !ok {
		t.Fatalf("NewStreams returned %T, want *Streams", tr)
	}
	return s
}
```

If `newTestPacketNumberIssuer` does not exist, use whatever `throughput_test.go` passes for `pnIssuer`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/ -run 'IdleConnection|IdleStreamTransport|InternalStateCarriesTheMTU' -v`
Expected: FAIL — `InternalState.MTUFallbacks` undefined, and `nextWakeDeadline` returns nothing for an idle connection.

- [ ] **Step 3: Wire it**

In `NewStreams`, **move the `rtt := congestion.NewRTTStats(...)` line above the struct literal** — it is currently created at :1289, after `NewMTUTracker` is called at :1279 — store it on `Streams`, and pass the accessor:

```go
	rtt := congestion.NewRTTStats(333 * time.Millisecond)
	s := &Streams{
		// ... unchanged fields ...
		rtt: rtt,
		mtu: mtu.NewMTUTracker(initialMTU, maxMTU, 30*time.Second,
			func() time.Duration { return rtt.SRTT }),
	}
```

Add `rtt *congestion.RTTStats` to the `Streams` struct and delete the later local.

In `nextWakeDeadline`, take the tracker's deadline as a third candidate. Keep the pacer's early return as it is; fold the MTU deadline in beside the loss timer:

```go
	if mtuDeadline, ok := s.mtu.NextDeadline(time.Now()); ok {
		if deadline.IsZero() || mtuDeadline.Before(deadline) {
			deadline = mtuDeadline
		}
	}
```

Place it directly after `deadline := s.sh.LossDetectionTimeout()` and before the pacer block, so the pacer keeps its precedence when it applies.

In `GetInternalState`, add the two fields beside the datagram block:

```go
		MTUFallbacks:    s.mtu.Fallbacks(),
		MTUBaseUnusable: s.mtu.BaseUnusable(),
```

and declare them on `InternalState` in `trsf/api.go`, next to `CurrentMTU`.

Feed the detector from the stream-data `SentPacket` callbacks. In `conn.go` where a stream's `SentRange` is turned into a `SentPacket` (around :960, the non-probe path), wrap the existing callbacks:

```go
			size := len(encodedPkt)
			existingACK, existingLost := sentRange.OnACK, sentRange.OnLost
			p := &SentPacket{
				// ... existing fields ...
				OnACK: func(now time.Time) {
					s.mtu.OnLargePacketACKed(size, now)
					existingACK(now)
				},
				OnLost: func(now time.Time) {
					s.mtu.OnLargePacketLost(size, now)
					existingLost(now)
				},
			}
```

`OnLargePacketACKed` itself records liveness for every size and only treats `size > min` as size evidence, so no separate small-packet call is needed on this path. Call `s.mtu.OnSmallPacketACKed(now)` from the ACK-frame handling path so a connection carrying only ACKs still counts as alive.

- [ ] **Step 4: Run the tests**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/conn.go trsf/api.go trsf/mtu_wiring_test.go
git commit -m "feat(trsf): give the MTU tracker a wake deadline and real packet evidence

Probe()'s interval was checked inside Probe() and nothing armed a timer for
it: nextWakeDeadline offered only the loss and pacing timers, and an idle
connection has the first disarmed and the second skipped. So the existing
UPWARD re-probe was already best-effort, and a detector whose job is the
idle case inherited that. The tracker's deadline is now a third candidate.

Non-probe packets feed OnLargePacketACKed / OnLargePacketLost through the
callbacks MTU probes already use, so ack_handler gains no new concept.

rtt moves above the struct literal because the tracker now needs it: it was
constructed ten lines after NewMTUTracker was called."
```

---

### Task 5: Re-split a retransmit that no longer fits

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/send_stream.go:265-282`
- Test: `/home/kforfk/workspace/objtrsf/trsf/send_stream_split_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: no new exported surface. `triggerPacket(maxPayload int) *SentRange` keeps its signature.

Lowering the estimate does nothing for data already in flight: the retransmit branch guards on whether the **header** fits, not the chunk, so a chunk captured at the old budget is returned whole forever. That guard is correct today only because the estimate never decreases.

- [ ] **Step 1: Write the failing tests**

Create `trsf/send_stream_split_test.go`. Build a `sendStream` the way the existing `send_stream` tests do; if there is no helper, add one alongside these tests.

```go
package trsf

import (
	"testing"
	"time"
)

// A chunk captured at a larger budget must be split to fit the current one,
// not returned whole. Returning it whole is what makes a lowered MTU estimate
// change nothing for data already in flight.
func TestRetransmitSplitsToTheCurrentBudget(t *testing.T) {
	s := newSendStreamForTest(t)
	queueRetransmit(t, s, 0, make([]byte, 1200), false)

	first := s.triggerPacket(600)
	if first == nil {
		t.Fatal("triggerPacket returned nil with a range queued and room for it")
	}
	if got := StreamPacketHeaderSize(first.ID, first.Offset, len(first.Data)) + len(first.Data); got > 600 {
		t.Fatalf("first fragment is %d bytes against a 600 budget", got)
	}

	second := s.triggerPacket(600)
	if second == nil {
		t.Fatal("the tail was not re-queued")
	}
	if second.Offset != first.Offset+uint64(len(first.Data)) {
		t.Errorf("tail offset %d does not continue the head (%d + %d)",
			second.Offset, first.Offset, len(first.Data))
	}
	if len(first.Data)+len(second.Data) != 1200 {
		t.Errorf("split lost or duplicated bytes: %d + %d != 1200", len(first.Data), len(second.Data))
	}
}

// EOF on the head ends the stream at the head's last offset and the remainder
// is never delivered.
func TestSplitPutsEofOnTheTailOnly(t *testing.T) {
	s := newSendStreamForTest(t)
	queueRetransmit(t, s, 0, make([]byte, 1200), true)

	head := s.triggerPacket(600)
	if head.Eof {
		t.Error("head carries Eof: the receiver would end the stream before the tail arrives")
	}
	tail := s.triggerPacket(600)
	if !tail.Eof {
		t.Error("tail does not carry Eof: the stream never ends")
	}
}

// Splitting is not a one-shot. The budget can drop again before the tail goes
// out.
func TestTheTailSplitsAgainWhenTheBudgetDropsTwice(t *testing.T) {
	s := newSendStreamForTest(t)
	queueRetransmit(t, s, 0, make([]byte, 1200), false)

	a := s.triggerPacket(800)
	b := s.triggerPacket(300)
	c := s.triggerPacket(300)
	if a == nil || b == nil || c == nil {
		t.Fatal("expected three fragments")
	}
	total := len(a.Data) + len(b.Data) + len(c.Data)
	if total > 1200 {
		t.Fatalf("fragments carry %d bytes, more than the original 1200", total)
	}
	for _, f := range []*SentRange{b, c} {
		if got := StreamPacketHeaderSize(f.ID, f.Offset, len(f.Data)) + len(f.Data); got > 300 {
			t.Errorf("fragment at offset %d is %d bytes against a 300 budget", f.Offset, got)
		}
	}
}

// The original guard's job: a budget with no room for even one byte of payload
// must push back, not emit an empty range that consumes a queue slot and
// advances nothing.
func TestABudgetTooSmallForOneBytePushesBack(t *testing.T) {
	s := newSendStreamForTest(t)
	queueRetransmit(t, s, 0, make([]byte, 1200), false)

	if got := s.triggerPacket(2); got != nil {
		t.Fatalf("triggerPacket returned a %d-byte range against a 2-byte budget", len(got.Data))
	}
	// Still queued, and still whole.
	if got := s.triggerPacket(600); got == nil {
		t.Fatal("the range was dropped rather than pushed back")
	}
}

// A head with no bytes and no EOF advances nothing, and the tail is the same
// range again: the shape that turns the retransmit queue into a spin.
func TestAnEofOnlyRangeIsNotSplit(t *testing.T) {
	s := newSendStreamForTest(t)
	queueRetransmit(t, s, 0, nil, true)

	got := s.triggerPacket(600)
	if got == nil {
		t.Fatal("an Eof-only range was not returned")
	}
	if !got.Eof || len(got.Data) != 0 {
		t.Errorf("got Data=%d Eof=%v, want the range returned whole", len(got.Data), got.Eof)
	}
}

// sentRanges is matched by pointer identity. Splitting must replace the one
// entry with the two new ranges: leaving the original retires data that was
// never acknowledged, and appending without removing degrades the O(1) head
// path into the O(n) filter.
func TestSplitReplacesTheOriginalInSentRanges(t *testing.T) {
	s := newSendStreamForTest(t)
	original := queueRetransmit(t, s, 0, make([]byte, 1200), false)

	head := s.triggerPacket(600)
	tail := s.triggerPacket(600)

	s.m.Lock()
	tracked := append([]*SentRange(nil), s.sentRanges...)
	s.m.Unlock()
	for _, sr := range tracked {
		if sr == original {
			t.Error("the original range is still tracked after being split")
		}
	}
	for _, want := range []*SentRange{head, tail} {
		found := false
		for _, sr := range tracked {
			if sr == want {
				found = true
			}
		}
		if !found {
			t.Errorf("fragment at offset %d is not tracked in sentRanges", want.Offset)
		}
	}

	now := time.Now()
	head.OnACK(now)
	tail.OnACK(now)
	s.m.Lock()
	remaining := len(s.sentRanges)
	s.m.Unlock()
	if remaining != 0 {
		t.Errorf("%d ranges still tracked after both fragments were acknowledged", remaining)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/ -run 'Retransmit|Split|BudgetTooSmall|EofOnly' -v`
Expected: FAIL — the first fragment comes back at 1200 bytes against a 600 budget.

- [ ] **Step 3: Implement the split**

Replace the retransmit branch at `send_stream.go:268-281`:

```go
	if popped := r.retransmitQueue.Pop(); popped != nil {
		headerSize := StreamPacketHeaderSize(popped.ID, popped.Offset, len(popped.Data))
		switch {
		case headerSize+len(popped.Data) <= maxPayload:
			r.logger.Debug("retransmitting stream data", "id", popped.ID, "offset", popped.Offset, "size", len(popped.Data))
			// This return skips the tail of the function, so the re-queue has
			// to happen here too — see requeueIfMore for what it cost when it
			// did not.
			r.requeueIfMore()
			return popped

		case len(popped.Data) == 0 || maxPayload <= headerSize+1:
			// Nothing to split. An Eof-only range has no bytes to divide, and
			// a budget with no room for a single byte would produce a head
			// that advances nothing and a tail identical to the input — a spin
			// rather than progress. Push back and wait for a budget that fits.
			r.retransmitQueue.Push(popped)

		default:
			head, tail := r.splitSentRange(popped, maxPayload)
			r.retransmitQueue.Push(tail)
			r.logger.Debug("re-split a retransmission to the current budget",
				"id", popped.ID, "offset", popped.Offset,
				"was", len(popped.Data), "now", len(head.Data), "budget", maxPayload)
			r.requeueIfMore()
			return head
		}
	}
```

Add the splitter beside it. `headerSize` is recomputed per fragment because the length field is a varint and a shorter fragment can need a narrower one — the same class of error the comment below this branch records:

```go
// splitSentRange divides a queued retransmission that no longer fits the
// current budget. The caller holds r.m.
//
// Both fragments replace the original in sentRanges, which is matched by
// pointer identity: leaving the original behind retires data that was never
// acknowledged, and appending without removing degrades onACK's O(1) head path
// into its O(n) fallback.
//
// Eof rides the TAIL. On the head it ends the stream at the head's last offset
// and the remainder is never delivered.
//
// Neither fragment touches the flow controller. The original send already
// consumed the window; a retransmission is the same bytes again.
func (r *sendStream) splitSentRange(src *SentRange, maxPayload int) (head, tail *SentRange) {
	n := maxPayload - StreamPacketHeaderSize(src.ID, src.Offset, len(src.Data))
	if n > len(src.Data) {
		n = len(src.Data)
	}
	head = &SentRange{ID: src.ID, Offset: src.Offset, Data: src.Data[:n], SentSize: n}
	tail = &SentRange{
		ID:       src.ID,
		Offset:   src.Offset + uint64(n),
		Data:     src.Data[n:],
		SentSize: len(src.Data) - n,
		Eof:      src.Eof,
	}
	for _, f := range []*SentRange{head, tail} {
		fr := f
		fr.OnACK = func(now time.Time) { r.onACK(fr, now) }
		fr.OnLost = func(now time.Time) {
			r.retransmitQueue.Push(fr)
			r.sendTrigger.PushBecause(r, pushLoss)
		}
	}
	r.replaceSentRange(src, head, tail)
	return head, tail
}

// replaceSentRange swaps one tracked range for the fragments it became. The
// caller holds r.m.
func (r *sendStream) replaceSentRange(src *SentRange, with ...*SentRange) {
	for i, sr := range r.sentRanges {
		if sr == src {
			r.sentRanges = append(r.sentRanges[:i], append(append([]*SentRange{}, with...), r.sentRanges[i+1:]...)...)
			return
		}
	}
	// Already retired by an ACK that overtook the loss. The fragments are
	// still sent — the peer discards duplicate offsets — but nothing tracks
	// them, which is what an untracked retransmission already meant here.
	r.sentRanges = append(r.sentRanges, with...)
}
```

Re-verify the head's encoded size: `n` is derived from the header width for the **full** `len(src.Data)`, so the head's own header is the same width or narrower and the fragment cannot exceed `maxPayload`.

- [ ] **Step 4: Run the tests**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./trsf/... -count=1`
Expected: PASS, including the throughput rungs' correctness assertions.

- [ ] **Step 5: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/send_stream.go trsf/send_stream_split_test.go
git commit -m "fix(trsf): a retransmission can be re-split to the current budget

The guard asked whether the HEADER fits, not the chunk, so a chunk captured
at a 1270-byte budget was returned whole into a 1200-byte one. That guard
was correct while the MTU estimate could only rise; it stops being correct
the moment the estimate can fall, so it changes with it.

Eof rides the tail -- on the head it ends the stream at the head's last
offset. headerSize is recomputed per fragment because the length field is a
varint. Both fragments replace the original in sentRanges, which is matched
by pointer identity, so leaving the original behind would retire data that
was never acknowledged."
```

---

### Task 6: Land `objtrsf`, bump the harness, carry the counters

**Files:**
- Modify: `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/<this-worktree>/go.mod`, `go.sum`
- Modify: `runner/protocol/message.bgn` (the `TrsfCounterKey` enum)
- Modify: `runner/protocol/trsf_row.go` (the `add(...)` block around :24-30)
- Regenerate: `runner/protocol/message.go`
- Test: `runner/protocol/trsf_row_test.go` (existing — `TestTrsfRowFromCarriesEveryNumberInInternalState`, :57)

**Interfaces:**
- Consumes: `InternalState.MTUFallbacks`, `InternalState.MTUBaseUnusable` from Task 4.
- Produces: `TrsfCounterKey_MtuFallbacks`, `TrsfCounterKey_MtuBaseUnusable`.

- [ ] **Step 1: Land objtrsf and bump**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./... -count=1
git log --oneline -6
# FF-push to trunk per the recorded policy for this repo, then:
cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/<this-worktree>
go get github.com/on-keyday/objtrsf@<new-sha>
go mod tidy
```

- [ ] **Step 2: Run the existing coverage test to verify it fails**

Run: `go test ./runner/protocol/ -run TestTrsfRowFromCarriesEveryNumberInInternalState -v`
Expected: FAIL, naming `MTUFallbacks` and `MTUBaseUnusable` as numbers no row carries. This test is why no new test is written here — it already does the job it did for the nine datagram counters.

- [ ] **Step 3: Append the two members and carry them**

In `runner/protocol/message.bgn`, append to `TrsfCounterKey` after `unrouted_transport_kind` (existing ordinals unchanged, so an older runner omits these rather than reporting zeros):

```
    # How many times the estimate fell because a path stopped carrying a size
    # it had already proven. A fleet-wide rise means the detector is firing on
    # congestion; zero everywhere means it is not firing at all, and those two
    # are indistinguishable without this counter.
    mtu_fallbacks = "mtu_fallbacks"

    # How many times the search collapsed with the base itself still being
    # lost. Nothing below the base is attempted (RFC 9000 s14), so this is the
    # row that explains a connection answering small calls and hanging on
    # large ones. A COUNT, like every member here: it reads "this happened N
    # times", never "this is true now". Pair it with mtu on the same row to
    # tell a connection that is currently stuck from one that was.
    mtu_base_unusable = "mtu_base_unusable"
```

Regenerate: `bash scripts/protoregen.sh`. The regen churn in `message.go` is expected and is not investigated.

In `runner/protocol/trsf_row.go`, beside the existing `add(TrsfCounterKey_Mtu, uint64(st.CurrentMTU))` at :27:

```go
	add(TrsfCounterKey_MtuFallbacks, st.MTUFallbacks)
	add(TrsfCounterKey_MtuBaseUnusable, st.MTUBaseUnusable)
```

- [ ] **Step 4: Verify**

```bash
make vet
make test
go test ./runner/protocol/ -run TestTrsfRowFromCarriesEveryNumberInInternalState -v
bash scripts/wire-skew-check.sh
```
Expected: all pass. `wire-skew-check.sh` runs unconditionally because `message.bgn` changed; an append to a keyed list with unchanged ordinals is the shape it should report as recoverable.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum runner/protocol/
git commit -m "feat(trsf-state): bump objtrsf to the downward MTU transition, and carry its counters

mtu_fallbacks distinguishes 'the detector is firing on congestion' from
'the detector is not firing at all', which are the same picture without it.
mtu_base_unusable is the row that explains a connection answering small
calls and hanging on large ones.

Both are counts, appended, existing ordinals unchanged -- so an older
runner omits them rather than reporting zeros, which is the distinction the
keyed list exists for."
```

---

### Task 7: Calibrate `k` and accept on `netem-lab`

**Files:**
- No source changes expected. If an arm fails, the fix lands in the objtrsf task it belongs to and this task re-runs.
- Modify (only if calibration moves them): the three constants in `trsf/mtu/plpmtud.go` from Task 2.

**Interfaces:**
- Consumes: everything.
- Produces: measured values for `blackHoleRTTs`, `blackHoleFloor`, `blackHoleCap`.

`k = 10`, `floor = 1s` and `cap = 30s` were picked by argument in the spec and are explicitly there to be replaced by measurement. **The two no-shrink arms are the calibration**: they are the only shapes that make a healthy path lose large packets in bursts, which is the whole false-positive hazard.

- [ ] **Step 1: The false-positive arms**

```bash
scripts/netem-lab/netem-lab.py --name cal up --profile lossy
scripts/netem-lab/netem-lab.py --name cal bench --runs 6 --size-mb 64
scripts/netem-lab/netem-lab.py --name cal exec cli -- \
  harness-cli --server-cid "$CID" conns --trsf --json
scripts/netem-lab/netem-lab.py --name cal down
```
Expected: `mtu_fallbacks` **0** on every row. Repeat with `--profile bufferbloat`. If either produces a fallback, raise `blackHoleRTTs` and re-run both; do not raise the floor first, because the floor only governs paths whose srtt is tiny and neither profile is one.

- [ ] **Step 2: The shrink arms, above the base**

```bash
scripts/netem-lab/netem-lab.py --name sh up --profile lan --mtu 1300
# confirm convergence near 1270 first:
scripts/netem-lab/netem-lab.py --name sh exec cli -- \
  harness-cli --server-cid "$CID" conns --trsf --json
scripts/netem-lab/netem-lab.py --name sh shape --profile lan --mtu 1260
```
Expected: `mtu` falls to 1200 and re-converges near 1230 within tens of seconds, `mtu_fallbacks` is 1, and `conns --trsf` answers throughout — that last one is the symptom this whole change exists for. Re-run with `--pmtu-blackhole` added at `up`: with ICMP delivered the local kernel learns the new size first and can mask the effect.

- [ ] **Step 3: The idle arm**

The same as Step 2 with **no traffic offered at all** between `up` and `shape`. This is the only arm that exercises Task 4's wake deadline, and the only one that would still pass if it had been forgotten.

- [ ] **Step 4: Below the base, and the ws control**

```bash
scripts/netem-lab/netem-lab.py --name lo up --profile lan --mtu 1300
scripts/netem-lab/netem-lab.py --name lo shape --profile lan --mtu 1100
```
Expected: `mtu_base_unusable` non-zero. The connection is **not** repaired — 1100 cannot carry the base, and the spec's §2 says so. This is the reproduction that opened the investigation; its outcome changes from "hangs with nothing saying why" to "says why".

Then `up --transport ws` with connections established and no traffic: `loop_iterations` must not rise, which is the `min == max` gate checked in place rather than only in a unit test.

**Read both endpoints on every arm.** The previous PLPMTUD defect showed only on the receive-dominated side and was hidden by dumping the server alone:

```bash
RC=$(... exec cli -- harness-cli --server-cid "$CID" conns | awk '$2=="runner"{print $1}')
... exec cli -- harness-cli --server-cid "$CID" conns --trsf --runner "$RC" --json
```

- [ ] **Step 5: Throughput regression on the rungs**

`mock` is **not** a free control here: unlike the datagram frame, this change is inside `trsf` itself including the run loop's wake computation, so both rungs can legitimately move.

```bash
cd /home/kforfk/workspace/objtrsf
git stash list   # ensure a clean tree; use a WIP commit, never a bare stash
go test -c ./trsf -o /tmp/trsf-BEFORE.test   # built at the pre-Task-1 commit
go test -c ./trsf -o /tmp/trsf-AFTER.test
# 16 super-iterations, ABBA and BAAB alternating so each arm takes every slot:
for i in $(seq 1 16); do
  if [ $((i % 2)) -eq 1 ]; then ORDER="BEFORE AFTER AFTER BEFORE"; else ORDER="AFTER BEFORE BEFORE AFTER"; fi
  for arm in $ORDER; do
    /tmp/trsf-$arm.test -test.run '^$' -test.bench 'Throughput/(mock|udp)$' \
      -test.benchtime 1x -test.count 1
  done
done
```

Plain ABBA is **not** sufficient: measured 2026-09-17 on this box, position medians were U-shaped and ABBA hands both middle slots to one arm, which produced a −5.8% that reversed under the balanced order. Record `/proc/loadavg` before and after.

The idle-wake cost does not appear in throughput. Measure it separately as CPU on a lab with connections established and no traffic.

- [ ] **Step 6: Land and record**

Land the feature set — spec, plan, objtrsf bump, counters — as one unit per the recorded policy, then `make build` in the main checkout. Write the calibrated `k`/floor/cap into `trsf/mtu/plpmtud.go`'s constants with the measurement that chose them, and update the spec's §5b if they moved.

---

## Self-Review

**Spec coverage.** §5a wake deadline and both gates → Task 3 (`NextDeadline`) + Task 4 (wiring). §5b verdict, T formula, separated counters → Tasks 1–2. §5c fallback incl. the `lastProbe` race → Task 1. §5d below-BASE reporting → Task 3. §5e split and its three obligations → Task 5. §4b wire → Task 6. §7 operator surface → Task 6. §10 every scenario → Tasks 1–5 (unit) and 7 (lab). §9's "detector fires on congestion" → Task 7 Step 1, which is what sets `k`.

**Gap found and closed:** the spec's §5b names `OnLargePacketACKed`/`OnLargePacketLost` only; liveness needs a third call for a connection carrying ACKs alone. `OnSmallPacketACKed` is introduced in Task 2 and wired in Task 4.

**Type consistency.** `fallBackToBase` (unexported, lock held) vs `FallBackToBase` (exported wrapper) are used consistently: tests call the exported one, `evaluateBlackHole` and `OnLost` call the unexported one. `NewMTUTracker`'s fourth parameter is `func() time.Duration` in Task 2's definition, Task 3's tests, and Task 4's call site. `InternalState.MTUFallbacks`/`MTUBaseUnusable` match `TrsfCounterKey_MtuFallbacks`/`MtuBaseUnusable` in Task 6.

**Known soft spots for the implementer, not placeholders:** Task 4's `newStreamsForTest` and Task 5's `newSendStreamForTest`/`queueRetransmit` follow whatever the existing tests in those files already do; read `throughput_test.go` and the existing `send_stream` tests and reuse their constructors rather than inventing new ones. Task 4's exact `SentPacket` construction site is described by content ("the non-probe stream-data path") because the line will have moved by then.
