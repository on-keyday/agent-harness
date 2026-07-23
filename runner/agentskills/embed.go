// Package agentskills embeds the harness agent skill files so both the runner
// (which injects them into task worktrees) and harness-cli (which prints them
// on demand) share one copy. It imports only the standard library.
package agentskills

import (
	"embed"
	"sort"
	"strings"
)

//go:embed all:harness-cli all:independent-review all:landing-to-main all:session-debugging
var FS embed.FS

// Skill returns the SKILL.md bytes for a named skill (e.g. "harness-cli").
func Skill(name string) ([]byte, error) {
	return FS.ReadFile(name + "/SKILL.md")
}

// List returns the names of the embedded skills, sorted. It enumerates the
// embed FS (each skill is a top-level directory holding a SKILL.md) rather
// than hardcoding names, so extending the //go:embed directive above is the
// only edit needed to surface a new skill.
func List() ([]string, error) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := FS.Open(e.Name() + "/SKILL.md"); err != nil {
			continue // a directory without a SKILL.md is not a skill
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
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
