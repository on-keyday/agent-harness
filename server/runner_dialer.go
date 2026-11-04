package server

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// dialManager keeps every dial record's runner connected: one loop per record,
// dialing its candidates in order, waiting while the connection lives, and
// backing off between attempts
// (docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md §1.2).
//
// A runner has one connection whoever dialed it. Two loops aimed at one runner
// would take it from each other forever, killing its tasks on every switch, so
// the manager tracks which record owns which connection and settles the
// conflict instead (D20): a record finding the runner already connected at its
// own address adopts the connection, or yields to the record that owns it; a
// record whose connection another record's displaced yields too.
type dialManager struct {
	cfg dialManagerConfig

	mu      sync.Mutex
	ctx     context.Context // set by Start
	loops   map[uint32]*dialLoop
	owner   map[objproto.ConnectionID]uint32 // live connection → the record holding it
	started bool
}

type dialManagerConfig struct {
	Store    *RunnerDialStore
	Keys     *RunnerKeyStore
	Registry *Registry
	// Dial makes one connection (Server.dialWithOrigin): ECDH + greeting, then
	// the ordinary connection path, which reports through origin.
	Dial func(ctx context.Context, cid objproto.ConnectionID, origin *DialOrigin) error
	// SetLink marks an ADOPTED connection's registry entry as this record's.
	SetLink func(cid objproto.ConnectionID, link protocol.RunnerLink, record uint32)
	Logger  *slog.Logger

	// Defaults: cli.ResolveCandidate, a context-aware sleep, 500 ms, 30 s.
	Resolve        func(spec string) (objproto.ConnectionID, error)
	Sleep          func(ctx context.Context, d time.Duration) bool
	BackoffInitial time.Duration
	BackoffMax     time.Duration

	// OnStart, when set, runs as Start begins. Tests use it to check WHEN the
	// manager starts (after WAL replay), wherever Start is called from.
	OnStart func()
}

type dialLoop struct {
	rec  RunnerDialRecord
	stop chan struct{}

	// guarded by dialManager.mu
	state       protocol.RunnerDialState
	conn        objproto.ConnectionID
	hasConn     bool
	gone        chan struct{} // closed when the live connection ends
	runner      protocol.RunnerID
	answered    int // index of the candidate that answered; -1 none
	nextAttempt time.Time
	lastErr     string
	dupOf       uint32
	exited      bool
	// addrs are the candidates as the last attempt resolved them. Kick
	// matches a record by address against these, so it never resolves a
	// name itself (a stalled lookup would hold the kicking client's requests).
	addrs []objproto.ConnectionID
}

func newDialManager(cfg dialManagerConfig) *dialManager {
	if cfg.Resolve == nil {
		cfg.Resolve = cli.ResolveCandidate
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		}
	}
	if cfg.BackoffInitial == 0 {
		cfg.BackoffInitial = 500 * time.Millisecond
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &dialManager{cfg: cfg, loops: map[uint32]*dialLoop{}, owner: map[objproto.ConnectionID]uint32{}}
}

// Start begins dialing every stored record. The server calls it once WAL
// replay is done: a runner registered before the held tasks are known would be
// told none of its children survive.
func (m *dialManager) Start(ctx context.Context) {
	if m.cfg.OnStart != nil {
		m.cfg.OnStart()
	}
	m.mu.Lock()
	m.ctx, m.started = ctx, true
	m.mu.Unlock()
	for _, rec := range m.cfg.Store.List() {
		m.Add(rec)
	}
}

// Add starts rec's loop (a no-op before Start, which starts every stored record,
// and for a record that already has one).
func (m *dialManager) Add(rec RunnerDialRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return
	}
	if _, ok := m.loops[rec.ID]; ok {
		return
	}
	l := &dialLoop{rec: rec, stop: make(chan struct{}), state: protocol.RunnerDialState_Dialing, answered: -1}
	m.loops[rec.ID] = l
	go m.run(m.ctx, l)
}

// Stop ends one record's loop and forgets it. Its live connection stays (D17),
// and stops naming the record: its link becomes dialed_out, which is what it
// is now — a connection the server made that no record holds.
func (m *dialManager) Stop(id uint32) {
	m.mu.Lock()
	l, ok := m.loops[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	close(l.stop)
	delete(m.loops, id)
	var held []objproto.ConnectionID
	for cid, owner := range m.owner {
		if owner == id {
			delete(m.owner, cid)
			held = append(held, cid)
		}
	}
	m.mu.Unlock()
	if m.cfg.SetLink != nil {
		for _, cid := range held {
			m.cfg.SetLink(cid, protocol.RunnerLink_DialedOut, 0)
		}
	}
}

// current reports whether l is still the live loop for its record.
func (m *dialManager) current(l *dialLoop) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loops[l.rec.ID] == l
}

func (m *dialManager) running(id uint32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.loops[id]
	return ok && !l.exited
}

// onConnRemoved: a registry entry went away. If a record held that
// connection, its loop wakes.
func (m *dialManager) onConnRemoved(cid objproto.ConnectionID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connGoneLocked(cid)
}

func (m *dialManager) connGoneLocked(cid objproto.ConnectionID) {
	id, ok := m.owner[cid]
	if !ok {
		return
	}
	delete(m.owner, cid)
	if l, ok := m.loops[id]; ok && l.hasConn && l.conn == cid {
		l.hasConn = false
		if l.gone != nil {
			close(l.gone)
			l.gone = nil
		}
	}
}

// sameAddr compares what identifies where a connection goes; the ID part is
// drawn at random per dial.
func sameAddr(a, b objproto.ConnectionID) bool {
	return a.Transport == b.Transport && a.Addr == b.Addr
}

// RecordsPointingAt names every record that would bring e back (spec §2 step
// 3): it holds e's connection, its last registration was e's identity, or one
// of its candidates resolves to e's address.
func (m *dialManager) RecordsPointingAt(e RunnerEntry) []uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var zero protocol.RunnerID
	var out []uint32
	for id, l := range m.loops {
		hit := l.hasConn && l.conn == e.ID || l.runner != zero && l.runner == e.Identity
		for _, a := range l.addrs {
			if hit {
				break
			}
			hit = sameAddr(a, e.ID)
		}
		if hit {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// States is the `runner dial ls` listing, in the store's order.
func (m *dialManager) States() []protocol.RunnerDialRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	recs := make([]RunnerDialRecord, 0, len(m.loops))
	for _, l := range m.loops {
		recs = append(recs, l.rec)
	}
	sortDialRecords(recs)
	rows := make([]protocol.RunnerDialRow, 0, len(recs))
	for _, rec := range recs {
		l := m.loops[rec.ID]
		var r protocol.RunnerDialRow
		r.Id = rec.ID
		r.SetLabel([]byte(rec.Label))
		copy(r.Key[:], rec.Key)
		cs := make([]protocol.RunnerDialCandidate, len(rec.Candidates))
		for i, c := range rec.Candidates {
			cs[i].SetText([]byte(c))
		}
		r.SetCandidates(cs)
		r.DialState = l.state
		r.Runner = l.runner
		r.Answered = 0xff
		if l.state == protocol.RunnerDialState_Connected && l.answered >= 0 {
			r.Answered = uint8(l.answered)
		}
		if l.state == protocol.RunnerDialState_Waiting {
			r.NextAttemptUnixMs = uint64(l.nextAttempt.UnixMilli())
		}
		r.SetLastError([]byte(l.lastErr))
		r.AddedAtUnixMs = uint64(rec.AddedAt.UnixMilli())
		r.SetAddedBy([]byte(rec.AddedBy))
		r.DuplicateOf = l.dupOf
		rows = append(rows, r)
	}
	return rows
}

func (m *dialManager) set(l *dialLoop, f func()) {
	m.mu.Lock()
	f()
	m.mu.Unlock()
}

// alive reports whether l should keep going: not stopped, server not down,
// and not yielded to another record.
func (m *dialManager) alive(ctx context.Context, l *dialLoop) bool {
	select {
	case <-l.stop:
		return false
	case <-ctx.Done():
		return false
	default:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return l.state != protocol.RunnerDialState_Duplicate
}

func (m *dialManager) run(ctx context.Context, l *dialLoop) {
	defer m.set(l, func() { l.exited = true })
	sleepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-l.stop:
			cancel()
		case <-sleepCtx.Done():
		}
	}()

	backoff := m.cfg.BackoffInitial
	for m.alive(ctx, l) {
		connected, errText := m.attempt(ctx, sleepCtx, l)
		if connected {
			m.waitGone(sleepCtx, l)
			backoff = m.cfg.BackoffInitial
			errText = "connection ended"
		}
		if !m.alive(ctx, l) {
			return
		}
		m.set(l, func() {
			l.state = protocol.RunnerDialState_Waiting
			l.nextAttempt = time.Now().Add(backoff)
			l.lastErr = errText
		})
		if !m.cfg.Sleep(sleepCtx, backoff) {
			return
		}
		backoff = min(backoff*2, m.cfg.BackoffMax)
	}
}

func (m *dialManager) waitGone(ctx context.Context, l *dialLoop) {
	m.mu.Lock()
	gone := l.gone
	m.mu.Unlock()
	if gone == nil {
		return
	}
	select {
	case <-gone:
	case <-ctx.Done():
	}
}

type resolvedCandidate struct {
	index int
	cid   objproto.ConnectionID
}

// attempt is one pass: adopt the runner if it is already here, else walk the
// candidates until one completes ECDH and see whether it registers.
// connected=true leaves l holding a live connection; otherwise errText says why.
//
// rootCtx is the SERVER's context and is what a dialed connection lives under;
// ctx is this loop's, ended by Stop. A connection must not die with the loop:
// `runner dial rm` stops re-dialing and leaves the runner connected (D17), and
// runner kick stops the loop before its leave has reached the runner.
func (m *dialManager) attempt(rootCtx, ctx context.Context, l *dialLoop) (connected bool, errText string) {
	rec := l.rec
	if _, ok := m.cfg.Keys.Lookup(rec.Key); !ok {
		return false, "key not enrolled"
	}
	var resolved []resolvedCandidate
	lastErr := ""
	for i, c := range rec.Candidates {
		cid, err := m.cfg.Resolve(c)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		resolved = append(resolved, resolvedCandidate{i, cid})
	}
	addrs := make([]objproto.ConnectionID, len(resolved))
	for i, rc := range resolved {
		addrs[i] = rc.cid
	}
	m.set(l, func() { l.addrs = addrs })
	if len(resolved) == 0 {
		return false, lastErr
	}

	if adopted, yielded := m.adoptOrYield(l, resolved); yielded {
		return false, ""
	} else if adopted {
		return true, ""
	}

	m.set(l, func() { l.state = protocol.RunnerDialState_Dialing })
	for _, rc := range resolved {
		att := newDialAttempt()
		origin := &DialOrigin{
			Link: protocol.RunnerLink_DialRecord, RecordID: rec.ID, ExpectKey: rec.Key,
			OnRegistered: func(id protocol.RunnerID) {
				if !m.current(l) {
					// The record was removed while this connection was
					// still registering: it must not claim the record.
					if e, ok := m.cfg.Registry.GetByIdentity(id); ok && m.cfg.SetLink != nil {
						m.cfg.SetLink(e.ID, protocol.RunnerLink_DialedOut, 0)
					}
				}
				att.registered(id)
			},
			OnRefused: att.refused,
			OnClosed: func() {
				att.closed()
				m.onConnRemoved(rc.cid)
			},
		}
		if err := m.cfg.Dial(rootCtx, rc.cid, origin); err != nil {
			lastErr = err.Error()
			continue
		}
		// ECDH done: the walk ends here, whatever registration says.
		select {
		case id := <-att.regCh:
			cid := rc.cid
			if e, ok := m.cfg.Registry.GetByIdentity(id); ok {
				cid = e.ID // the registry's own id for the connection
			}
			m.connected(l, cid, rc.index, id)
			m.mu.Lock()
			// It may have ended between registering and here.
			if _, still := m.cfg.Registry.Get(cid); !still {
				m.connGoneLocked(cid)
			}
			m.mu.Unlock()
			m.cfg.Logger.Info("dial record connected", "record", FormatDialRecordID(rec.ID),
				"candidate", rec.Candidates[rc.index], "runner", id.Hex())
			return true, ""
		case reason := <-att.refCh:
			return false, reason
		case <-att.closeCh:
			return false, "closed before registering"
		case <-ctx.Done():
			return false, ""
		}
	}
	return false, lastErr
}

// adoptOrYield: is the runner already connected at one of this record's
// addresses on its key? Free → adopt it (D20); owned by another record → yield
// to that record, without dialing a runner it holds.
func (m *dialManager) adoptOrYield(l *dialLoop, resolved []resolvedCandidate) (adopted, yielded bool) {
	for _, e := range m.cfg.Registry.List() {
		if !e.Key.Equal(l.rec.Key) {
			continue
		}
		for _, rc := range resolved {
			if !sameAddr(e.ID, rc.cid) {
				continue
			}
			m.mu.Lock()
			if other, owned := m.owner[e.ID]; owned && other != l.rec.ID {
				l.state = protocol.RunnerDialState_Duplicate
				l.dupOf = other
				m.mu.Unlock()
				m.cfg.Logger.Warn("dial record yields: another record holds this runner",
					"record", FormatDialRecordID(l.rec.ID), "holder", FormatDialRecordID(other))
				return false, true
			}
			m.mu.Unlock()
			// e.ID, not rc.cid: the candidate was just re-resolved with a fresh
			// random id, and only transport and address matched.
			m.connected(l, e.ID, rc.index, e.Identity)
			if m.cfg.SetLink != nil {
				m.cfg.SetLink(e.ID, protocol.RunnerLink_DialRecord, l.rec.ID)
			}
			m.mu.Lock()
			// It may have gone between List and here.
			if _, still := m.cfg.Registry.Get(e.ID); !still {
				m.connGoneLocked(e.ID)
			}
			m.mu.Unlock()
			m.cfg.Logger.Info("dial record adopted a connected runner", "record", FormatDialRecordID(l.rec.ID),
				"runner", e.Identity.Hex())
			return true, false
		}
	}
	return false, false
}

// connected records that l holds rc's connection to runner id, and makes every
// OTHER record that last held this runner yield: its connection was displaced
// by this one, and dialing back would take the runner again (D20).
func (m *dialManager) connected(l *dialLoop, cid objproto.ConnectionID, index int, id protocol.RunnerID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l.state = protocol.RunnerDialState_Connected
	l.conn, l.hasConn, l.gone = cid, true, make(chan struct{})
	l.runner, l.answered, l.lastErr = id, index, ""
	if m.loops[l.rec.ID] != l {
		return // removed meanwhile: it owns nothing
	}
	m.owner[cid] = l.rec.ID
	for oid, o := range m.loops {
		if oid == l.rec.ID || o.runner != id || o.state == protocol.RunnerDialState_Duplicate {
			continue
		}
		o.state, o.dupOf = protocol.RunnerDialState_Duplicate, l.rec.ID
		m.cfg.Logger.Warn("dial record yields: its runner was taken by another record",
			"record", FormatDialRecordID(oid), "holder", FormatDialRecordID(l.rec.ID))
	}
}

// dialAttempt turns the origin callbacks into channels; each fires once.
type dialAttempt struct {
	regCh   chan protocol.RunnerID
	refCh   chan string
	closeCh chan struct{}
	once    sync.Once
}

func newDialAttempt() *dialAttempt {
	return &dialAttempt{regCh: make(chan protocol.RunnerID, 1), refCh: make(chan string, 1), closeCh: make(chan struct{})}
}

func (a *dialAttempt) registered(id protocol.RunnerID) {
	select {
	case a.regCh <- id:
	default:
	}
}

func (a *dialAttempt) refused(reason string) {
	select {
	case a.refCh <- reason:
	default:
	}
}

func (a *dialAttempt) closed() { a.once.Do(func() { close(a.closeCh) }) }

// FormatDialRecordID is how a record id is shown: 8 lower hex.
func FormatDialRecordID(id uint32) string {
	const hexdig = "0123456789abcdef"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = hexdig[id&0xf]
		id >>= 4
	}
	return string(b)
}
