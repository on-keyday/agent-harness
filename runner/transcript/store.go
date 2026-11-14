// Package transcript lists and reads the transcripts coding agents store on
// the runner host, for one working directory (pillar B,
// docs/superpowers/specs/2026-11-14-pillar-b-transcripts-design.md).
//
// A session is listed only when the working directory it records equals the
// one asked about (B-D3): a name or path match only finds candidates. Every
// method that names a session re-checks that, and a file is opened only by a
// name the session's own listing produced, so no path from a caller is ever
// opened.
package transcript

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Session is one agent session recorded in a directory.
type Session struct {
	Agent, ID        string
	Created, Updated time.Time
	Size             int64 // all of the session's files
}

// File is one file a session is made of, named relative to the session.
type File struct {
	Name      string // slash-separated
	Size      int64
	SizeKnown bool // false for a file produced on read (opencode's export)
}

var (
	// ErrNoSession: no session with that id was recorded in the directory.
	ErrNoSession = errors.New("no such session in this directory")
	// ErrNoFile: not a name the session's listing produced.
	ErrNoFile = errors.New("no such file in this session")
)

// Store is one agent's transcript store.
type Store interface {
	Agent() string
	// Sessions lists the sessions recorded with working directory dir.
	Sessions(dir string) ([]Session, error)
	// Files lists session id's files; ErrNoSession unless it was recorded in dir.
	Files(dir, id string) ([]File, error)
	// Open opens one of Files' names; ErrNoSession or ErrNoFile otherwise.
	Open(dir, id, name string) (io.ReadCloser, error)
}

// Kinds are the store kinds New knows, as a profile names them
// (transcriptStore).
var Kinds = []string{"claude", "codex", "pi", "agy", "opencode"}

// New returns the store for kind, rooted where that agent keeps it as seen
// through getenv, the agent's environment (B-D5, B-F8).
func New(kind string, getenv func(string) string) (Store, error) {
	switch kind {
	case "claude":
		return newClaude(getenv)
	case "codex":
		return newCodex(getenv)
	case "pi":
		return newPi(getenv)
	case "agy":
		return newAgy(getenv)
	case "opencode":
		return newOpencode(getenv)
	}
	return nil, fmt.Errorf("transcript: no store for agent kind %q", kind)
}

// SamePath compares two paths as the runner's OS does (B-D9).
func SamePath(a, b string) bool { return samePath(a, b, runtime.GOOS == "windows") }

func samePath(a, b string, windows bool) bool {
	if windows {
		norm := func(p string) string {
			p = strings.ReplaceAll(p, `\`, "/")
			return strings.TrimSuffix(cleanSlash(p), "/")
		}
		return strings.EqualFold(norm(a), norm(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// cleanSlash cleans a slash-separated path the way filepath.Clean does on unix.
func cleanSlash(p string) string {
	vol := ""
	if len(p) >= 2 && p[1] == ':' {
		vol, p = p[:2], p[2:]
	}
	parts := strings.Split(p, "/")
	var out []string
	for _, s := range parts {
		switch s {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	return vol + "/" + strings.Join(out, "/")
}
