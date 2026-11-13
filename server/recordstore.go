package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RecordStore keeps the PTY records of runs (pillar A spec, A-D23..A-D26)
// under one directory:
//
//	<Dir>/<task-id>/<run>/NNNNNNNN.seg     the open segment, appended to
//	<Dir>/<task-id>/<run>/NNNNNNNN.seg.gz  a closed segment
//	<Dir>/<task-id>/<run>/meta.json        recordMeta
//
// A segment is RecordEntry after RecordEntry. It closes — and is gzipped then
// — at a rotation, a switch-off and the run's end; the startup pass closes
// the open segment of every run that was not held. Every segment after a
// record's first starts with a head the caller supplies (A-D23), so dropping
// old segments never leaves a record that cannot be played.
type RecordStore struct {
	mu   sync.Mutex
	cfg  RecordConfig
	runs map[string]map[uint32]*recordRun
}

// RecordConfig is the store's configuration (server flags --record-run-max,
// --record-total-max, --record-retention).
type RecordConfig struct {
	Dir       string
	RunMax    int64         // entry bytes kept per run; a segment is RunMax/8
	TotalMax  int64         // bytes on disk across the store
	Retention time.Duration // default for a new record; 0 = forever
}

// recordFormat is the version recordMeta.Format carries. A meta file without
// one is format 1: segments of protocol.RecordEntry.
const recordFormat = 1

type recordMeta struct {
	Format    int    `json:"format"`
	Run       uint32 `json:"run"`
	StartedNs int64  `json:"started_ns"`
	// EndedNs is when recording ended (a switch-off, or the run's end);
	// RunEndedNs when the run itself ended, which retention counts from
	// (A-D26) — a switched-off record of a run still going never expires.
	EndedNs         int64 `json:"ended_ns,omitempty"`
	RunEndedNs      int64 `json:"run_ended_ns,omitempty"`
	EndReason       uint8 `json:"end_reason,omitempty"`
	TruncatedFromNs int64 `json:"truncated_from_ns,omitempty"`
	Forever         bool  `json:"forever,omitempty"`
	TTLMs           int64 `json:"ttl_ms,omitempty"`
	// Closed segments, so a restart need not decompress them to learn their
	// sizes and first entry time.
	Segments []segmentMeta `json:"segments,omitempty"`
}

type segmentMeta struct {
	Seq        int   `json:"seq"`
	EntryBytes int64 `json:"entry_bytes"`
	DiskBytes  int64 `json:"disk_bytes"`
	FirstTs    int64 `json:"first_ts"`
}

type recordSegment struct {
	seq        int
	closed     bool
	entryBytes int64
	diskBytes  int64 // closed: the .gz size; open: entryBytes
	firstTs    int64
}

type recordRun struct {
	task   string
	meta   recordMeta
	segs   []*recordSegment // by seq
	writer *RunWriter       // non-nil while a session records into it
}

// NewRecordStore loads every record under cfg.Dir.
func NewRecordStore(cfg RecordConfig) (*RecordStore, error) {
	if cfg.RunMax <= 0 {
		cfg.RunMax = 64 << 20
	}
	if cfg.TotalMax <= 0 {
		cfg.TotalMax = 1 << 30
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	s := &RecordStore{cfg: cfg, runs: map[string]map[uint32]*recordRun{}}
	tasks, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, err
	}
	for _, td := range tasks {
		if !td.IsDir() || !isTaskHex(td.Name()) {
			continue
		}
		runs, err := os.ReadDir(filepath.Join(cfg.Dir, td.Name()))
		if err != nil {
			return nil, err
		}
		for _, rd := range runs {
			n, err := strconv.ParseUint(rd.Name(), 10, 32)
			if !rd.IsDir() || err != nil || n == 0 {
				continue
			}
			rr, err := s.loadRun(td.Name(), uint32(n))
			if err != nil {
				slog.Warn("record: skipping unreadable run", "task", td.Name(), "run", n, "err", err)
				continue
			}
			s.runsOf(td.Name())[uint32(n)] = rr
		}
	}
	return s, nil
}

func isTaskHex(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (s *RecordStore) runsOf(task string) map[uint32]*recordRun {
	m := s.runs[task]
	if m == nil {
		m = map[uint32]*recordRun{}
		s.runs[task] = m
	}
	return m
}

func (s *RecordStore) runDir(task string, run uint32) string {
	return filepath.Join(s.cfg.Dir, task, strconv.FormatUint(uint64(run), 10))
}

func segName(seq int, closed bool) string {
	if closed {
		return fmt.Sprintf("%08d.seg.gz", seq)
	}
	return fmt.Sprintf("%08d.seg", seq)
}

func (s *RecordStore) segPath(task string, run uint32, seg *recordSegment) string {
	return filepath.Join(s.runDir(task, run), segName(seg.seq, seg.closed))
}

func (s *RecordStore) loadRun(task string, run uint32) (*recordRun, error) {
	dir := s.runDir(task, run)
	rr := &recordRun{task: task, meta: recordMeta{Format: recordFormat, Run: run}}
	if b, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		// An unreadable meta costs what it held (times, retention), not the
		// run: the segments are rebuilt from the files (review I5).
		if err := json.Unmarshal(b, &rr.meta); err != nil {
			slog.Warn("record: unreadable meta.json; rebuilding from the segments", "task", task, "run", run, "err", err)
			rr.meta = recordMeta{Format: recordFormat, Run: run}
		}
		rr.meta.Run = run
	}
	known := map[int]segmentMeta{}
	for _, sm := range rr.meta.Segments {
		known[sm.Seq] = sm
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	// A crash between a segment's rename to .seg.gz and its .seg's removal
	// leaves both. The .gz is complete — it gets its name only once fully
	// written — so it wins and the .seg goes (review I3).
	gz := map[string]bool{}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".seg.gz") {
			gz[strings.TrimSuffix(e.Name(), ".gz")] = true
		}
	}
	for _, e := range ents {
		name := e.Name()
		closed := strings.HasSuffix(name, ".seg.gz")
		if !closed && !strings.HasSuffix(name, ".seg") {
			continue
		}
		if !closed && gz[name] {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				slog.Warn("record: cannot remove a segment's leftover .seg", "task", task, "run", run, "err", err)
			}
			continue
		}
		seq, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".seg"))
		if err != nil {
			continue
		}
		seg := &recordSegment{seq: seq, closed: closed}
		if sm, ok := known[seq]; ok && closed {
			seg.entryBytes, seg.diskBytes, seg.firstTs = sm.EntryBytes, sm.DiskBytes, sm.FirstTs
		} else {
			sc, err := scanSegment(filepath.Join(dir, name), closed)
			if err != nil {
				// One unreadable segment costs itself, not the run (review I5).
				slog.Warn("record: skipping an unreadable segment", "task", task, "run", run, "segment", name, "err", err)
				continue
			}
			seg.entryBytes, seg.firstTs = sc.valid, sc.firstTs
			seg.diskBytes = sc.valid
			if closed {
				if fi, err := os.Stat(filepath.Join(dir, name)); err == nil {
					seg.diskBytes = fi.Size()
				}
			}
		}
		rr.segs = append(rr.segs, seg)
	}
	sort.Slice(rr.segs, func(i, j int) bool { return rr.segs[i].seq < rr.segs[j].seq })
	return rr, nil
}

// segScan is what reading a segment from its start found.
type segScan struct {
	valid   int64 // bytes of whole entries
	firstTs int64
	lastTs  int64
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readSegment calls fn for every whole entry of a segment and stops quietly at
// a cut-off last entry, which is what a crash mid-write leaves (Review Focus
// 2). fn returning an error stops the read with that error.
func readSegment(path string, closed bool, fn func(protocol.RecordEntry, int64) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReader(f)
	if closed {
		zr, err := gzip.NewReader(r)
		if errors.Is(err, io.EOF) {
			return nil // an empty file: a .gz a power loss left unwritten
		}
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	cr := &countingReader{r: r}
	for {
		var e protocol.RecordEntry
		if err := e.Read(cr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if err := fn(e, cr.n); err != nil {
			return err
		}
	}
}

func scanSegment(path string, closed bool) (segScan, error) {
	var sc segScan
	err := readSegment(path, closed, func(e protocol.RecordEntry, end int64) error {
		if sc.firstTs == 0 {
			sc.firstTs = int64(e.TsNs)
		}
		sc.lastTs = int64(e.TsNs)
		sc.valid = end
		return nil
	})
	return sc, err
}

func (s *RecordStore) saveMetaLocked(rr *recordRun) error {
	rr.meta.Format = recordFormat
	rr.meta.Segments = rr.meta.Segments[:0]
	for _, seg := range rr.segs {
		if seg.closed {
			rr.meta.Segments = append(rr.meta.Segments, segmentMeta{Seq: seg.seq, EntryBytes: seg.entryBytes, DiskBytes: seg.diskBytes, FirstTs: seg.firstTs})
		}
	}
	b, err := json.MarshalIndent(rr.meta, "", "  ")
	if err != nil {
		return err
	}
	dir := s.runDir(rr.task, rr.meta.Run)
	tmp := filepath.Join(dir, "meta.json.tmp")
	if err := writeFileSync(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "meta.json"))
}

// writeFileSync writes and fsyncs a file before it is renamed into place, so a
// power loss cannot leave the new name on an empty file (review I5).
func writeFileSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// RunWriter appends one run's entries. It is used by one goroutine at a time
// (the session's recorder); the store never takes its lock.
type RunWriter struct {
	s    *RecordStore
	task string
	run  uint32
	mu   sync.Mutex
	f    *os.File
	bw   *bufio.Writer
	seg  *recordSegment // the open segment, nil until one is needed
	gone atomic.Bool    // the record was deleted under it: appends do nothing
	done bool           // detached or closed
}

// OpenRun starts recording a run, or continues one: a run that already has a
// record keeps it, and its open segment, if any, is appended to after cutting
// off a partial last entry (a re-adopting server, Review Focus 1).
func (s *RecordStore) OpenRun(task string, run uint32, now time.Time) (*RunWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.runsOf(task)
	rr := runs[run]
	if rr != nil && rr.writer != nil {
		return nil, fmt.Errorf("record %s#%d is already being written", task, run)
	}
	if rr == nil {
		if err := os.MkdirAll(s.runDir(task, run), 0o700); err != nil {
			return nil, err
		}
		rr = &recordRun{task: task, meta: recordMeta{Format: recordFormat, Run: run, StartedNs: now.UnixNano()}}
		if s.cfg.Retention <= 0 {
			rr.meta.Forever = true
		} else {
			rr.meta.TTLMs = s.cfg.Retention.Milliseconds()
		}
		runs[run] = rr
	}
	rr.meta.EndedNs = 0
	w := &RunWriter{s: s, task: task, run: run}
	if n := len(rr.segs); n > 0 && !rr.segs[n-1].closed {
		seg := rr.segs[n-1]
		path := s.segPath(task, run, seg)
		sc, err := scanSegment(path, false)
		if err != nil {
			return nil, err
		}
		if err := os.Truncate(path, sc.valid); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		seg.entryBytes, seg.diskBytes = sc.valid, sc.valid
		w.f, w.bw, w.seg = f, bufio.NewWriterSize(f, 64<<10), seg
	}
	rr.writer = w
	if err := s.saveMetaLocked(rr); err != nil {
		return nil, err
	}
	return w, nil
}

// SegmentLimit is the entry bytes at which the caller should rotate.
func (w *RunWriter) SegmentLimit() int64 {
	if l := w.s.cfg.RunMax / 8; l > 0 {
		return l
	}
	return 1
}

// Append writes one entry to the open segment, opening one if needed.
func (w *RunWriter) Append(e protocol.RecordEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || w.gone.Load() {
		return nil
	}
	return w.appendLocked(e)
}

func (w *RunWriter) appendLocked(e protocol.RecordEntry) error {
	if w.seg == nil {
		if err := w.openSegmentLocked(); err != nil {
			return err
		}
	}
	b := e.MustAppend(nil)
	if _, err := w.bw.Write(b); err != nil {
		return err
	}
	w.s.mu.Lock()
	w.seg.entryBytes += int64(len(b))
	w.seg.diskBytes = w.seg.entryBytes
	if w.seg.firstTs == 0 {
		w.seg.firstTs = int64(e.TsNs)
	}
	w.s.mu.Unlock()
	return nil
}

func (w *RunWriter) openSegmentLocked() error {
	s := w.s
	s.mu.Lock()
	rr := s.runs[w.task][w.run]
	if rr == nil {
		s.mu.Unlock()
		return fmt.Errorf("record %s#%d is gone", w.task, w.run)
	}
	seq := 0
	if n := len(rr.segs); n > 0 {
		seq = rr.segs[n-1].seq + 1
	}
	seg := &recordSegment{seq: seq}
	path := s.segPath(w.task, w.run, seg)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	rr.segs = append(rr.segs, seg)
	s.mu.Unlock()
	w.f, w.bw, w.seg = f, bufio.NewWriterSize(f, 64<<10), seg
	return nil
}

// Flush makes everything appended so far readable.
func (w *RunWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bw == nil {
		return nil
	}
	return w.bw.Flush()
}

// Rotate closes the open segment and starts one that begins with head (A-D23).
func (w *RunWriter) Rotate(head []protocol.RecordEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || w.gone.Load() {
		return nil
	}
	if err := w.closeSegmentLocked(); err != nil {
		return err
	}
	if err := w.openSegmentLocked(); err != nil {
		return err
	}
	for _, e := range head {
		if err := w.appendLocked(e); err != nil {
			return err
		}
	}
	return nil
}

// closeSegmentLocked flushes and closes the open segment, gzips it (an empty
// one is removed instead), and applies the caps.
func (w *RunWriter) closeSegmentLocked() error {
	if w.seg == nil {
		return nil
	}
	if err := w.bw.Flush(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	seg := w.seg
	w.f, w.bw, w.seg = nil, nil, nil
	s := w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	rr := s.runs[w.task][w.run]
	if rr == nil {
		return nil
	}
	if err := s.closeSegmentFileLocked(rr, seg); err != nil {
		return err
	}
	s.enforceCapsLocked(rr)
	return s.saveMetaLocked(rr)
}

// closeSegmentFileLocked turns an open segment into a closed one on disk,
// cutting off a partial last entry.
func (s *RecordStore) closeSegmentFileLocked(rr *recordRun, seg *recordSegment) error {
	path := s.segPath(rr.task, rr.meta.Run, seg)
	sc, err := scanSegment(path, false)
	if err != nil {
		return err
	}
	if sc.valid == 0 {
		rr.segs = removeSeg(rr.segs, seg)
		return os.Remove(path)
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	seg.closed = true
	gzPath := s.segPath(rr.task, rr.meta.Run, seg)
	tmp := gzPath + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		seg.closed = false
		return err
	}
	zw := gzip.NewWriter(dst)
	if _, err := io.Copy(zw, io.LimitReader(src, sc.valid)); err != nil {
		dst.Close()
		seg.closed = false
		return err
	}
	if err := zw.Close(); err != nil {
		dst.Close()
		seg.closed = false
		return err
	}
	err = dst.Sync()
	var fi os.FileInfo
	if err == nil {
		fi, err = dst.Stat()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		seg.closed = false
		return err
	}
	if err := os.Rename(tmp, gzPath); err != nil {
		seg.closed = false
		return err
	}
	seg.entryBytes, seg.diskBytes, seg.firstTs = sc.valid, fi.Size(), sc.firstTs
	return os.Remove(path)
}

func removeSeg(segs []*recordSegment, seg *recordSegment) []*recordSegment {
	out := segs[:0]
	for _, x := range segs {
		if x != seg {
			out = append(out, x)
		}
	}
	return out
}

// enforceCapsLocked drops the oldest segments past the caps (A-D25): first
// the run's own past RunMax, then the oldest closed segment on the whole
// store past TotalMax. The open segment is never dropped.
func (s *RecordStore) enforceCapsLocked(rr *recordRun) {
	total := func(r *recordRun) (n int64) {
		for _, seg := range r.segs {
			n += seg.entryBytes
		}
		return n
	}
	for total(rr) > s.cfg.RunMax && len(rr.segs) > 1 && rr.segs[0].closed {
		s.dropSegmentLocked(rr, rr.segs[0])
	}
	for s.diskTotalLocked() > s.cfg.TotalMax {
		var victim *recordSegment
		var owner *recordRun
		for _, runs := range s.runs {
			for _, r := range runs {
				for _, seg := range r.segs {
					if seg.closed && (victim == nil || seg.firstTs < victim.firstTs) {
						victim, owner = seg, r
					}
				}
			}
		}
		if victim == nil {
			return
		}
		s.dropSegmentLocked(owner, victim)
		if owner != rr {
			if err := s.saveMetaLocked(owner); err != nil {
				slog.Warn("record: meta write failed", "task", owner.task, "run", owner.meta.Run, "err", err)
			}
		}
	}
}

// dropSegmentLocked deletes one closed segment. A run left with no segment
// and no writer is deleted whole.
func (s *RecordStore) dropSegmentLocked(rr *recordRun, seg *recordSegment) {
	if err := os.Remove(s.segPath(rr.task, rr.meta.Run, seg)); err != nil && !os.IsNotExist(err) {
		slog.Warn("record: segment delete failed", "task", rr.task, "run", rr.meta.Run, "err", err)
	}
	rr.segs = removeSeg(rr.segs, seg)
	if len(rr.segs) == 0 && rr.writer == nil {
		s.deleteRunLocked(rr)
		return
	}
	if len(rr.segs) > 0 {
		rr.meta.TruncatedFromNs = rr.segs[0].firstTs
	}
}

func (s *RecordStore) deleteRunLocked(rr *recordRun) {
	if rr.writer != nil {
		rr.writer.gone.Store(true)
	}
	if err := os.RemoveAll(s.runDir(rr.task, rr.meta.Run)); err != nil {
		slog.Warn("record: run delete failed", "task", rr.task, "run", rr.meta.Run, "err", err)
	}
	if runs := s.runs[rr.task]; runs != nil {
		delete(runs, rr.meta.Run)
		if len(runs) == 0 {
			delete(s.runs, rr.task)
			_ = os.Remove(filepath.Join(s.cfg.Dir, rr.task))
		}
	}
}

func (s *RecordStore) diskTotalLocked() (n int64) {
	for _, runs := range s.runs {
		for _, r := range runs {
			for _, seg := range r.segs {
				n += seg.diskBytes
			}
		}
	}
	return n
}

// Detach releases the writer without closing its segment: the session's mux
// stopped, and a re-adopting server may continue the run (Review Focus 1).
func (w *RunWriter) Detach() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return nil
	}
	w.done = true
	var err error
	if w.bw != nil {
		err = w.bw.Flush()
		if cerr := w.f.Close(); err == nil {
			err = cerr
		}
		w.f, w.bw = nil, nil
	}
	w.s.mu.Lock()
	if rr := w.s.runs[w.task][w.run]; rr != nil && rr.writer == w {
		rr.writer = nil
	}
	w.s.mu.Unlock()
	return err
}

// Close is a switch-off: the segment is closed and the recording's end is
// stamped; the record stays.
func (w *RunWriter) Close(now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return nil
	}
	w.done = true
	if w.gone.Load() {
		if w.f != nil {
			w.f.Close()
		}
		return nil
	}
	if err := w.closeSegmentLocked(); err != nil {
		return err
	}
	s := w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	rr := s.runs[w.task][w.run]
	if rr == nil {
		return nil
	}
	if rr.writer == w {
		rr.writer = nil
	}
	rr.meta.EndedNs = now.UnixNano()
	return s.saveMetaLocked(rr)
}

// FinishRun stamps a run's end and reason on its record and closes its open
// segment. Called once the session is over; a run being written is left as it
// is (the startup pass closes it if a race left it open). No record = no-op.
func (s *RecordStore) FinishRun(task string, run uint32, ended time.Time, reason protocol.EndReason) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rr := s.runs[task][run]
	if rr == nil {
		return nil
	}
	if rr.writer == nil {
		if n := len(rr.segs); n > 0 && !rr.segs[n-1].closed {
			if err := s.closeSegmentFileLocked(rr, rr.segs[n-1]); err != nil {
				return err
			}
			s.enforceCapsLocked(rr)
			if s.runs[task][run] == nil {
				return nil
			}
		}
	}
	if rr.meta.EndedNs == 0 {
		rr.meta.EndedNs = ended.UnixNano()
	}
	rr.meta.RunEndedNs = ended.UnixNano()
	rr.meta.EndReason = uint8(reason)
	return s.saveMetaLocked(rr)
}

// CompressLeftovers is the startup pass: every open segment whose run is not
// held is closed, and a record with no end gets its last entry's time.
func (s *RecordStore) CompressLeftovers(isHeld func(task string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for task, runs := range s.runs {
		if isHeld(task) {
			continue
		}
		for _, rr := range runs {
			if rr.writer != nil {
				continue
			}
			changed := false
			if n := len(rr.segs); n > 0 && !rr.segs[n-1].closed {
				seg := rr.segs[n-1]
				sc, err := scanSegment(s.segPath(task, rr.meta.Run, seg), false)
				if err == nil {
					err = s.closeSegmentFileLocked(rr, seg)
				}
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				if rr.meta.EndedNs == 0 {
					rr.meta.EndedNs = sc.lastTs
				}
				changed = true
			}
			// A run not held across this restart cannot still be going (a
			// crash fails it, an unheld stop kills it), so its record's run
			// end is known now even if no FinishRun reached it; without one
			// it would never expire (review I6).
			if rr.meta.RunEndedNs == 0 {
				rr.meta.RunEndedNs = rr.meta.EndedNs
				if rr.meta.RunEndedNs == 0 {
					rr.meta.RunEndedNs = time.Now().UnixNano()
				}
				changed = true
			}
			if changed {
				if err := s.saveMetaLocked(rr); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// Read calls fn for each entry of a run's record, oldest first. A run being
// recorded is read up to what its writer has flushed.
func (s *RecordStore) Read(task string, run uint32, fn func(protocol.RecordEntry) error) error {
	s.mu.Lock()
	rr := s.resolveLocked(task, run)
	if rr == nil {
		s.mu.Unlock()
		return os.ErrNotExist
	}
	type segRef struct {
		path   string
		closed bool
	}
	var segs []segRef
	for _, seg := range rr.segs {
		segs = append(segs, segRef{s.segPath(task, rr.meta.Run, seg), seg.closed})
	}
	s.mu.Unlock()
	each := func(e protocol.RecordEntry, _ int64) error { return fn(e) }
	for _, sg := range segs {
		err := readSegment(sg.path, sg.closed, each)
		// The listing is a moment old: an open segment may have been closed
		// (gzipped under a new name) since, and a closed one evicted. Neither
		// fails a live read (review I1).
		if errors.Is(err, os.ErrNotExist) && !sg.closed {
			err = readSegment(sg.path+".gz", true, each)
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// resolveLocked finds a run; run 0 is the task's latest.
func (s *RecordStore) resolveLocked(task string, run uint32) *recordRun {
	runs := s.runs[task]
	if run != 0 {
		return runs[run]
	}
	var latest *recordRun
	for _, rr := range runs {
		if latest == nil || rr.meta.Run > latest.meta.Run {
			latest = rr
		}
	}
	return latest
}

// List returns each run's record of task, or of every task when task is "".
func (s *RecordStore) List(task string) []protocol.RecordInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.RecordInfo
	for t, runs := range s.runs {
		if task != "" && t != task {
			continue
		}
		for _, rr := range runs {
			out = append(out, s.infoLocked(rr))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskId != out[j].TaskId {
			return hex.EncodeToString(out[i].TaskId.Id[:]) < hex.EncodeToString(out[j].TaskId.Id[:])
		}
		return out[i].Run < out[j].Run
	})
	return out
}

func (s *RecordStore) infoLocked(rr *recordRun) protocol.RecordInfo {
	var info protocol.RecordInfo
	if b, err := hex.DecodeString(rr.task); err == nil {
		copy(info.TaskId.Id[:], b)
	}
	info.Run = rr.meta.Run
	info.StartedNs = uint64(rr.meta.StartedNs)
	info.EndedNs = uint64(rr.meta.EndedNs)
	info.EndReason = protocol.EndReason(rr.meta.EndReason)
	info.TruncatedFromNs = uint64(rr.meta.TruncatedFromNs)
	for _, seg := range rr.segs {
		info.EntryBytes += uint64(seg.entryBytes)
		info.DiskBytes += uint64(seg.diskBytes)
	}
	info.SetForever(rr.meta.Forever)
	info.SetRecording(rr.writer != nil)
	if !rr.meta.Forever && rr.meta.RunEndedNs != 0 {
		info.ExpiresNs = uint64(rr.meta.RunEndedNs + rr.meta.TTLMs*int64(time.Millisecond))
	}
	return info
}

// Delete removes one run's record, or every run of task when run is 0. A run
// being recorded stops recording: its writer turns inert. Reports whether
// anything was there.
func (s *RecordStore) Delete(task string, run uint32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.runs[task]
	if len(runs) == 0 {
		return false, nil
	}
	if run == 0 {
		for _, rr := range runs {
			s.deleteRunLocked(rr)
		}
		return true, nil
	}
	rr := runs[run]
	if rr == nil {
		return false, nil
	}
	s.deleteRunLocked(rr)
	return true, nil
}

// Retention reports a run's retention (run 0 = latest).
func (s *RecordStore) Retention(task string, run uint32) (forever bool, ttl time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rr := s.resolveLocked(task, run)
	if rr == nil {
		return false, 0, false
	}
	return rr.meta.Forever, time.Duration(rr.meta.TTLMs) * time.Millisecond, true
}

// SetRetention changes a run's retention (run 0 = latest). A ttl counts from
// the run's end; ttl 0 and not forever expires at the end itself.
func (s *RecordStore) SetRetention(task string, run uint32, forever bool, ttl time.Duration, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rr := s.resolveLocked(task, run)
	if rr == nil {
		return os.ErrNotExist
	}
	rr.meta.Forever = forever
	rr.meta.TTLMs = 0
	if !forever {
		rr.meta.TTLMs = ttl.Milliseconds()
	}
	if err := s.saveMetaLocked(rr); err != nil {
		return err
	}
	s.sweepExpiredLocked(now)
	return nil
}

// RunRetention expires records once a minute until ctx ends.
func (s *RecordStore) RunRetention(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sweepExpired(now)
		}
	}
}

func (s *RecordStore) sweepExpired(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepExpiredLocked(now)
}

// sweepExpiredLocked deletes every record past its retention: ended, not
// forever, not being written, and its end plus ttl reached.
func (s *RecordStore) sweepExpiredLocked(now time.Time) {
	var expired []*recordRun
	for _, runs := range s.runs {
		for _, rr := range runs {
			if rr.writer != nil || rr.meta.Forever || rr.meta.RunEndedNs == 0 {
				continue
			}
			if now.UnixNano() >= rr.meta.RunEndedNs+rr.meta.TTLMs*int64(time.Millisecond) {
				expired = append(expired, rr)
			}
		}
	}
	for _, rr := range expired {
		s.deleteRunLocked(rr)
	}
}
