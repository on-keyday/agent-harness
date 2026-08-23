package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every fixture below is a screen CAPTURED from a live Claude Code session on
// 2026-08-22 via `harness-cli session snapshot`, not one written to fit the
// rules. A rule set tested only against invented screens proves the rules match
// themselves.

// The tool-permission prompt, whole. Note the three-option shape: the middle
// option starts with "Yes", not "No", which is what breaks a rule that assumes
// option 2 is the refusal.
const fxPermissionPrompt = `
──────────────────────────────────────────────────────────
 Bash command

   touch /tmp/herdr-blocked-test
   Create empty file at /tmp/herdr-blocked-test

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to /tmp from this project
   3. No

 Esc to cancel · Tab to amend · ctrl+e to explain`

// The trust prompt Claude shows on first run in an unfamiliar directory.
const fxTrustDialog = `
──────────────────────────────────────────────────────────
 Accessing workspace:

 /home/kforfk/workspace

 Quick safety check: Is this a project you created or one you trust?

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 ❯ 1. Yes, I trust this folder
   2. No, exit

 Enter to confirm · Esc to cancel`

// An idle session: the input box, empty, above the mode footer.
const fxIdlePromptBox = `
● AGENTS.md    THIRD-PARTY-NOTICES.md  docs

  Listed 1 directory (ctrl+o to expand)

✻ Worked for 6s

──────────────────────────────────────────────────────────
❯
──────────────────────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)`

// A session MID-TURN. Captured from a live session on 2026-08-22, and the
// reason the screen-side working rule exists: Claude keeps its input box on
// screen for the whole turn, so this screen and an idle one differ only by the
// status line above the box and the interrupt hint in the footer.
const fxWorkingWithInputBox = `
✢ Bunning… (8m 19s · ↓ 32.5k tokens)

──────────────────────────────────────────────────────────
❯
──────────────────────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt`

// Shell mode. Typing '!' REPLACES the '❯' marker, so a rule that knows only
// '❯' reports unknown for a session that is plainly waiting for input. Rebuilt
// from an operator's own --detect --json output, whose prompt_box_body read
// "! " and whose after_last_rule read "  ! for shell mode".
const fxShellModePromptBox = `
● AGENTS.md    THIRD-PARTY-NOTICES.md  docs

──────────────────────────────────────────────────────────
!
──────────────────────────────────────────────────────────
  ! for shell mode`

// A plain shell — no agent UI at all.
const fxBashPrompt = `[kforfk@host workspace]$ echo hi
hi
[kforfk@host workspace]$ `

func lines(s string) []string { return strings.Split(strings.TrimPrefix(s, "\n"), "\n") }

func claudeRules(t *testing.T) DetectRuleSet {
	t.Helper()
	sets, err := DetectRuleSets()
	if err != nil {
		t.Fatalf("DetectRuleSets: %v", err)
	}
	set, ok := sets["claude"]
	if !ok {
		t.Fatalf("no claude rule set; have %v", sets)
	}
	return set
}

func TestDetectRuleSetsLoadAndValidate(t *testing.T) {
	sets, err := DetectRuleSets()
	if err != nil {
		t.Fatalf("the embedded rules do not load: %v", err)
	}
	if len(sets) == 0 {
		t.Fatal("no rule sets embedded")
	}
	for agent, s := range sets {
		if s.Version == "" {
			t.Errorf("%s: no version; an explain output could not say which rules produced a verdict", agent)
		}
		if len(s.Rules) == 0 {
			t.Errorf("%s: no rules", agent)
		}
	}
}

func TestDetectRealScreens(t *testing.T) {
	set := claudeRules(t)
	for _, tc := range []struct {
		name  string
		in    DetectInput
		state DetectState
		rule  string
	}{
		{
			name:  "tool permission prompt is blocked, not idle",
			in:    DetectInput{Lines: lines(fxPermissionPrompt), Title: "✳ Reply with pong"},
			state: DetectBlocked,
			rule:  "permission_prompt_blocked",
		},
		{
			name:  "startup trust dialog is blocked",
			in:    DetectInput{Lines: lines(fxTrustDialog)},
			state: DetectBlocked,
			rule:  "choice_dialog_blocked",
		},
		{
			name:  "an empty input box is idle",
			in:    DetectInput{Lines: lines(fxIdlePromptBox), Title: "✳ Claude Code"},
			state: DetectIdle,
			rule:  "prompt_box_idle",
		},
		{
			name:  "a spinner title is working even mid-transcript",
			in:    DetectInput{Lines: lines(fxIdlePromptBox), Title: "◐ Reply with pong"},
			state: DetectWorking,
			rule:  "title_spinner_working",
		},
		{
			// Operator-reported: this screen returned unknown, because the
			// shell-mode marker replaces the one the rule knew.
			name:  "shell mode is still an input box",
			in:    DetectInput{Lines: lines(fxShellModePromptBox)},
			state: DetectIdle,
			rule:  "prompt_box_idle",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect(set, tc.in)
			if got.State != tc.state {
				t.Errorf("state = %q, want %q (matched %q)", got.State, tc.state, got.MatchedRule)
			}
			if got.MatchedRule != tc.rule {
				t.Errorf("matched rule = %q, want %q", got.MatchedRule, tc.rule)
			}
		})
	}
}

// An input box on screen does NOT mean the agent is waiting for input — Claude
// draws it throughout a turn. So the idle rule fires on a working screen and
// has to lose, which is what the priorities are for. Both working rules are
// checked here because they cover for each other: the title carries the signal
// continuously but can be dropped by a replay ring, and the footer hint is on
// the grid but only while the footer is drawn.
func TestWorkingBeatsAnInputBoxThatIsAlsoOnScreen(t *testing.T) {
	set := claudeRules(t)

	// Title dropped (a long burst, an evicted OSC): the screen must still say
	// working, on the footer hint alone.
	noTitle := Detect(set, DetectInput{Lines: lines(fxWorkingWithInputBox)})
	if noTitle.State != DetectWorking {
		t.Fatalf("state = %q, want working; matched %q", noTitle.State, noTitle.MatchedRule)
	}
	if noTitle.MatchedRule != "interrupt_hint_working" {
		t.Errorf("matched %q, want the screen-side rule to carry it with no title", noTitle.MatchedRule)
	}

	// The idle rule really does fire on this screen — otherwise this test would
	// pass for the wrong reason and stop guarding the conflict.
	var idle *DetectEvaluated
	for i := range noTitle.Rules {
		if noTitle.Rules[i].ID == "prompt_box_idle" {
			idle = &noTitle.Rules[i]
		}
	}
	if idle == nil || !idle.Matched {
		t.Fatal("prompt_box_idle did not match a mid-turn screen; the conflict this guards is gone")
	}

	// With the title present the higher-priority rule takes it, same verdict.
	withTitle := Detect(set, DetectInput{
		Lines: lines(fxWorkingWithInputBox), Title: "◑ herdr agent harness",
	})
	if withTitle.State != DetectWorking || withTitle.MatchedRule != "title_spinner_working" {
		t.Errorf("with a spinner title: state=%q rule=%q, want working via the title",
			withTitle.State, withTitle.MatchedRule)
	}
}

// The measurement that decides the whole priority scheme: while Claude is
// BLOCKED on a human, its window title still carries the idle glyph. A detector
// that trusted the title symmetrically would report "free" for the one state
// that most needs a human. Measured live 2026-08-22.
func TestBlockedBeatsTheIdleTitleThatLiesAboutIt(t *testing.T) {
	set := claudeRules(t)
	in := DetectInput{Lines: lines(fxPermissionPrompt), Title: "✳ Reply with pong"}

	got := Detect(set, in)
	if got.State != DetectBlocked {
		t.Fatalf("state = %q, want blocked — the idle title won", got.State)
	}

	// And prove the title rule really did fire, so this is priority doing the
	// work rather than the title rule quietly failing to match.
	var titleIdle *DetectEvaluated
	for i := range got.Rules {
		if got.Rules[i].ID == "title_idle_weak" {
			titleIdle = &got.Rules[i]
		}
	}
	if titleIdle == nil {
		t.Fatal("no title_idle_weak rule in the explain output")
	}
	if !titleIdle.Matched {
		t.Fatal("title_idle_weak did not match; this test is no longer exercising the conflict it was written for")
	}
}

// "No rule matched" and "genuinely idle" must not be the same value. A bash
// prompt is the everyday case: no agent, no opinion.
func TestUnclaimedScreenIsUnknownWithAReason(t *testing.T) {
	set := claudeRules(t)
	got := Detect(set, DetectInput{Lines: lines(fxBashPrompt)})

	if got.State != DetectUnknown {
		t.Errorf("state = %q, want unknown", got.State)
	}
	if got.FallbackReason == "" {
		t.Error("no fallback_reason; a caller cannot tell an unclaimed screen from a judged one")
	}
	if got.MatchedRule != "" {
		t.Errorf("matched rule = %q, want none", got.MatchedRule)
	}
}

// Every rule is evaluated and reported, including the ones that did not fire —
// that is what makes a wrong verdict debuggable rather than merely re-runnable.
func TestExplainReportsEveryRuleWithItsEvidence(t *testing.T) {
	set := claudeRules(t)
	got := Detect(set, DetectInput{Lines: lines(fxIdlePromptBox), Title: "✳ Claude Code"})

	if len(got.Rules) != len(set.Rules) {
		t.Fatalf("explained %d rules, want all %d", len(got.Rules), len(set.Rules))
	}
	for _, r := range got.Rules {
		if r.ID == "" {
			t.Error("an evaluated rule has no id")
		}
		if r.Evidence.Region == "" {
			t.Errorf("%s: no region recorded", r.ID)
		}
	}
	// The winning rule's evidence must show the text it actually read.
	for _, r := range got.Rules {
		if r.ID == "prompt_box_idle" && !strings.Contains(r.Evidence.RegionPreview, "❯") {
			t.Errorf("prompt_box_idle evidence = %q, want the prompt marker it matched on",
				r.Evidence.RegionPreview)
		}
	}
}

// The input box is located as the second divider from the bottom. A dialog
// screen has one divider, so the region resolves EMPTY rather than to some
// other part of the screen — which is what stops the idle rule from reading a
// dialog's option list as an input line.
func TestPromptBoxRegionIsEmptyOnADialogScreen(t *testing.T) {
	if body := promptBoxBody(lines(fxPermissionPrompt)); len(body) != 0 {
		t.Errorf("prompt_box_body on a dialog screen = %q, want empty", body)
	}
	if body := promptBoxBody(lines(fxIdlePromptBox)); len(body) == 0 {
		t.Fatal("prompt_box_body on an idle screen is empty")
	}
}

func TestRegionsReadTheRightPartOfTheScreen(t *testing.T) {
	in := DetectInput{Lines: lines(fxIdlePromptBox), Title: "✳ Claude Code"}

	if got := region(in, "title"); got != "✳ Claude Code" {
		t.Errorf("title region = %q", got)
	}
	if got := region(in, "bottom_non_empty(1)"); !strings.Contains(got, "auto mode on") {
		t.Errorf("bottom_non_empty(1) = %q, want the footer", got)
	}
	// Blank rows are skipped rather than counted, or a padded frame would make
	// "the last N lines" mean "N blanks".
	if got := region(in, "bottom_non_empty(3)"); strings.Count(got, "\n") != 2 {
		t.Errorf("bottom_non_empty(3) = %q, want exactly 3 non-blank lines", got)
	}
	if got := region(in, "last_non_empty_above_prompt_box"); !strings.Contains(got, "Worked for 6s") {
		t.Errorf("last_non_empty_above_prompt_box = %q, want the turn's status line", got)
	}
	if got := region(in, "after_last_rule"); !strings.Contains(got, "auto mode on") {
		t.Errorf("after_last_rule = %q, want the footer below the box", got)
	}
}

func TestIsHorizontalRule(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"──────────", true},
		{"  ────  ", true},
		{"─── Some label", true}, // a titled divider
		{"─ x", false},           // too short to carry a label
		{"", false},
		{"   ", false},
		{"❯ 1. Yes", false},
		{"not a rule", false},

		// The three shapes a multiplexer pane puts on screen. All three are
		// judged here as they arrive RAW, which is why stripFrameColumns has to
		// run before anything calls this — see TestPaneChromeBreaksTheScreenModel.
		//
		// Claude's own divider, wearing the pane border: not a rule, because the
		// run no longer starts the line. This is defect A.
		{"                    │────────────────▕", false},
		// The pane's sidebar divider sharing a row with transcript prose: a rule,
		// because 25 leading '─' satisfy the titled-divider branch no matter what
		// follows. This is defect B, and it is asserted TRUE on purpose — nothing
		// here was loosened or tightened to hide it; the column is removed instead.
		{"─────────────────────────│  - after_last_rule 5848 B vs 43 B", true},
		// A markdown table Claude printed, inside the pane. Never a rule: the
		// stripper only removes VERTICAL glyphs, so a tee still starts the line.
		{"                    │  ├──────┼──────┤  ▕", false},
	} {
		if got := isHorizontalRule(tc.line); got != tc.want {
			t.Errorf("isHorizontalRule(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// A rule with no condition would match every screen, and one naming an unknown
// region would read "" and never match. Both are silent at match time, so the
// loader has to be the thing that refuses them.
func TestRuleSetValidationRejectsSilentlyBrokenRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  DetectRuleSet
	}{
		{"no condition", DetectRuleSet{Agent: "x", Rules: []DetectRule{{ID: "a", Region: "title"}}}},
		{"unknown region", DetectRuleSet{Agent: "x", Rules: []DetectRule{
			{ID: "a", Region: "nope", DetectCond: DetectCond{Contains: []string{"z"}}}}}},
		{"duplicate id", DetectRuleSet{Agent: "x", Rules: []DetectRule{
			{ID: "a", Region: "title", DetectCond: DetectCond{Contains: []string{"z"}}},
			{ID: "a", Region: "title", DetectCond: DetectCond{Contains: []string{"y"}}}}}},
		{"bad pattern", DetectRuleSet{Agent: "x", Rules: []DetectRule{
			{ID: "a", Region: "title", DetectCond: DetectCond{Regex: []string{"("}}}}}},
		{"no agent", DetectRuleSet{Rules: []DetectRule{
			{ID: "a", Region: "title", DetectCond: DetectCond{Contains: []string{"z"}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRuleSet(tc.set); err == nil {
				t.Error("accepted a rule set that would fail silently at match time")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Inside a multiplexer pane
// ---------------------------------------------------------------------------

// framedPane is an idle Claude screen captured 2026-08-23 from a session
// running inside a herdr pane — same harness and same Claude build as the
// unframed fixtures above, 48×210, with the pane's border down column 25. It is
// on disk rather than inline because it is a whole real screen; the hostname in
// its title is the only edit.
//
// Its title is the PANE's, not Claude's, so no title rule can fire on it. That
// is a property of the capture, not a limitation of the fixture: a pane owns
// the OSC title, so inside one the verdict has to come from the grid.
func framedPane(t *testing.T) DetectInput {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "framed_idle_pane.json"))
	if err != nil {
		t.Fatalf("read pane fixture: %v", err)
	}
	var f struct {
		Title string   `json:"title"`
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode pane fixture: %v", err)
	}
	return DetectInput{Lines: f.Lines, Title: f.Title}
}

// The three breakages are asserted on the RAW lines first. Without that half
// the test would keep passing if the stripping became unnecessary, and would be
// guarding nothing — the failure mode the fixture exists to prevent.
func TestPaneChromeBreaksTheScreenModelUntilItIsStripped(t *testing.T) {
	pane := framedPane(t)

	// A: the input box is invisible, because its borders no longer start with
	// the run of '─'.
	if got := promptBoxTop(pane.Lines); got != -1 {
		t.Fatalf("promptBoxTop on the raw pane = %d, want -1; the fixture no longer shows defect A", got)
	}
	if body := promptBoxBody(pane.Lines); len(body) != 0 {
		t.Fatalf("prompt_box_body on the raw pane = %q, want empty", body)
	}
	// B: the anchor lands mid-screen, so "below the last divider" sweeps in the
	// transcript. Measured 5925 bytes against 43 for the same screen unframed.
	rawBelow := len(strings.Join(afterLastRule(pane.Lines), "\n"))
	if rawBelow < 4000 {
		t.Fatalf("after_last_rule on the raw pane = %d B, want the whole mid-screen sweep", rawBelow)
	}

	stripped := stripFrameColumns(pane.Lines)

	if promptBoxTop(stripped) < 0 {
		t.Fatal("the input box is still invisible after stripping the pane border")
	}
	body := strings.Join(promptBoxBody(stripped), "\n")
	if !strings.Contains(body, "❯") {
		t.Errorf("prompt_box_body after stripping = %q, want the prompt marker", body)
	}
	// C: and the marker is now at the start of its line, which is what the
	// '^'-anchored patterns in detect_rules.json need.
	if below := len(strings.Join(afterLastRule(stripped), "\n")); below > 600 {
		t.Errorf("after_last_rule after stripping = %d B, want the footer only (measured 381)", below)
	}
}

// End to end: the same capture, through the real rule set.
func TestDetectInsideAMultiplexerPane(t *testing.T) {
	got := Detect(claudeRules(t), framedPane(t))
	if got.State != DetectIdle {
		t.Fatalf("state = %q, want idle (matched %q, fallback %q)",
			got.State, got.MatchedRule, got.FallbackReason)
	}
	if got.MatchedRule != "prompt_box_idle" {
		t.Errorf("matched rule = %q, want prompt_box_idle", got.MatchedRule)
	}
}

// Stripping must be inert on a screen with no pane around it. Claude draws
// verticals of its own — a table, a diff gutter — and cutting at one would
// throw away the content to its left.
func TestFrameStrippingLeavesAnUnframedScreenAlone(t *testing.T) {
	for name, fx := range map[string]string{
		"idle":       fxIdlePromptBox,
		"working":    fxWorkingWithInputBox,
		"permission": fxPermissionPrompt,
		"trust":      fxTrustDialog,
		"shell mode": fxShellModePromptBox,
		"bash":       fxBashPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			l := lines(fx)
			if col := frameColumn(l); col != -1 {
				t.Errorf("frameColumn = %d, want none", col)
			}
			if got := stripFrameColumns(l); strings.Join(got, "\n") != strings.Join(l, "\n") {
				t.Error("an unframed screen was modified")
			}
		})
	}
}
