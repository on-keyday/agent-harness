package cli

import (
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
