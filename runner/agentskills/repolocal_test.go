package agentskills

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestShippedSkillsCiteNoRepoPaths: the embedded skills are written into every
// task worktree of every repository the harness drives, so nearly all of their
// readers have only the skill text and harness-cli — not this repository. A
// backticked path into this repo's tree (`scripts/x.py`, `server/y.go:12`,
// `docs/superpowers/...`) points them at a file they do not have; annotating it
// ("the harness repo's ...") still spends their attention on it. Reasoning that
// needs such a path belongs in the commit message or the .claude/-only copy.
//
// The directory list is every top-level directory of this repository, so a new
// one is covered without editing the test. A path that exists relative to the
// skill's own directory (a script the skill ships, e.g. codebase-tour's
// scripts/) is the skill's, not the repo's, and passes. .claude/ and .agents/
// paths never match: the runner materialises those into every worktree.
func TestShippedSkillsCiteNoRepoPaths(t *testing.T) {
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, regexp.QuoteMeta(e.Name()))
		}
	}
	cite := regexp.MustCompile("`((?:" + strings.Join(dirs, "|") + `)/[A-Za-z0-9_./-]+)(?::[0-9,-]+)?` + "`")

	skillsDir := filepath.Join(repoRoot, "runner", "agentskills")
	err = filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		rel, _ := filepath.Rel(skillsDir, path)
		skillDir := filepath.Join(skillsDir, strings.SplitN(rel, string(filepath.Separator), 2)[0])
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range cite.FindAllStringSubmatch(line, -1) {
				if _, err := os.Stat(filepath.Join(skillDir, filepath.FromSlash(m[1]))); err == nil {
					continue
				}
				t.Errorf("%s:%d cites %s, a path in this repository; the skill ships to "+
					"every repo, whose agents do not have it — drop it (provenance goes in "+
					"the commit message)", rel, i+1, m[0])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
