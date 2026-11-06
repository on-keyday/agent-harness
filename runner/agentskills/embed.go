// Package agentskills embeds the harness agent skill files so both the runner
// (which injects them into task worktrees) and harness-cli (which prints them
// on demand) share one copy. It imports only the standard library.
package agentskills

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed all:codebase-tour all:harness-cli all:harness-cli-from-a-tool all:independent-review all:landing-to-main all:running-parallel-workers all:session-debugging all:supervising-workers
var FS embed.FS

// Skill returns the SKILL.md bytes for a named skill (e.g. "harness-cli").
func Skill(name string) ([]byte, error) {
	return FS.ReadFile(name + "/SKILL.md")
}

// List returns the names of the embedded skills, sorted. It enumerates the
// embed FS (each skill is a top-level directory holding a SKILL.md) rather
// than hardcoding names, so extending the //go:embed directive above is the
// only edit needed to surface a new skill.
func List() ([]string, error) { return ListFS(FS) }

// ListFS is List over an arbitrary fs.FS. It exists because the runner can be
// pointed at an on-disk skill directory instead of this embed FS
// (agent-runner --agentskills-dir), and an on-disk directory is NOT curated the
// way the //go:embed directive is: os.DirFS("runner/agentskills") also exposes
// embed.go and agentskills_test.go, and a checkout's .claude/skills holds
// repo-only skills. So "a skill is a top-level directory containing SKILL.md"
// has to be an enforced filter over whatever FS is supplied, not a property the
// caller can assume — copying an unfiltered DirFS would ship .go files to every
// task worktree as if they were agent guidance.
func ListFS(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(fsys, e.Name()+"/SKILL.md"); err != nil {
			continue // a directory without a SKILL.md is not a skill
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// A skill may carry files beside its SKILL.md (references/, scripts/), which
// the runner copies into a worktree with the rest of the directory. Where no
// worktree receives them — a no-worktree task, a runtime that never reads
// .claude/skills — the three functions below are the only way to them, through
// `harness-cli skill`.

// Files returns the paths of a skill's files relative to its directory,
// sorted, SKILL.md included.
func Files(name string) ([]string, error) { return FilesFS(FS, name) }

// FilesFS is Files over an arbitrary fs.FS, filtered the way ListFS is: name
// must be a top-level directory holding a SKILL.md.
func FilesFS(fsys fs.FS, name string) ([]string, error) {
	if err := checkSkill(fsys, name); err != nil {
		return nil, err
	}
	var out []string
	err := fs.WalkDir(fsys, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			out = append(out, strings.TrimPrefix(p, name+"/"))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// File returns one file of a skill, by its path relative to the skill's
// directory ("references/questions.md"). A path that leaves the directory is
// refused, not resolved.
func File(name, rel string) ([]byte, error) {
	if err := checkSkill(FS, name); err != nil {
		return nil, err
	}
	rel = strings.ReplaceAll(rel, `\`, "/")
	if !fs.ValidPath(rel) || rel == "." {
		return nil, fmt.Errorf("%q is not a path inside the skill", rel)
	}
	return FS.ReadFile(name + "/" + rel)
}

// Extract writes a skill's files under dir/<name>/, so paths the skill gives
// relative to its own directory resolve there. Without force it refuses before
// writing anything when any of the files already exists, and reports which.
// It returns the paths it wrote.
func Extract(name, dir string, force bool) ([]string, error) {
	files, err := Files(name)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(dir, name)
	if !force {
		var exist []string
		for _, f := range files {
			if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(f))); err == nil {
				exist = append(exist, f)
			}
		}
		if len(exist) > 0 {
			return nil, fmt.Errorf("already under %s: %s (-f replaces them)", root, strings.Join(exist, ", "))
		}
	}
	written := make([]string, 0, len(files))
	for _, f := range files {
		data, err := FS.ReadFile(name + "/" + f)
		if err != nil {
			return written, err
		}
		dst := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return written, err
		}
		written = append(written, dst)
	}
	return written, nil
}

func checkSkill(fsys fs.FS, name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || !fs.ValidPath(name) {
		return fmt.Errorf("%q is not a skill name", name)
	}
	if _, err := fs.Stat(fsys, name+"/SKILL.md"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("not an embedded skill: %w", fs.ErrNotExist)
		}
		return err
	}
	return nil
}

// Description returns the `description:` value from a skill's YAML
// frontmatter, or "" if it has none. It reads the same embedded SKILL.md
// that Skill() serves, so the listing never drifts from the printed doc.
func Description(name string) (string, error) {
	b, err := Skill(name)
	if err != nil {
		return "", err
	}
	return frontmatterField(b, "description"), nil
}

// frontmatterField extracts a single-line `key: value` from a leading
// `---`-delimited YAML frontmatter block. Returns "" if the doc has no
// frontmatter or the key is absent.
func frontmatterField(md []byte, key string) string {
	s := string(md)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return ""
	}
	prefix := key + ":"
	lines := strings.Split(s, "\n")
	for _, raw := range lines[1:] {
		line := strings.TrimRight(raw, "\r")
		if line == "---" {
			break // end of frontmatter
		}
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}
