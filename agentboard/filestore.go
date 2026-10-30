package agentboard

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FileStore is the Store under <data-dir>/board/: retention.json for the
// retention table, and one JSON Lines log per retained topic. Enums are
// stored as numbers and ids as hex, never as wire-encoded structs, so a
// later .bgn change cannot make an old log unreadable (the WAL's lesson).
type FileStore struct {
	dir string

	mu   sync.Mutex
	ret  map[string]Retention
	logs map[string]*topicLog
	// purged holds the seqs Remove was asked for, so a late Append of one —
	// the publish's mirror racing the purge — is dropped instead of putting
	// the bytes back on disk. Process lifetime only: the race is in-process.
	purged map[string]map[uint64]bool

	stop chan struct{}
	done chan struct{}
}

type topicLog struct {
	f *os.File
	w *bufio.Writer
}

const (
	storeVersion       = 1
	storeRetentionFile = "retention.json"
	// storeMaxLine bounds one record. A 1 MiB payload is ~1.4 MiB in base64;
	// the margin covers --agentboard-max-payload being raised.
	storeMaxLine = 64 << 20
)

// storeFileName is the log's filename: a hash, because a topic name may hold
// characters a filesystem rejects. The name itself is in the log's header.
func storeFileName(topic string) string {
	sum := sha256.Sum256([]byte(topic))
	return hex.EncodeToString(sum[:16]) + ".log"
}

// storeRecord is one line of a topic log. "hdr" opens the log; "msg" is a
// published message; "retract" withdraws one; "shown" moves a task's delivery
// mark; "floor" is written by a purge rewrite and carries the highest seq and
// the last publish time the removed records held, so a purge lowers neither.
// Fields not used by a type are omitted.
type storeRecord struct {
	T     string `json:"t"`
	V     int    `json:"v,omitempty"`
	Topic string `json:"topic,omitempty"`

	Seq              uint64 `json:"seq,omitempty"`
	InReplyTo        uint64 `json:"in_reply_to,omitempty"`
	Payload          []byte `json:"payload,omitempty"`
	FromRunner       string `json:"from_runner,omitempty"`
	FromTask         string `json:"from_task,omitempty"`
	FromHostname     string `json:"from_hostname,omitempty"`
	FromAgentProfile string `json:"from_agent_profile,omitempty"`
	SenderKind       uint8  `json:"sender_kind,omitempty"`
	ReplyToTopic     string `json:"reply_to_topic,omitempty"`
	ReceivedAtMs     int64  `json:"received_at_ms,omitempty"`
	NoRetireOnReply  bool   `json:"no_retire_on_reply,omitempty"`
	NoContact        bool   `json:"no_contact,omitempty"`
	ExcludeSelf      bool   `json:"exclude_self,omitempty"`

	AtMs   int64  `json:"at_ms,omitempty"`
	By     uint8  `json:"by,omitempty"`
	ByTask string `json:"by_task,omitempty"`

	Task string `json:"task,omitempty"`
}

type retentionFile struct {
	Version int                        `json:"version"`
	Topics  map[string]retentionRecord `json:"topics"`
}

type retentionRecord struct {
	TTLMs   int64  `json:"ttl_ms,omitempty"`
	Forever bool   `json:"forever,omitempty"`
	SetBy   string `json:"set_by,omitempty"`
	SetAtMs int64  `json:"set_at_ms"`
}

// OpenFileStore opens (creating) dir. Records are buffered and written every
// flushEvery and on Close; flushEvery <= 0 flushes on Close only.
func OpenFileStore(dir string, flushEvery time.Duration) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &FileStore{dir: dir, ret: map[string]Retention{}, logs: map[string]*topicLog{},
		purged: map[string]map[uint64]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	raw, err := os.ReadFile(filepath.Join(dir, storeRetentionFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var rf retentionFile
		if err := json.Unmarshal(raw, &rf); err != nil {
			return nil, fmt.Errorf("agentboard store: %s: %w", storeRetentionFile, err)
		}
		for name, r := range rf.Topics {
			s.ret[name] = Retention{
				TTL: time.Duration(r.TTLMs) * time.Millisecond, Forever: r.Forever,
				SetBy: taskFromHex(r.SetBy), SetAt: time.UnixMilli(r.SetAtMs),
			}
		}
	}
	if flushEvery > 0 {
		go s.flushLoop(flushEvery)
	} else {
		close(s.done)
	}
	return s, nil
}

func (s *FileStore) flushLoop(every time.Duration) {
	defer close(s.done)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			s.mu.Lock()
			s.flushAllLocked()
			s.mu.Unlock()
		}
	}
}

func (s *FileStore) flushAllLocked() {
	for name, l := range s.logs {
		if err := l.w.Flush(); err != nil {
			slog.Error("agentboard store: flush", "topic", name, "err", err)
		}
	}
}

func (s *FileStore) writeRetentionLocked() error {
	rf := retentionFile{Version: storeVersion, Topics: map[string]retentionRecord{}}
	for name, r := range s.ret {
		rec := retentionRecord{Forever: r.Forever, SetAtMs: r.SetAt.UnixMilli()}
		if !r.Forever {
			rec.TTLMs = r.TTL.Milliseconds()
		}
		if r.SetBy != (protocol.TaskID{}) {
			rec.SetBy = hex.EncodeToString(r.SetBy.Id[:])
		}
		rf.Topics[name] = rec
	}
	raw, err := json.Marshal(rf)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, storeRetentionFile), raw)
}

// writeFileAtomic replaces path with data: a crash leaves the old file or the
// new one, never a mix.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func headerLine(topic string) []byte {
	raw, _ := json.Marshal(storeRecord{T: "hdr", V: storeVersion, Topic: topic})
	return append(raw, '\n')
}

// logLocked returns topic's open log, creating the file with its header.
func (s *FileStore) logLocked(topic string) (*topicLog, error) {
	if l, ok := s.logs[topic]; ok {
		return l, nil
	}
	path := filepath.Join(s.dir, storeFileName(topic))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() == 0 {
		if _, err := f.Write(headerLine(topic)); err != nil {
			f.Close()
			return nil, err
		}
	} else if err := cutTornTail(f, st.Size()); err != nil {
		f.Close()
		return nil, err
	}
	l := &topicLog{f: f, w: bufio.NewWriter(f)}
	s.logs[topic] = l
	return l, nil
}

func (s *FileStore) closeLogLocked(topic string) {
	if l, ok := s.logs[topic]; ok {
		_ = l.w.Flush()
		_ = l.f.Close()
		delete(s.logs, topic)
	}
}

func (s *FileStore) appendLocked(topic string, rec storeRecord) error {
	if _, ok := s.ret[topic]; !ok {
		return nil
	}
	l, err := s.logLocked(topic)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = l.w.Write(append(raw, '\n'))
	return err
}

func (s *FileStore) SetRetention(topic string, r Retention) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ret[topic] = r
	if err := s.writeRetentionLocked(); err != nil {
		return err
	}
	_, err := s.logLocked(topic)
	return err
}

func (s *FileStore) Drop(topic string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLogLocked(topic)
	delete(s.ret, topic)
	delete(s.purged, topic)
	if err := os.Remove(filepath.Join(s.dir, storeFileName(topic))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.writeRetentionLocked()
}

func (s *FileStore) Truncate(topic string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLogLocked(topic)
	path := filepath.Join(s.dir, storeFileName(topic))
	p, err := parseFile(path, topic)
	if err != nil {
		return err
	}
	// The purge empties the topic now: its period counts from here.
	out := append(headerLine(topic), floorLine(p.maxSeq, time.Now())...)
	return writeFileAtomic(path, out)
}

func (s *FileStore) Append(topic string, m RetainedMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.purged[topic][m.Seq] {
		return nil
	}
	return s.appendLocked(topic, msgRecord(m))
}

func (s *FileStore) Retract(topic string, m RetainedMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := storeRecord{T: "retract", Seq: m.Seq, AtMs: m.RetractedAt.UnixMilli(), By: uint8(m.RetractedBy)}
	if m.RetractedByTask != (protocol.TaskID{}) {
		rec.ByTask = hex.EncodeToString(m.RetractedByTask.Id[:])
	}
	return s.appendLocked(topic, rec)
}

func (s *FileStore) SetShown(topic string, task protocol.TaskID, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(topic, storeRecord{T: "shown", Task: hex.EncodeToString(task.Id[:]), Seq: seq})
}

func (s *FileStore) Remove(topic string, seq uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.purged[topic] == nil {
		s.purged[topic] = map[uint64]bool{}
	}
	s.purged[topic][seq] = true
	s.closeLogLocked(topic)
	path := filepath.Join(s.dir, storeFileName(topic))
	p, err := parseFile(path, topic)
	if err != nil {
		return false, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var out bytes.Buffer
	found := false
	err = eachLine(f, func(line []byte) {
		var rec storeRecord
		if json.Unmarshal(line, &rec) == nil && rec.Seq == seq && (rec.T == "msg" || rec.T == "retract") {
			found = found || rec.T == "msg"
			return
		}
		out.Write(line)
		out.WriteByte('\n')
	})
	f.Close()
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	// The removed record may have been the newest: keep the floor it set.
	out.Write(floorLine(p.maxSeq, p.lastAt))
	return true, writeFileAtomic(path, out.Bytes())
}

func (s *FileStore) Page(topic string, beforeSeq uint64, limit int) ([]RetainedMessage, error) {
	parsed, err := s.read(topic)
	if err != nil {
		return nil, err
	}
	all := append(parsed.live, parsed.withdrawn...)
	sort.Slice(all, func(i, j int) bool { return all[i].Seq > all[j].Seq })
	out := make([]RetainedMessage, 0, limit)
	for _, m := range all {
		if beforeSeq != 0 && m.Seq >= beforeSeq {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *FileStore) Bytes(topic string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var buffered int64
	if l, ok := s.logs[topic]; ok {
		buffered = int64(l.w.Buffered())
	}
	st, err := os.Stat(filepath.Join(s.dir, storeFileName(topic)))
	if err != nil {
		return buffered
	}
	return st.Size() + buffered
}

func (s *FileStore) Load(ringN int) ([]StoredTopic, error) {
	s.mu.Lock()
	names := make([]string, 0, len(s.ret))
	for name := range s.ret {
		names = append(names, name)
	}
	s.mu.Unlock()
	sort.Strings(names)
	s.sweepOrphans(names)
	out := make([]StoredTopic, 0, len(names))
	for _, name := range names {
		parsed, err := s.read(name)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		r := s.ret[name]
		s.mu.Unlock()
		out = append(out, StoredTopic{
			Name: name, Retention: r,
			Ring: tail(parsed.live, ringN), Retracted: tail(parsed.withdrawn, ringN),
			Shown: parsed.shown, MaxSeq: parsed.maxSeq, LastPublishedAt: parsed.lastAt,
		})
	}
	return out, nil
}

// sweepOrphans deletes every log that belongs to no retained topic — left by
// a Drop that removed the retention but not the file, or by a version before
// writes were refused for unretained topics. retention.json is the authority
// on what is retained; a log it does not name holds bytes no purge can reach.
func (s *FileStore) sweepOrphans(names []string) {
	keep := map[string]bool{}
	for _, n := range names {
		keep[storeFileName(n)] = true
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		slog.Error("agentboard store: list for orphan sweep", "err", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".log" || keep[e.Name()] {
			continue
		}
		slog.Warn("agentboard store: removing a log no retained topic owns", "file", e.Name())
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {
			slog.Error("agentboard store: orphan sweep", "file", e.Name(), "err", err)
		}
	}
}

func (s *FileStore) Close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for name := range s.logs {
		l := s.logs[name]
		errs = append(errs, l.w.Flush(), l.f.Close())
		delete(s.logs, name)
	}
	return errors.Join(errs...)
}

type parsedLog struct {
	live      []RetainedMessage // ascending seq
	withdrawn []RetainedMessage // ascending seq
	shown     map[protocol.TaskID]uint64
	maxSeq    uint64
	lastAt    time.Time
}

// read flushes topic's buffered records and parses its log.
func (s *FileStore) read(topic string) (parsedLog, error) {
	s.mu.Lock()
	if l, ok := s.logs[topic]; ok {
		if err := l.w.Flush(); err != nil {
			s.mu.Unlock()
			return parsedLog{}, err
		}
	}
	s.mu.Unlock()
	return parseFile(filepath.Join(s.dir, storeFileName(topic)), topic)
}

// parseFile parses one topic log. A line that does not decode — a damaged
// record — is skipped with a warning; the rest of the log still loads. A
// missing file is an empty log.
func parseFile(path, topic string) (parsedLog, error) {
	p := parsedLog{shown: map[protocol.TaskID]uint64{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	defer f.Close()
	byseq := map[uint64]RetainedMessage{}
	retracts := map[uint64]storeRecord{}
	raise := func(seq uint64) {
		if seq > p.maxSeq {
			p.maxSeq = seq
		}
	}
	lineNo := 0
	err = eachLine(f, func(line []byte) {
		lineNo++
		var rec storeRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			slog.Warn("agentboard store: skipping unreadable record", "topic", topic, "line", lineNo, "err", err)
			return
		}
		raise(rec.Seq)
		switch rec.T {
		case "msg":
			m := rec.message(topic)
			byseq[m.Seq] = m
			if m.ReceivedAt.After(p.lastAt) {
				p.lastAt = m.ReceivedAt
			}
		case "retract":
			retracts[rec.Seq] = rec
		case "shown":
			p.shown[taskFromHex(rec.Task)] = rec.Seq
		case "floor":
			if at := time.UnixMilli(rec.AtMs); at.After(p.lastAt) {
				p.lastAt = at
			}
		}
	})
	if err != nil {
		return p, err
	}
	for seq, m := range byseq {
		if r, ok := retracts[seq]; ok {
			m.RetractedAt = time.UnixMilli(r.AtMs)
			m.RetractedBy = protocol.RetractedBy(r.By)
			m.RetractedByTask = taskFromHex(r.ByTask)
			p.withdrawn = append(p.withdrawn, m)
			continue
		}
		p.live = append(p.live, m)
	}
	sort.Slice(p.live, func(i, j int) bool { return p.live[i].Seq < p.live[j].Seq })
	sort.Slice(p.withdrawn, func(i, j int) bool { return p.withdrawn[i].Seq < p.withdrawn[j].Seq })
	return p, nil
}

func floorLine(maxSeq uint64, lastAt time.Time) []byte {
	rec := storeRecord{T: "floor", Seq: maxSeq}
	if !lastAt.IsZero() {
		rec.AtMs = lastAt.UnixMilli()
	}
	raw, _ := json.Marshal(rec)
	return append(raw, '\n')
}

// cutTornTail truncates f after its last newline: a record a crash left half
// written is removed before anything is appended behind it, which would
// otherwise fuse the two into one unreadable line — and leave the torn bytes
// where no purge can reach them.
func cutTornTail(f *os.File, size int64) error {
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			if cut := start + int64(i) + 1; cut != size {
				slog.Warn("agentboard store: cutting a torn record", "file", f.Name(), "bytes", size-cut)
				return f.Truncate(cut)
			}
			return nil
		}
		end = start
	}
	// No newline at all: not even the header survived.
	return f.Truncate(0)
}

// eachLine calls fn for every newline-terminated line and for a final
// unterminated one. A bufio.Reader rather than a Scanner: a Scanner fails the
// whole read on one line over its token cap, which is how the WAL once lost
// its entire file.
func eachLine(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\n")
			if len(line) > 0 && len(line) <= storeMaxLine {
				fn(line)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func msgRecord(m RetainedMessage) storeRecord {
	rec := storeRecord{
		T: "msg", Seq: m.Seq, InReplyTo: m.InReplyTo, Payload: m.Payload,
		FromHostname: m.FromHostname, FromAgentProfile: m.FromAgentProfile,
		SenderKind: uint8(m.SenderKind), ReplyToTopic: m.ReplyToTopic,
		ReceivedAtMs: m.ReceivedAt.UnixMilli(), NoRetireOnReply: m.NoRetireOnReply,
		NoContact: m.NoContact, ExcludeSelf: m.ExcludeSelf,
	}
	if m.FromRunner != (protocol.RunnerID{}) {
		rec.FromRunner = hex.EncodeToString(m.FromRunner.Id[:])
	}
	if m.FromTask != (protocol.TaskID{}) {
		rec.FromTask = hex.EncodeToString(m.FromTask.Id[:])
	}
	return rec
}

func (rec storeRecord) message(topic string) RetainedMessage {
	m := RetainedMessage{
		Seq: rec.Seq, InReplyTo: rec.InReplyTo, Topic: topic, Payload: rec.Payload,
		FromTask: taskFromHex(rec.FromTask), FromHostname: rec.FromHostname,
		FromAgentProfile: rec.FromAgentProfile, SenderKind: protocol.SenderKind(rec.SenderKind),
		ReplyToTopic: rec.ReplyToTopic, ReceivedAt: time.UnixMilli(rec.ReceivedAtMs),
		NoRetireOnReply: rec.NoRetireOnReply, NoContact: rec.NoContact, ExcludeSelf: rec.ExcludeSelf,
	}
	if raw, err := hex.DecodeString(rec.FromRunner); err == nil && len(raw) == 16 {
		copy(m.FromRunner.Id[:], raw)
	}
	return m
}

func taskFromHex(s string) protocol.TaskID {
	var t protocol.TaskID
	if raw, err := hex.DecodeString(s); err == nil && len(raw) == 16 {
		copy(t.Id[:], raw)
	}
	return t
}

func tail(ms []RetainedMessage, n int) []RetainedMessage {
	if len(ms) > n {
		return ms[len(ms)-n:]
	}
	return ms
}
