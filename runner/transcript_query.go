package runner

import (
	"archive/tar"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/transcript"
	"github.com/on-keyday/objtrsf/trsf"
)

// Bounds on what a transcript request names (pillar B plan, Global
// Constraints): longer ones are refused, not looked up.
const (
	transcriptSessionMax = 128
	transcriptFileMax    = 512
)

// handleTranscript answers one transcript request on the stream the server
// allocated, shaped like handleGitQuery: wait for the stream, always write an
// answer, always close.
func (s *Session) handleTranscript(ctx context.Context, req *protocol.RunnerTranscriptRequest) {
	log := s.logger()
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		log.Error("transcript: stream not visible", "stream_id", req.StreamId)
		return
	}
	defer stream.CloseBoth()
	if err := s.writeTranscriptAnswer(req, stream); err != nil {
		log.Error("transcript: write answer", "err", err)
		return
	}
	if err := stream.AppendData(true); err != nil {
		log.Error("transcript: half-close", "err", err)
	}
}

// writeTranscriptAnswer writes the TranscriptResult header and, for a get,
// the bytes after it. A file is copied in chunks, never held whole.
func (s *Session) writeTranscriptAnswer(req *protocol.RunnerTranscriptRequest, w io.Writer) error {
	res, body := s.transcriptAnswer(req)
	hdr, err := res.Append(nil)
	if err != nil {
		if body != nil {
			body.Close()
		}
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		if body != nil {
			body.Close()
		}
		return err
	}
	if body == nil {
		return nil
	}
	defer body.Close()
	_, err = io.CopyBuffer(w, body, make([]byte, 64<<10))
	return err
}

func transcriptFail(op protocol.TranscriptOp, st protocol.TranscriptStatus, msg string) protocol.TranscriptResult {
	res := protocol.TranscriptResult{Status: st, Op: op}
	res.SetMessage([]byte(msg))
	switch op {
	case protocol.TranscriptOp_Sessions:
		res.SetSessions(protocol.TranscriptSessionsBody{})
	case protocol.TranscriptOp_Files:
		res.SetFiles(protocol.TranscriptFilesBody{})
	case protocol.TranscriptOp_Get:
		res.SetGet(protocol.TranscriptGetBody{})
	}
	return res
}

// transcriptAnswer resolves the task's directory and every store this
// runner's profiles name, and answers the request (B-D1, B-D2, B-D8).
func (s *Session) transcriptAnswer(req *protocol.RunnerTranscriptRequest) (protocol.TranscriptResult, io.ReadCloser) {
	op := req.Op
	repo := string(req.RepoPath)
	if len(s.AllowedRoots) > 0 && !s.repoAllowed(repo) {
		return transcriptFail(op, protocol.TranscriptStatus_StoreError, "repo "+repo+" is not one of this runner's roots"), nil
	}
	session, file := string(req.Session), string(req.File)
	if len(session) > transcriptSessionMax {
		return transcriptFail(op, protocol.TranscriptStatus_NoSuchSession, "session id too long"), nil
	}
	if len(file) > transcriptFileMax {
		return transcriptFail(op, protocol.TranscriptStatus_NoSuchFile, "file name too long"), nil
	}

	// The task's directory, and whether it is shared (B-D2): a worktree is the
	// task's alone; a --no-worktree task ran in the repo itself.
	dir := filepath.Join(repo, ".harness-worktrees", hex.EncodeToString(req.TaskId.Id[:]))
	shared := false
	if s.NoWorktree {
		dir, shared = repo, true
	}
	if shared && !req.IncludeShared() {
		if op == protocol.TranscriptOp_Sessions {
			res := transcriptFail(op, protocol.TranscriptStatus_Ok,
				"this task ran in the shared repo directory, so its sessions are listed only to a caller holding exec_run on it")
			return res, nil
		}
		return transcriptFail(op, protocol.TranscriptStatus_NoSuchSession, "a shared session needs exec_run"), nil
	}

	getenv := s.transcriptEnv
	if getenv == nil {
		getenv = os.Getenv
	}
	var stores []transcript.Store
	var failures []string
	for _, kind := range s.Profiles.TranscriptStores() {
		st, err := transcript.New(kind, getenv)
		if err != nil {
			failures = append(failures, kind+": "+err.Error())
			continue
		}
		stores = append(stores, st)
	}
	if len(stores) == 0 && len(failures) == 0 {
		return transcriptFail(op, protocol.TranscriptStatus_StoreError, "no agent on this runner keeps transcripts"), nil
	}

	if op == protocol.TranscriptOp_Sessions {
		return listTranscriptSessions(stores, failures, dir, shared), nil
	}
	if session == "" {
		return transcriptFail(op, protocol.TranscriptStatus_NoSuchSession, "no session named"), nil
	}
	// The store that recorded this session in dir; ids are the agents' own,
	// so the first store that knows it answers.
	var owner transcript.Store
	var files []transcript.File
	for _, st := range stores {
		fs, err := st.Files(dir, session)
		if errors.Is(err, transcript.ErrNoSession) {
			continue
		}
		if err != nil {
			failures = append(failures, st.Agent()+": "+err.Error())
			continue
		}
		owner, files = st, fs
		break
	}
	if owner == nil {
		if len(failures) > 0 {
			return transcriptFail(op, protocol.TranscriptStatus_StoreError, strings.Join(failures, "; ")), nil
		}
		return transcriptFail(op, protocol.TranscriptStatus_NoSuchSession, "no session "+session+" in this task's directory"), nil
	}
	switch op {
	case protocol.TranscriptOp_Files:
		res := protocol.TranscriptResult{Status: protocol.TranscriptStatus_Ok, Op: op}
		var body protocol.TranscriptFilesBody
		var rows []protocol.TranscriptFile
		for _, f := range files {
			var row protocol.TranscriptFile
			row.SetName([]byte(f.Name))
			row.Size = uint64(f.Size)
			row.SetSizeKnown(f.SizeKnown)
			rows = append(rows, row)
		}
		body.SetFiles(rows)
		res.SetFiles(body)
		return res, nil
	case protocol.TranscriptOp_Get:
		var body io.ReadCloser
		var size int64
		var err error
		if file == "" {
			body, size, err = tarSession(owner, dir, session, files)
		} else {
			body, size, err = openSized(owner, dir, session, file)
		}
		if errors.Is(err, transcript.ErrNoFile) {
			return transcriptFail(op, protocol.TranscriptStatus_NoSuchFile, "no file "+file+" in session "+session), nil
		}
		if err != nil {
			return transcriptFail(op, protocol.TranscriptStatus_StoreError, err.Error()), nil
		}
		res := protocol.TranscriptResult{Status: protocol.TranscriptStatus_Ok, Op: op}
		res.SetGet(protocol.TranscriptGetBody{Size: uint64(size)})
		return res, body
	}
	return transcriptFail(op, protocol.TranscriptStatus_InternalError, fmt.Sprintf("unknown op %v", op)), nil
}

func listTranscriptSessions(stores []transcript.Store, failures []string, dir string, shared bool) protocol.TranscriptResult {
	var rows []protocol.TranscriptSession
	answered := 0
	for _, st := range stores {
		ss, err := st.Sessions(dir)
		if err != nil {
			failures = append(failures, st.Agent()+": "+err.Error())
			continue
		}
		answered++
		for _, x := range ss {
			var row protocol.TranscriptSession
			row.SetAgent([]byte(x.Agent))
			row.SetId([]byte(x.ID))
			row.CreatedNs = uint64(x.Created.UnixNano())
			row.UpdatedNs = uint64(x.Updated.UnixNano())
			row.Size = uint64(x.Size)
			row.SetShared(shared)
			row.SetSizeKnown(x.SizeKnown)
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedNs < rows[j].CreatedNs })
	// store_error only when no store answered at all; otherwise the rows
	// stand and the message names the agents that could not be read.
	status := protocol.TranscriptStatus_Ok
	if answered == 0 {
		status = protocol.TranscriptStatus_StoreError
	}
	res := protocol.TranscriptResult{Status: status, Op: protocol.TranscriptOp_Sessions}
	res.SetMessage([]byte(strings.Join(failures, "; ")))
	var body protocol.TranscriptSessionsBody
	body.SetSessions(rows)
	res.SetSessions(body)
	return res
}

// openSized opens one file of the session with the size that will be sent.
// A file on disk is sent as it was when opened — sized by Stat, read no
// further — so a live session growing meanwhile still matches its header. A
// file produced on read (opencode's export) is spooled to a temp file first:
// its size is known only then, and a failure surfaces here, before any ok.
func openSized(st transcript.Store, dir, id, name string) (io.ReadCloser, int64, error) {
	rc, err := st.Open(dir, id, name)
	if err != nil {
		return nil, 0, err
	}
	if f, ok := rc.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, 0, err
		}
		return limitedFile{io.LimitReader(f, fi.Size()), f}, fi.Size(), nil
	}
	defer rc.Close()
	return spool(rc)
}

type limitedFile struct {
	io.Reader
	f *os.File
}

func (l limitedFile) Close() error { return l.f.Close() }

// spool copies r to a temp file and returns it rewound, with its size; the
// file is removed when closed.
func spool(r io.Reader) (io.ReadCloser, int64, error) {
	tmp, err := os.CreateTemp("", "transcript-*")
	if err != nil {
		return nil, 0, err
	}
	t := tempFile{tmp}
	n, err := io.Copy(tmp, r)
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		t.Close()
		return nil, 0, err
	}
	return t, n, nil
}

type tempFile struct{ *os.File }

func (t tempFile) Close() error {
	err := t.File.Close()
	_ = os.Remove(t.File.Name())
	return err
}

// tarSession writes the session's files as a tar into a temp file and returns
// it with its size: the header must say how many bytes follow, and building it
// first also turns a file vanishing midway into an error before any ok. Each
// entry is the file as it was when opened (openSized).
func tarSession(st transcript.Store, dir, id string, files []transcript.File) (io.ReadCloser, int64, error) {
	tmp, err := os.CreateTemp("", "transcript-*.tar")
	if err != nil {
		return nil, 0, err
	}
	t := tempFile{tmp}
	fail := func(err error) (io.ReadCloser, int64, error) {
		t.Close()
		return nil, 0, err
	}
	tw := tar.NewWriter(tmp)
	for _, f := range files {
		body, size, err := openSized(st, dir, id, f.Name)
		if err != nil {
			return fail(err)
		}
		err = tw.WriteHeader(&tar.Header{Name: f.Name, Mode: 0o644, Size: size})
		if err == nil {
			_, err = io.Copy(tw, body)
		}
		body.Close()
		if err != nil {
			return fail(err)
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	n, err := tmp.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		return fail(err)
	}
	return t, n, nil
}
