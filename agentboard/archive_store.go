package agentboard

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// An archive is a topic's history frozen into a file nothing appends to: the
// log format of a topic log, under <store dir>/archive/<id>.log, whose header
// names the source topic, the seq it runs through, when and by whom it was
// made, and how many messages it holds. A subdirectory, because sweepOrphans
// deletes every *.log directly under the store directory that no retained
// topic owns. See docs/superpowers/specs/2026-11-02-agentboard-flush-archive-design.md.

// ErrArchiveEmpty is an archive of a topic that holds no message.
var ErrArchiveEmpty = errors.New("agentboard: nothing to archive")

// ArchiveInfo is one archive as its header and the file size describe it.
type ArchiveInfo struct {
	ID        string
	Topic     string
	Through   uint64 // the highest seq the archive holds
	Count     int    // messages in it, live and withdrawn
	CreatedAt time.Time
	CreatedBy protocol.TaskID // zero = the operator
	Bytes     int64
}

const archiveDirName = "archive"

// archiveIDRe is the id's form. Checked before any path is built from an id,
// so a request cannot name a file outside the archive directory.
var archiveIDRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

func (s *FileStore) archiveDir() string { return filepath.Join(s.dir, archiveDirName) }

func (s *FileStore) archivePath(id string) (string, bool) {
	if !archiveIDRe.MatchString(id) {
		return "", false
	}
	return filepath.Join(s.archiveDir(), id+".log"), true
}

// newArchiveIDLocked returns an id no archive uses yet.
func (s *FileStore) newArchiveIDLocked() (string, string, error) {
	for {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", "", err
		}
		id := hex.EncodeToString(b[:])
		path, _ := s.archivePath(id)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return id, path, nil
		} else if err != nil {
			return "", "", err
		}
	}
}

func archiveHeader(info ArchiveInfo) []byte {
	rec := storeRecord{T: "hdr", V: storeVersion, Topic: info.Topic, Archive: info.ID,
		Seq: info.Through, AtMs: info.CreatedAt.UnixMilli(), Count: info.Count}
	if info.CreatedBy != (protocol.TaskID{}) {
		rec.ByTask = hex.EncodeToString(info.CreatedBy.Id[:])
	}
	raw, _ := json.Marshal(rec)
	return append(raw, '\n')
}

func recordLine(rec storeRecord) []byte {
	raw, _ := json.Marshal(rec)
	return append(raw, '\n')
}

// ArchiveLog freezes a retained topic's log: every message up to the highest
// seq it holds, with its withdrawal as it stands. The store's lock is held
// throughout, so a publish racing it is either wholly in the archive or not
// in it.
func (s *FileStore) ArchiveLog(topic string, by protocol.TaskID, at time.Time) (ArchiveInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.logs[topic]; ok {
		if err := l.w.Flush(); err != nil {
			return ArchiveInfo{}, err
		}
	}
	src := filepath.Join(s.dir, storeFileName(topic))
	meta, err := scanMeta(src, topic)
	if err != nil {
		return ArchiveInfo{}, err
	}
	if len(meta.msgs) == 0 {
		return ArchiveInfo{}, ErrArchiveEmpty
	}
	info := ArchiveInfo{Topic: topic, Count: len(meta.msgs), CreatedAt: at, CreatedBy: by}
	for seq := range meta.msgs {
		if seq > info.Through {
			info.Through = seq
		}
	}
	return s.writeArchiveLocked(info, func(w io.Writer) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		var werr error
		if err := eachLine(f, func(line []byte) {
			var rec metaRecord
			if json.Unmarshal(line, &rec) != nil || (rec.T != "msg" && rec.T != "retract") || !meta.msgs[rec.Seq] {
				return
			}
			if werr == nil {
				_, werr = w.Write(append(line, '\n'))
			}
		}); err != nil {
			return err
		}
		return werr
	})
}

// ArchiveMessages freezes msgs, a topic's in-memory history, withdrawn
// entries carrying their withdrawal.
func (s *FileStore) ArchiveMessages(topic string, msgs []RetainedMessage, by protocol.TaskID, at time.Time) (ArchiveInfo, error) {
	if len(msgs) == 0 {
		return ArchiveInfo{}, ErrArchiveEmpty
	}
	info := ArchiveInfo{Topic: topic, Count: len(msgs), CreatedAt: at, CreatedBy: by}
	for _, m := range msgs {
		if m.Seq > info.Through {
			info.Through = m.Seq
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeArchiveLocked(info, func(w io.Writer) error {
		for _, m := range msgs {
			if _, err := w.Write(recordLine(msgRecord(m))); err != nil {
				return err
			}
			if !m.RetractedAt.IsZero() {
				if _, err := w.Write(recordLine(retractRecord(m))); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// writeArchiveLocked creates a new archive: header, then what body writes.
func (s *FileStore) writeArchiveLocked(info ArchiveInfo, body func(io.Writer) error) (ArchiveInfo, error) {
	if err := os.MkdirAll(s.archiveDir(), 0o755); err != nil {
		return ArchiveInfo{}, err
	}
	id, path, err := s.newArchiveIDLocked()
	if err != nil {
		return ArchiveInfo{}, err
	}
	info.ID = id
	if err := rewriteFile(path, func(w io.Writer) error {
		if _, err := w.Write(archiveHeader(info)); err != nil {
			return err
		}
		return body(w)
	}); err != nil {
		return ArchiveInfo{}, err
	}
	if st, err := os.Stat(path); err == nil {
		info.Bytes = st.Size()
	}
	return info, nil
}

// readArchiveInfo reads an archive's header and size.
func readArchiveInfo(path string) (ArchiveInfo, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ArchiveInfo{}, false, nil
	}
	if err != nil {
		return ArchiveInfo{}, false, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ArchiveInfo{}, false, err
	}
	var rec metaRecord
	if err := json.Unmarshal(line, &rec); err != nil || rec.T != "hdr" || rec.Archive == "" {
		return ArchiveInfo{}, false, errors.New("agentboard store: " + filepath.Base(path) + ": no archive header")
	}
	info := ArchiveInfo{ID: rec.Archive, Topic: rec.Topic, Through: rec.Seq, Count: rec.Count,
		CreatedAt: time.UnixMilli(rec.AtMs), CreatedBy: taskFromHex(rec.ByTask)}
	if st, err := f.Stat(); err == nil {
		info.Bytes = st.Size()
	}
	return info, true, nil
}

// Archives lists every archive. One that cannot be read is skipped and
// logged; it never hides the rest.
func (s *FileStore) Archives() ([]ArchiveInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.archiveDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ArchiveInfo
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".log")
		path, valid := s.archivePath(id)
		if e.IsDir() || !ok || !valid {
			continue
		}
		info, found, err := readArchiveInfo(path)
		if err != nil {
			storeLog("archive list", id, err)
			continue
		}
		if found {
			out = append(out, info)
		}
	}
	return out, nil
}

func (s *FileStore) ArchiveOf(id string) (ArchiveInfo, bool, error) {
	path, ok := s.archivePath(id)
	if !ok {
		return ArchiveInfo{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return readArchiveInfo(path)
}

func (s *FileStore) ArchivePage(id string, beforeSeq uint64, limit int) ([]RetainedMessage, error) {
	path, ok := s.archivePath(id)
	if !ok {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, found, err := readArchiveInfo(path)
	if err != nil || !found {
		return nil, err
	}
	return pageFile(path, info.Topic, beforeSeq, limit)
}

func (s *FileStore) ArchiveRemove(id string, seq uint64) (bool, error) {
	path, ok := s.archivePath(id)
	if !ok {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, found, err := readArchiveInfo(path)
	if err != nil || !found {
		return false, err
	}
	if seq == 0 {
		return true, os.Remove(path)
	}
	meta, err := scanMeta(path, info.Topic)
	if err != nil || !meta.msgs[seq] {
		return false, err
	}
	info.Count--
	err = rewriteFile(path, func(w io.Writer) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := w.Write(archiveHeader(info)); err != nil {
			return err
		}
		var werr error
		if err := eachLine(f, func(line []byte) {
			var rec metaRecord
			if json.Unmarshal(line, &rec) == nil && (rec.T == "hdr" || (rec.Seq == seq && (rec.T == "msg" || rec.T == "retract"))) {
				return
			}
			if werr == nil {
				_, werr = w.Write(append(line, '\n'))
			}
		}); err != nil {
			return err
		}
		return werr
	})
	return err == nil, err
}
