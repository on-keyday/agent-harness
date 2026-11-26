package main

import (
	"fmt"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// --- server-defined actions (grant conditions spec C8, C13) ---------------

func (h cliVerbs) actionPut(a verb.ActionPutAction) error {
	c, err := cli.Dial(h.ctx, h.cid(), protocol.ClientKind_Cli)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	line, ok, err := c.RunActionPut(h.ctx, cli.ActionPutInput{
		Edit: a.Sub == "edit", Ref: a.Ref, Revision: a.Revision,
		Name: a.Name, Description: a.Description, DescriptionSet: a.DescriptionSet,
		Params: a.Params, Template: a.Template,
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(h.stdout(), line)
	if !ok {
		return exitCodeError{code: 1}
	}
	return nil
}

func (h cliVerbs) ActionNew(a verb.ActionPutAction) error  { return h.actionPut(a) }
func (h cliVerbs) ActionEdit(a verb.ActionPutAction) error { return h.actionPut(a) }

func (h cliVerbs) ActionRm(a verb.ActionRemoveAction) error {
	c, err := cli.Dial(h.ctx, h.cid(), protocol.ClientKind_Cli)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	line, ok, err := c.RunActionRemove(h.ctx, a.Ref)
	if err != nil {
		return err
	}
	fmt.Fprintln(h.stdout(), line)
	if !ok {
		return exitCodeError{code: 1}
	}
	return nil
}

func (h cliVerbs) ActionLs(a verb.ActionListAction) error {
	c, err := cli.Dial(h.ctx, h.cid(), protocol.ClientKind_Cli)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	list, err := cli.ActionListWith(h.ctx, c)
	if err != nil {
		return err
	}
	if a.JSON {
		fmt.Fprint(h.stdout(), cli.FormatActionsJSON(list))
		return nil
	}
	fmt.Fprint(h.stdout(), cli.FormatActions(list))
	return nil
}
