package protocol

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// inlineRowsAllowed is every response format that may still carry a list of
// rows inline, with the reason it may. A response is one task-control message,
// which over UDP is one datagram dropped without a word when it does not fit
// (~1170 bytes); a list grows past that without anyone editing a schema. A new
// list belongs on a stream, as ListResult / ListResultBody does it — or here,
// with the reason written down.
var inlineRowsAllowed = map[string]string{
	"RestoreTasksResponse": "not a board response, and its long rows (a whole prompt) " +
		"come from oneshot tasks, which see little use; revisit when they do " +
		"(2026-10-16-board-row-responses-streamed-design.md §4)",
	"OpenInteractiveResponse": "candidates only on ambiguous_runner, about 95 bytes each: " +
		"over budget at about 12 runners matching one root",
	"RunnerHelloResponse": "16 bytes per re-adopted task: over budget at about 72 on one runner",
	"SetCapsResponse":     "16 bytes per changed task: over budget at about 72 descendants at once",
	"RunnerTrsfStateResponse": "one row per live connection on one runner, " +
		"diagnostic only",
	"WhoAmIResponse": "the caller's own overrides: 8 bytes + 16 per id each, and a task holds " +
		"one per --scope-for clause it was granted",
}

// TestResponseRowsAreNotInline fails when a response format in message.bgn holds
// a list of anything but bytes and is not on inlineRowsAllowed. It reads the
// schema rather than the generated Go, because the schema is what a person
// adding a field edits.
func TestResponseRowsAreNotInline(t *testing.T) {
	f, err := os.Open("message.bgn")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	formatRe := regexp.MustCompile(`^format (\w+):`)
	// `name :[len]Elem` — a list whose element is not a byte.
	listRe := regexp.MustCompile(`:\[[^\]]+\](\w+)`)

	var offenders []string
	current := ""
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if m := formatRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if strings.HasPrefix(line, "enum ") || strings.HasPrefix(line, "format ") {
			current = ""
			continue
		}
		if current == "" || !(strings.HasSuffix(current, "Response") || strings.HasSuffix(current, "Result")) {
			continue
		}
		code, _, _ := strings.Cut(line, "#")
		m := listRe.FindStringSubmatch(code)
		if m == nil || m[1] == "u8" {
			continue
		}
		if !seen[current] {
			seen[current] = true
			if _, ok := inlineRowsAllowed[current]; !ok {
				offenders = append(offenders, current+" ("+strings.TrimSpace(code)+")")
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s carries rows inline: move them to a body on a stream, or add it to inlineRowsAllowed with the reason", o)
	}
	for name := range inlineRowsAllowed {
		if !seen[name] {
			t.Errorf("inlineRowsAllowed names %s, which carries no inline rows any more: drop the entry", name)
		}
	}
}
