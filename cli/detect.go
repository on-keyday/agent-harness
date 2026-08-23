package cli

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Agent-state detection: a pure function from a rendered screen to a lifecycle
// state, plus the evidence for how it got there.
//
// The problem it solves is that PTY byte-quiescence cannot tell "the model is
// thinking", "a dialog is waiting for a human" and "genuinely free" apart —
// all three are silent. Those need different operator responses, and the middle
// one is the whole reason to watch a worker at all.
//
// Everything here operates on ALREADY-RENDERED text: a screen as one string per
// grid row, plus the OSC title. No VT semantics, no terminal state, no I/O — so
// it runs on a snapshot taken minutes ago just as well as on a live one, and a
// rule can be tested against a captured screen with no agent running.
//
// The rules themselves live in detect_rules.json rather than in Go, because
// they are the part that rots: they match another program's UI, and that UI
// changes on its own schedule. Keeping them as data means a bad regex is a file
// edit, not a release.

// DetectState is the lifecycle state a screen reports.
type DetectState string

const (
	// DetectUnknown is "no opinion" — an unrecognised program, or a screen no
	// rule claimed. It is NOT a synonym for idle: acting on it as though the
	// agent were free is the mistake this whole file exists to prevent.
	DetectUnknown DetectState = "unknown"
	// DetectWorking is the agent actively running a turn.
	DetectWorking DetectState = "working"
	// DetectBlocked is the agent waiting on a HUMAN — an approval, a menu, a
	// question. Silent, exactly like thinking, and the opposite of free.
	DetectBlocked DetectState = "blocked"
	// DetectIdle is the agent ready for input.
	DetectIdle DetectState = "idle"
)

// DetectInput is one screen to judge.
//
// Title is the OSC 0/2 window title, which is NOT part of the grid and has to
// be captured separately from the byte stream. It is the single highest-value
// signal for `working` on agents that put a spinner there, and the single most
// misleading one for `idle` — see the priorities in detect_rules.json.
type DetectInput struct {
	Lines []string
	Title string
}

// DetectRule is one rule: a condition evaluated against one region of the
// screen, and the state a match implies.
type DetectRule struct {
	ID     string      `json:"id"`
	State  DetectState `json:"state"`
	Region string      `json:"region"`
	// Priority resolves overlaps. The highest-priority MATCHING rule wins, so
	// a weak-but-broad signal can be outranked by a specific one without
	// either having to know about the other.
	Priority int `json:"priority"`
	// SkipStateUpdate marks a rule that matches but must NOT change the state:
	// a screen that some other rule would misread (a transcript viewer, a
	// model picker) is recognised here so it silences everything below it.
	// Not expressible with priority alone, which can only pick a winner.
	SkipStateUpdate bool `json:"skip_state_update,omitempty"`
	// Note is free text for a human reading the rule file; never matched on.
	Note string `json:"note,omitempty"`

	DetectCond
}

// DetectCond is the condition language. Every field is optional and all present
// fields must hold, so the zero value matches everything (a rule with no
// condition is a bug the loader rejects rather than a wildcard).
//
// It nests: Any/All/Not hold further conditions, which is what lets one rule
// say "these two strings, and any one of these three other shapes".
type DetectCond struct {
	// Contains: every string must appear, case-insensitively.
	Contains []string `json:"contains,omitempty"`
	// Regex: every pattern must match somewhere in the region.
	Regex []string `json:"regex,omitempty"`
	// LineRegex: every pattern must match some SINGLE line of the region.
	// Distinct from Regex because `^`/`$` anchor to the region otherwise, and
	// most screen shapes are per-line facts.
	LineRegex []string `json:"line_regex,omitempty"`
	// Any: at least one must hold.
	Any []DetectCond `json:"any,omitempty"`
	// All: every one must hold.
	All []DetectCond `json:"all,omitempty"`
	// Not: none may hold.
	Not []DetectCond `json:"not,omitempty"`
}

func (c DetectCond) empty() bool {
	return len(c.Contains) == 0 && len(c.Regex) == 0 && len(c.LineRegex) == 0 &&
		len(c.Any) == 0 && len(c.All) == 0 && len(c.Not) == 0
}

// DetectRuleSet is one agent's rules, in file order.
type DetectRuleSet struct {
	Agent string `json:"agent"`
	// Version is stamped by whoever edits the file. It exists so an explain
	// output can say WHICH ruleset produced a verdict, which is the first
	// question when a verdict looks wrong.
	Version string       `json:"version"`
	Rules   []DetectRule `json:"rules"`
}

// DetectEvidence is what a rule actually looked at. It is the difference
// between a heuristic you can debug and one you can only re-run: a verdict
// names its rule, and the rule names the text it read.
type DetectEvidence struct {
	Region      string `json:"region"`
	RegionBytes int    `json:"region_bytes"`
	// RegionPreview is the head of the region text, so a wrong verdict shows
	// what the rule was reading rather than what you assume was on screen.
	RegionPreview string `json:"region_preview"`
}

// DetectEvaluated is one rule's result, kept for every rule rather than
// stopping at the winner — the rules that did NOT fire are most of the answer
// when a screen is judged wrongly.
type DetectEvaluated struct {
	ID       string         `json:"id"`
	Priority int            `json:"priority"`
	State    DetectState    `json:"state"`
	Matched  bool           `json:"matched"`
	Evidence DetectEvidence `json:"evidence"`
}

// DetectExplain is the full verdict.
type DetectExplain struct {
	Agent   string      `json:"agent"`
	Version string      `json:"version"`
	State   DetectState `json:"state"`
	// MatchedRule is the winning rule's id, "" when none matched.
	MatchedRule string `json:"matched_rule,omitempty"`
	// FallbackReason is set when no rule matched and the state came from a
	// default. **Without it, "no rule matched" and "genuinely idle" are the
	// same value** — the trap that makes a silent misdetection look like a
	// confident answer.
	FallbackReason string `json:"fallback_reason,omitempty"`
	// SkippedBy names the rule that suppressed a state update, if any.
	SkippedBy string            `json:"skipped_by,omitempty"`
	Rules     []DetectEvaluated `json:"rules"`
}

//go:embed detect_rules.json
var embeddedDetectRules []byte

// DetectRuleSets returns the built-in rule sets, keyed by agent name.
func DetectRuleSets() (map[string]DetectRuleSet, error) {
	var sets []DetectRuleSet
	if err := json.Unmarshal(embeddedDetectRules, &sets); err != nil {
		return nil, fmt.Errorf("detect_rules.json: %w", err)
	}
	out := make(map[string]DetectRuleSet, len(sets))
	for _, s := range sets {
		if err := validateRuleSet(s); err != nil {
			return nil, err
		}
		out[s.Agent] = s
	}
	return out, nil
}

// validateRuleSet rejects the shapes that would fail silently at match time: a
// rule with no condition matches every screen, and one with an unknown region
// reads an empty string and never matches. Both look like a detection bug
// later; here they are a load error naming the rule.
func validateRuleSet(s DetectRuleSet) error {
	if s.Agent == "" {
		return fmt.Errorf("detect rules: a rule set has no agent name")
	}
	seen := map[string]bool{}
	for _, r := range s.Rules {
		switch {
		case r.ID == "":
			return fmt.Errorf("detect rules [%s]: a rule has no id", s.Agent)
		case seen[r.ID]:
			return fmt.Errorf("detect rules [%s]: duplicate rule id %q", s.Agent, r.ID)
		case r.DetectCond.empty():
			return fmt.Errorf("detect rules [%s/%s]: no condition; it would match every screen", s.Agent, r.ID)
		}
		seen[r.ID] = true
		if _, ok := regionKind(r.Region); !ok {
			return fmt.Errorf("detect rules [%s/%s]: unknown region %q", s.Agent, r.ID, r.Region)
		}
		if err := compileCond(r.DetectCond); err != nil {
			return fmt.Errorf("detect rules [%s/%s]: %w", s.Agent, r.ID, err)
		}
		if r.SkipStateUpdate && r.State != DetectUnknown {
			return fmt.Errorf("detect rules [%s/%s]: skip_state_update with state %q; a suppressing rule states no state",
				s.Agent, r.ID, r.State)
		}
	}
	return nil
}

func compileCond(c DetectCond) error {
	for _, p := range append(append([]string{}, c.Regex...), c.LineRegex...) {
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("bad pattern %q: %w", p, err)
		}
	}
	for _, sub := range append(append(append([]DetectCond{}, c.Any...), c.All...), c.Not...) {
		if err := compileCond(sub); err != nil {
			return err
		}
	}
	return nil
}

// Detect judges one screen against one agent's rules.
//
// EVERY rule is evaluated, not just up to the first match: the explain output
// is the point, and a rule that did not fire is evidence too. Rule sets are
// tens of rules over a few kilobytes of text, so the cost is noise.
func Detect(set DetectRuleSet, in DetectInput) DetectExplain {
	out := DetectExplain{Agent: set.Agent, Version: set.Version, State: DetectUnknown}

	// Undo any pane chrome first, so every rule below reads the agent's screen
	// rather than the multiplexer's. `in` is a copy, so this is local.
	in.Lines = stripFrameColumns(in.Lines)

	var best *DetectRule
	for i := range set.Rules {
		r := &set.Rules[i]
		text := region(in, r.Region)
		matched := condMatches(r.DetectCond, text)
		out.Rules = append(out.Rules, DetectEvaluated{
			ID: r.ID, Priority: r.Priority, State: r.State, Matched: matched,
			Evidence: DetectEvidence{
				Region:        r.Region,
				RegionBytes:   len(text),
				RegionPreview: preview(text),
			},
		})
		if !matched {
			continue
		}
		if best == nil || r.Priority > best.Priority {
			best = r
		}
	}

	if best == nil {
		// No rule claimed the screen. Reporting idle here would be the same
		// collapse the fallback_reason field exists to prevent, so the state
		// stays unknown and says why.
		out.FallbackReason = "no_rule_matched"
		return out
	}
	if best.SkipStateUpdate {
		out.SkippedBy = best.ID
		out.FallbackReason = "suppressed_by:" + best.ID
		return out
	}
	out.State = best.State
	out.MatchedRule = best.ID
	return out
}

const previewLimit = 240

func preview(s string) string {
	if len(s) <= previewLimit {
		return s
	}
	return s[:previewLimit] + "…"
}

// ---------------------------------------------------------------------------
// Regions
// ---------------------------------------------------------------------------

type regionSpec int

const (
	regionTitle regionSpec = iota
	regionWholeScreen
	regionBottomNonEmpty // parameterised: bottom_non_empty(N)
	regionAfterLastRule
	regionPromptBoxBody
	regionAbovePromptBox
	regionLastNonEmptyAbovePromptBox
)

// regionKind parses a region spec, reporting whether it is known. The count
// form is `bottom_non_empty(N)`.
func regionKind(spec string) (regionSpec, bool) {
	switch strings.TrimSpace(spec) {
	case "title":
		return regionTitle, true
	case "whole_screen":
		return regionWholeScreen, true
	case "after_last_rule":
		return regionAfterLastRule, true
	case "prompt_box_body":
		return regionPromptBoxBody, true
	case "above_prompt_box":
		return regionAbovePromptBox, true
	case "last_non_empty_above_prompt_box":
		return regionLastNonEmptyAbovePromptBox, true
	}
	if _, ok := regionCount(spec); ok {
		return regionBottomNonEmpty, true
	}
	return 0, false
}

func regionCount(spec string) (int, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(spec), "bottom_non_empty(")
	if !ok {
		return 0, false
	}
	rest, ok = strings.CutSuffix(rest, ")")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// region extracts the text a rule reads. An unknown spec yields "", which
// matches nothing — the loader rejects those, so this is defence rather than
// behaviour.
func region(in DetectInput, spec string) string {
	kind, ok := regionKind(spec)
	if !ok {
		return ""
	}
	switch kind {
	case regionTitle:
		// NOT part of the grid: it arrives from OSC 0/2 on the byte stream.
		//
		// A title that is not valid UTF-8 is a PARTIAL capture, not a title.
		// CollectRaw cuts the stream when its settle timer expires, wherever the
		// reader happens to be, so a title can be captured mid-character — an
		// operator report showed a one-byte title of "\xe2", the lead byte of
		// the spinner glyph. Matching a rule against that fragment could only
		// ever produce a wrong answer with a straight face, so it reads as
		// absent instead. Rules that need the title then simply do not fire, and
		// the screen-side rules decide.
		if !utf8.ValidString(in.Title) {
			return ""
		}
		return in.Title
	case regionWholeScreen:
		return strings.Join(in.Lines, "\n")
	case regionBottomNonEmpty:
		n, _ := regionCount(spec)
		return strings.Join(bottomNonEmpty(in.Lines, n), "\n")
	case regionAfterLastRule:
		return strings.Join(afterLastRule(in.Lines), "\n")
	case regionPromptBoxBody:
		return strings.Join(promptBoxBody(in.Lines), "\n")
	case regionAbovePromptBox:
		return strings.Join(abovePromptBox(in.Lines), "\n")
	case regionLastNonEmptyAbovePromptBox:
		return lastNonEmpty(abovePromptBox(in.Lines))
	}
	return ""
}

// bottomNonEmpty returns the last n non-blank lines, in screen order. Blank
// rows are skipped rather than counted: a full-screen app pads its frame, so
// "the last 5 lines" would otherwise mean "5 blanks" on a half-empty screen.
func bottomNonEmpty(lines []string, n int) []string {
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		out = append(out, lines[i])
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

func lastNonEmpty(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// verticalFrameRunes are the glyphs a multiplexer draws down the side of a
// pane. VERTICAL ones only: a corner or a tee ('┌', '├', '└') belongs to a box
// drawn INSIDE the pane — a markdown table, a dialog — and cutting at one would
// eat the content it encloses.
const verticalFrameRunes = "│┃┆┇┊┋╎╏║▏▕|"

// stripFrameColumns removes the left-hand chrome a multiplexer draws around a
// pane, so everything below sees the agent's own screen.
//
// It has to run before anything else reads the grid, because inside a pane
// EVERY row starts with the border and that breaks the screen model in three
// places at once — measured on a captured 48×210 herdr pane:
//
//   - The agent's own dividers arrive as `│────▕`. isHorizontalRule counts '─'
//     from the start of the line, so it stops seeing them: the input box goes
//     invisible and promptBoxTop returns -1.
//   - The pane's own sidebar divider shares its row with ordinary transcript
//     text (`─────│  some prose`), and the "three or more may carry a label"
//     branch accepts it. afterLastRule then anchors mid-screen: 5925 bytes of
//     transcript where the same screen unframed yields 43.
//   - Every line_regex in detect_rules.json is '^'-anchored and '│' is not
//     whitespace, so even a repaired divider search would still match nothing.
//
// Cutting the column fixes all three and leaves the rules and their patterns
// untouched. Nested panes peel one border per pass.
func stripFrameColumns(lines []string) []string {
	for {
		cut := frameColumn(lines)
		if cut < 0 {
			return lines
		}
		out := make([]string, len(lines))
		for i, l := range lines {
			if r := []rune(l); len(r) > cut+1 {
				out[i] = string(r[cut+1:])
			}
		}
		lines = out
	}
}

// frameColumn returns the column holding the pane's left border, or -1 when the
// screen has none.
//
// Only the LEFT border is looked for. The patterns that read these regions are
// '^'-anchored, so the right one cannot affect a match — and it is not reliably
// there to find: on the captured pane the right edge reached only the 24 of 48
// rows long enough to hold it, against 48 of 48 for the left border. A column
// past the midpoint is rejected for the same reason it would be wrong to cut
// there: doing so would discard most of the screen, which is content, not
// chrome.
func frameColumn(lines []string) int {
	if len(lines) == 0 {
		return -1
	}
	counts := map[int]int{}
	width := 0
	for _, l := range lines {
		col := 0
		for _, ch := range l {
			if strings.ContainsRune(verticalFrameRunes, ch) {
				counts[col]++
			}
			col++
		}
		if col > width {
			width = col
		}
	}
	// Four fifths of the rows, and never fewer than three: the border of a real
	// pane is on every row, while a vertical glyph that belongs to the content
	// is on a handful.
	need := (len(lines)*4 + 4) / 5
	if need < 3 {
		need = 3
	}
	best := -1
	for col, n := range counts {
		if n < need || col >= width/2 {
			continue
		}
		if best < 0 || col < best {
			best = col
		}
	}
	return best
}

// isHorizontalRule reports a box-drawing divider line. Agents draw their input
// box and their dialogs with these, which is what makes them a usable landmark
// for "the part of the screen below the last divider".
//
// A short run is only a rule when it is the whole line; three or more may carry
// a trailing label, which is how a titled divider is drawn.
func isHorizontalRule(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	runes := []rune(t)
	n := 0
	for n < len(runes) && runes[n] == '─' {
		n++
	}
	if n == 0 {
		return false
	}
	return strings.TrimSpace(string(runes[n:])) == "" || n >= 3
}

// afterLastRule is everything below the last divider — where a dialog puts its
// question and its options.
func afterLastRule(lines []string) []string {
	last := -1
	for i, l := range lines {
		if isHorizontalRule(l) {
			last = i
		}
	}
	if last < 0 {
		return lines
	}
	return lines[last+1:]
}

// promptBoxTop finds the top border of the input box: the SECOND divider
// counting from the bottom. An input box is drawn as a divider, the input line,
// and another divider, so the second-from-last is its top edge.
func promptBoxTop(lines []string) int {
	count := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if !isHorizontalRule(lines[i]) {
			continue
		}
		count++
		if count == 2 {
			return i
		}
	}
	return -1
}

// promptBoxBody is the input box's interior — the lines between its top border
// and the next divider below it. An empty `❯` here is the cleanest evidence an
// agent is waiting for input rather than for a human to answer something.
func promptBoxBody(lines []string) []string {
	top := promptBoxTop(lines)
	if top < 0 {
		return nil
	}
	for i := top + 1; i < len(lines); i++ {
		if isHorizontalRule(lines[i]) {
			return lines[top+1 : i]
		}
	}
	return lines[top+1:]
}

// abovePromptBox is everything above the input box: the transcript, where a
// turn's own status line sits.
func abovePromptBox(lines []string) []string {
	top := promptBoxTop(lines)
	if top < 0 {
		return lines
	}
	return lines[:top]
}

// ---------------------------------------------------------------------------
// Condition matching
// ---------------------------------------------------------------------------

// condMatches evaluates one condition against a region's text. Matching is
// case-insensitive for Contains because agent UIs re-case their own labels
// between releases; Regex/LineRegex carry their own `(?i)` when they want it.
func condMatches(c DetectCond, text string) bool {
	lower := strings.ToLower(text)
	for _, s := range c.Contains {
		if !strings.Contains(lower, strings.ToLower(s)) {
			return false
		}
	}
	for _, p := range c.Regex {
		re, err := regexp.Compile(p)
		if err != nil || !re.MatchString(text) {
			return false
		}
	}
	for _, p := range c.LineRegex {
		re, err := regexp.Compile(p)
		if err != nil {
			return false
		}
		hit := false
		for _, line := range strings.Split(text, "\n") {
			if re.MatchString(line) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for _, sub := range c.All {
		if !condMatches(sub, text) {
			return false
		}
	}
	for _, sub := range c.Not {
		if condMatches(sub, text) {
			return false
		}
	}
	if len(c.Any) > 0 {
		hit := false
		for _, sub := range c.Any {
			if condMatches(sub, text) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}
