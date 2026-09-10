package buildinfo

import "testing"

// The link-time stamp replaces Go's vcs.* as a SET, never field by field.
//
// Mixing is the specific way a build report lies: a stamped revision beside the
// vcs time reads as "commit X, committed at the time of Y", and beside the vcs
// modified bit it reports the dirtiness of a different tree — in this repo,
// literally the parent checkout, because a linked worktree's .git is a file and
// the toolchain walks past it. An earlier draft of Read() hardcoded
// Modified=false whenever a revision was stamped, which is the same class of
// lie with the answer written in.
func TestReadPrefersTheLinkTimeStampAsASet(t *testing.T) {
	origRev, origTime, origDirty := stampedRevision, stampedTime, stampedDirty
	t.Cleanup(func() { stampedRevision, stampedTime, stampedDirty = origRev, origTime, origDirty })

	stampedRevision = "2437993666f1dd7f0e35f39f194c2c5823643170"
	stampedTime = "2026-09-10T07:37:02+09:00"
	stampedDirty = "1"

	got := Read()
	if got.Revision != stampedRevision {
		t.Errorf("revision = %q, want the stamped one", got.Revision)
	}
	if got.Time != stampedTime {
		t.Errorf("time = %q, want the stamped one — a stamped revision beside a vcs time describes two trees", got.Time)
	}
	if !got.Modified {
		t.Error("modified = false with a dirty stamp: the binary would claim to match a commit it does not")
	}
	// Not stamped, so these still come from the build info of this test binary
	// — the fields the stamp has nothing to say about.
	if got.Go == "" {
		t.Error("go version should always be present in build info")
	}

	stampedDirty = ""
	if Read().Modified {
		t.Error("modified = true with a clean stamp")
	}
}

// With nothing stamped, Read is the plain vcs.* reader it started as. A `go
// build` with no Makefile involved must keep working, which is why the stamp is
// a preference and not a requirement.
func TestReadFallsBackToVCSSettings(t *testing.T) {
	origRev := stampedRevision
	stampedRevision = ""
	t.Cleanup(func() { stampedRevision = origRev })

	// A test binary carries no vcs.* (measured: `go test` records none), so the
	// assertion is about the SHAPE — the reader returns without inventing a
	// revision, and an empty one is a real answer callers must render as such.
	if got := Read(); got.Revision != "" && len(got.Revision) != 40 {
		t.Errorf("revision = %q: neither absent nor a full git sha", got.Revision)
	}
}
