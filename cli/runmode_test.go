package cli

import (
	"strings"
	"time"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBuildOpenInteractiveRequestCarriesRunModeConditions(t *testing.T) {
	cond := protocol.BackgroundConditions{DeadlineMs: 3600000, TurnEnd: protocol.TurnEndSignal_Quiet}
	oi := buildOpenInteractiveRequest("/r", SessionOpts{
		RunModeSet: true, Background: true, Conditions: &cond,
		Detached: true, InitialRows: 40, InitialCols: 120,
	})
	if !oi.RunModeSet() || !oi.Background() || !oi.ConditionsPresent() || oi.Conditions() == nil ||
		oi.Conditions().DeadlineMs != 3600000 || oi.Conditions().TurnEnd != protocol.TurnEndSignal_Quiet {
		t.Fatalf("request %+v", oi)
	}
	// Nothing set: none of the new bits.
	plain := buildOpenInteractiveRequest("/r", SessionOpts{})
	if plain.RunModeSet() || plain.Background() || plain.ConditionsPresent() || plain.PromptPresent() {
		t.Fatalf("zero opts set run-mode bits: %+v", plain)
	}
}

// The prompt rides a stream; the client refuses one the server would refuse
// before sending anything.
func TestPromptOverCapRefusedClientSide(t *testing.T) {
	if err := checkPrompt(strings.Repeat("a", protocol.PromptMax+1)); err == nil || !strings.Contains(err.Error(), "16384") {
		t.Fatalf("over-cap prompt: %v", err)
	}
	if err := checkPrompt(strings.Repeat("a", protocol.PromptMax)); err != nil {
		t.Fatalf("at-cap prompt refused: %v", err)
	}
}

func TestNewOpenStatusesHaveTexts(t *testing.T) {
	for _, st := range []protocol.OpenInteractiveStatus{
		protocol.OpenInteractiveStatus_DeadlineRequired, protocol.OpenInteractiveStatus_ProfileRequired,
		protocol.OpenInteractiveStatus_PromptRequired, protocol.OpenInteractiveStatus_TurnEndUnsupported,
		protocol.OpenInteractiveStatus_PromptTooLarge, protocol.OpenInteractiveStatus_BadPrompt,
		protocol.OpenInteractiveStatus_ConditionsWithoutBackground,
	} {
		err := openInteractiveStatusError("/r", "", st, nil)
		if err == nil || strings.Contains(err.Error(), "status=") {
			t.Errorf("%v: %v", st, err)
		}
	}
	if err := openInteractiveStatusError("/r", "", protocol.OpenInteractiveStatus_PromptTooLarge, nil); !strings.Contains(err.Error(), "16384") {
		t.Errorf("prompt_too_large text does not name the cap: %v", err)
	}
}

func TestSessionModeStatusTexts(t *testing.T) {
	for _, st := range []protocol.SessionModeStatus{
		protocol.SessionModeStatus_NotFound, protocol.SessionModeStatus_NotSession, protocol.SessionModeStatus_Queued,
		protocol.SessionModeStatus_DeadlineRequired, protocol.SessionModeStatus_TurnEndUnsupported,
		protocol.SessionModeStatus_InternalError, protocol.SessionModeStatus_SelfSwitchRefused,
	} {
		if err := sessionModeStatusError(st, nil); err == nil || strings.Contains(err.Error(), "status=") {
			t.Errorf("%v: %v", st, err)
		}
	}
	if err := sessionModeStatusError(protocol.SessionModeStatus_Ok, nil); err != nil {
		t.Errorf("ok: %v", err)
	}
	// The server's message is the answer when it gives one (not_session names
	// oneshot vs not running).
	if err := sessionModeStatusError(protocol.SessionModeStatus_NotSession, []byte("the task is not running")); !strings.Contains(err.Error(), "not running") {
		t.Errorf("server message dropped: %v", err)
	}
}

func TestRunStr(t *testing.T) {
	queued := protocol.TaskInfo{Status: protocol.TaskStatus_Queued, RunMode: protocol.RunMode_Background, WaitReason: protocol.WaitReason_AllBusy}
	if got := RunStr(&queued); got != "run=bg wait=all_busy" {
		t.Errorf("queued: %q", got)
	}
	placing := queued
	placing.WaitReason = protocol.WaitReason_None
	if got := RunStr(&placing); got != "run=bg wait=placing" {
		t.Errorf("placing: %q", got)
	}
	running := protocol.TaskInfo{Status: protocol.TaskStatus_Detached, RunMode: protocol.RunMode_Background, DeadlineNs: 1, DeadlineRemainingMs: 90_000}
	if got := RunStr(&running); got != "run=bg deadline=1m30s" {
		t.Errorf("running: %q", got)
	}
	ended := protocol.TaskInfo{Status: protocol.TaskStatus_Cancelled, EndedAt: 5, RunMode: protocol.RunMode_Background, EndReason: protocol.EndReason_TurnDoneQuiet}
	if got := RunStr(&ended); got != "run=bg end=turn_done_quiet" {
		t.Errorf("ended: %q", got)
	}
	byOperator := protocol.TaskInfo{Status: protocol.TaskStatus_Cancelled, EndedAt: 5, EndReason: protocol.EndReason_Cancelled}
	if got := RunStr(&byOperator); got != "end=cancelled by=operator" {
		t.Errorf("operator cancel: %q", got)
	}
	byAgent := byOperator
	byAgent.CancelledBy.Id[0], byAgent.CancelledBy.Id[1] = 0xab, 0xcd
	if got := RunStr(&byAgent); got != "end=cancelled by=abcd0000" {
		t.Errorf("agent cancel: %q", got)
	}
	hidden := byOperator
	hidden.SetCancelledByHidden(true)
	if got := RunStr(&hidden); got != "end=cancelled by=hidden" {
		t.Errorf("hidden cancel: %q", got)
	}
	legacy := protocol.TaskInfo{Status: protocol.TaskStatus_Succeeded, EndedAt: 5}
	if got := RunStr(&legacy); got != "end=unknown" {
		t.Errorf("legacy terminal: %q", got)
	}
	if got := RunStr(&protocol.TaskInfo{Status: protocol.TaskStatus_Running}); got != "" {
		t.Errorf("foreground running: %q", got)
	}
}

func TestRunModeFlagsApply(t *testing.T) {
	var o SessionOpts
	RunModeFlags{Background: true, Prompt: "p", Deadline: time.Hour}.Apply(&o)
	if !o.RunModeSet || !o.Background || !o.Detached || o.Prompt != "p" || o.Conditions == nil || o.Conditions.DeadlineMs != 3600000 {
		t.Fatalf("background: %+v", o)
	}
	// A resume naming no mode but carrying conditions: detached, mode unset.
	var r SessionOpts
	RunModeFlags{Deadline: time.Minute}.Apply(&r)
	if r.RunModeSet || !r.Detached || r.Conditions == nil {
		t.Fatalf("conditions only: %+v", r)
	}
	var f SessionOpts
	RunModeFlags{Foreground: true}.Apply(&f)
	if !f.RunModeSet || f.Background || f.Conditions != nil || f.Detached {
		t.Fatalf("foreground: %+v", f)
	}
	var none SessionOpts
	RunModeFlags{}.Apply(&none)
	if none.RunModeSet || none.Conditions != nil || none.Detached {
		t.Fatalf("nothing set changed opts: %+v", none)
	}
}

// --rows 0 --cols 0 sends no size: a detached open's 0x0 stays possible.
func TestRowsColsZeroSendsNoSize(t *testing.T) {
	oi := buildOpenInteractiveRequest("/r", SessionOpts{Detached: true})
	if r, c := oi.InitialRows(), oi.InitialCols(); r == nil || c == nil || *r != 0 || *c != 0 {
		t.Fatalf("size %v x %v, want 0 x 0 (the server applies none)", r, c)
	}
}
