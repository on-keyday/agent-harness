// Package buildinfo answers "which commit is this binary" for every process in
// the harness.
//
// It exists as a package rather than a helper inside harness-cli because the
// answer is now asked of two different processes: `harness-cli version` reports
// the local binary (the question a sandboxed agent had — its embedded skills
// can be several commits behind with nothing on its side to say so), and
// `whoami` reports the SERVER's, which is the question the fleet's deploy rule
// needs. Restarting the server first is only checkable if the server can be
// asked what it is running.
//
// It prefers a revision stamped at LINK time (see the Makefile) over Go's own
// vcs.* build settings, because in this repo those settings describe the wrong
// tree. Measured 2026-09-10, and the toolchain source says why: git's root
// marker is registered as a DIRECTORY (`{filename: ".git", isDir: true}`,
// cmd/go/internal/vcs/vcs.go:218) and `isVCSRoot` requires
// `fi.IsDir() == root.isDir` (:639), while a linked worktree's `.git` is a
// FILE. So `FromDir` walks straight past the worktree; a harness worktree lives
// at `<repo>/.harness-worktrees/<id>`, i.e. INSIDE the parent checkout, so it
// lands on the parent's real `.git` and stamps the PARENT's HEAD and the
// PARENT's status. A binary built from source at 24379936 in such a worktree
// reported ad55e3f5, "clean", with nothing to suggest otherwise — and a
// worktree placed outside the checkout gets no vcs.* at all.
//
// That is why the link-time stamp exists rather than being avoidable ceremony:
// almost everything here is built in a worktree, and the whole value of a
// reported revision is being able to trust it.
package buildinfo

import "runtime/debug"

// stampedRevision / stampedTime / stampedDirty are set with -ldflags -X by
// `make build` (and therefore by `make release`), from the tree actually being
// built. Empty for anything built with a bare `go build`, which then falls back
// to Go's vcs.* settings with the caveat above.
//
// stampedDirty is "1" when the built tree has MODIFIED TRACKED files, computed
// with `--untracked-files=no` on purpose. Go's own vcs.modified counts
// untracked files, which makes it permanently true for a checkout that has
// accumulated leftovers — measured: one untracked file is enough — and a flag
// that never changes says nothing while still supplying the sentence "the
// revision does not describe this binary". Restricted to tracked changes it
// answers the question actually being asked: does this binary differ from the
// commit it claims.
var (
	stampedRevision string
	stampedTime     string
	stampedDirty    string
)

// Stamp is what a binary can say about its own provenance. The JSON tags are
// the ones `harness-cli version --json` has always emitted; keep them.
type Stamp struct {
	Revision string `json:"revision"`
	Time     string `json:"time"`
	Modified bool   `json:"modified"`
	Module   string `json:"module,omitempty"`
	Go       string `json:"go,omitempty"`
}

// Read returns this binary's stamp. Every field is best-effort: a binary built
// with -buildvcs=false (which scripts/wire-skew-check.sh does deliberately, to
// build inside a detached worktree at all) carries no revision, and callers
// must render that as an explicit unknown rather than as an empty string —
// "no revision" and "revision you cannot see" read the same otherwise.
func Read() Stamp {
	var s Stamp
	info, ok := debug.ReadBuildInfo()
	if ok {
		s.Module = info.Main.Version
		s.Go = info.GoVersion
		for _, kv := range info.Settings {
			switch kv.Key {
			case "vcs.revision":
				s.Revision = kv.Value
			case "vcs.time":
				s.Time = kv.Value
			case "vcs.modified":
				s.Modified = kv.Value == "true"
			}
		}
	}
	// The link-time stamp wins, and it replaces the WHOLE SET rather than the
	// revision alone. Mixing is what makes a report lie: a stamped revision
	// beside the vcs time reads as "commit X, committed at the time of Y", and
	// beside the vcs modified bit it reports the dirtiness of a different tree
	// entirely. Every field here describes one tree or the other, never both.
	if stampedRevision != "" {
		s.Revision = stampedRevision
		s.Time = stampedTime
		s.Modified = stampedDirty == "1"
	}
	return s
}
