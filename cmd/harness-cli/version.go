package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
)

// buildStamp is what `harness-cli version` reports: which commit this binary —
// and therefore the agent skills compiled into it — was built from.
//
// The question it answers came from a sandboxed agent that could not tell how
// old its guidance was. harness-cli is bind-mounted into the sandbox container
// as a single file, so a running container keeps the binary that existed when
// it started and never sees a later `make build` (see
// scripts/sandbox/claude-in-podman.sh). The skills are embedded IN that binary,
// so a confined agent can be reading a skill several commits behind with
// nothing on its side to say so — the repo's HEAD is not visible from in there.
//
// go build stamps vcs.* into every binary built from a git tree, so the answer
// needed no new plumbing, only a way to ask for it.
type buildStamp struct {
	Revision string `json:"revision"`
	Time     string `json:"time"`
	Modified bool   `json:"modified"`
	Module   string `json:"module,omitempty"`
	Go       string `json:"go,omitempty"`
}

func readBuildStamp() buildStamp {
	var s buildStamp
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return s
	}
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
	return s
}

// writeVersion renders the stamp. The human line leads with the revision
// because that is the field you compare against a repo you can see; "dirty"
// is called out because an uncommitted build has no comparable revision.
func writeVersion(w io.Writer, asJSON bool) error {
	s := readBuildStamp()
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	rev := s.Revision
	if rev == "" {
		rev = "unknown (built without VCS stamping)"
	}
	state := "clean"
	if s.Modified {
		state = "DIRTY (uncommitted changes; revision does not describe this binary)"
	}
	if s.Time == "" {
		_, err := fmt.Fprintf(w, "harness-cli %s  %s\n", rev, state)
		return err
	}
	_, err := fmt.Fprintf(w, "harness-cli %s  committed %s  %s\n", rev, s.Time, state)
	return err
}
