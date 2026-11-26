package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// Server-defined actions (grant conditions spec C8, C13): the cmdline verbs.
// Each answers through ActionResultMsg with the text the CLI prints.

// ActionResultMsg is RunnerKeyResultMsg's shape for the action verbs, shown
// the same way.
type ActionResultMsg RunnerKeyResultMsg

func actionCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// DoActionPut puts one action through the shared cli funnel.
func DoActionPut(c *cli.Client, in cli.ActionPutInput) tea.Cmd {
	verbName := "action new"
	if in.Edit {
		verbName = "action edit"
	}
	return func() tea.Msg {
		ctx, cancel := actionCtx()
		defer cancel()
		line, ok, err := c.RunActionPut(ctx, in)
		return ActionResultMsg{Verb: verbName, Text: line, OK: ok, Err: err}
	}
}

func DoActionRemove(c *cli.Client, ref string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := actionCtx()
		defer cancel()
		line, ok, err := c.RunActionRemove(ctx, ref)
		return ActionResultMsg{Verb: "action rm", Text: line, OK: ok, Err: err}
	}
}

func DoActionList(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := actionCtx()
		defer cancel()
		list, err := cli.ActionListWith(ctx, c)
		if err != nil {
			return ActionResultMsg{Verb: "action ls", Err: err}
		}
		return ActionResultMsg{Verb: "action ls", Text: cli.FormatActions(list), OK: true}
	}
}

func actionPutInput(v verb.ActionPutAction) cli.ActionPutInput {
	return cli.ActionPutInput{
		Edit: v.Sub == "edit", Ref: v.Ref, Revision: v.Revision,
		Name: v.Name, Description: v.Description, DescriptionSet: v.DescriptionSet,
		Params: v.Params, Template: v.Template,
	}
}

func (h tuiVerbs) actionPut(v verb.ActionPutAction) tea.Cmd {
	if h.a.client == nil {
		h.a.cmdresult.Append(ErrorStyle.Render("action: not connected to server"))
		return nil
	}
	return DoActionPut(h.a.client, actionPutInput(v))
}

func (h tuiVerbs) ActionNew(v verb.ActionPutAction) tea.Cmd  { return h.actionPut(v) }
func (h tuiVerbs) ActionEdit(v verb.ActionPutAction) tea.Cmd { return h.actionPut(v) }

func (h tuiVerbs) ActionRm(v verb.ActionRemoveAction) tea.Cmd {
	if h.a.client == nil {
		h.a.cmdresult.Append(ErrorStyle.Render("action rm: not connected to server"))
		return nil
	}
	return DoActionRemove(h.a.client, v.Ref)
}

func (h tuiVerbs) ActionLs(verb.ActionListAction) tea.Cmd {
	if h.a.client == nil {
		h.a.cmdresult.Append(ErrorStyle.Render("action ls: not connected to server"))
		return nil
	}
	return DoActionList(h.a.client)
}
