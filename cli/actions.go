package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Server-defined actions, client side
// (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C8, C12, C13, C17). Every request carries an id; a name resolves to one
// through action_list (ResolveActionRef).

// ActionIDHex is an action id as every surface prints it: all 32 hex, the
// form --grant-condition exec_run=actions:<id> takes.
func ActionIDHex(id protocol.ActionID) string { return hex.EncodeToString(id.Id[:]) }

// ActionListWith reads every action. Needs action_admin or exec_run.
func ActionListWith(ctx context.Context, c *Client) ([]protocol.ActionRecord, error) {
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ActionList}}
	req.SetActionList(protocol.ActionListRequest{})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	l := resp.ActionList()
	if resp.Kind != protocol.TaskControlKind_ActionList || l == nil {
		return nil, fmt.Errorf("action ls: unexpected response kind=%v", resp.Kind)
	}
	if l.Status != protocol.ActionStatus_Ok {
		return nil, fmt.Errorf("action ls: %s", l.Status)
	}
	var body protocol.ActionListBody
	if err := ReadResponseBody(ctx, c.Transport(), l.StreamId, &body); err != nil {
		return nil, fmt.Errorf("action ls: %w", err)
	}
	return body.Actions, nil
}

// ActionPutWith creates (zero id, expect 0) or edits (the id, the revision it
// edits) one action. The definition rides its own stream: a template has no
// bound that fits one datagram. It is checked with the server's own rule
// first, so a bad definition is refused here with the same words.
func ActionPutWith(ctx context.Context, c payloadSender, id protocol.ActionID, expect uint32, def protocol.ActionDefinition) (protocol.ActionPutResponse, error) {
	if err := actiondef.Validate(&def); err != nil {
		return protocol.ActionPutResponse{}, fmt.Errorf("action: %w", err)
	}
	body, err := def.EncodeCopy(nil)
	if err != nil {
		return protocol.ActionPutResponse{}, fmt.Errorf("action: encode: %w", err)
	}
	tr, err := c.TaskControlWithPayload(ctx, func(streamID uint64) (*protocol.TaskControlRequest, error) {
		req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ActionPut}}
		req.SetActionPut(protocol.ActionPutRequest{Id: id, ExpectRevision: expect, PayloadStreamId: streamID})
		return req, nil
	}, body)
	if err == nil {
		err = tr.Err
	}
	if err != nil {
		return protocol.ActionPutResponse{}, err
	}
	if tr.Resp == nil {
		return protocol.ActionPutResponse{}, errors.New("action: no response from the server")
	}
	r := tr.Resp.ActionPut()
	if tr.Resp.Kind != protocol.TaskControlKind_ActionPut || r == nil {
		return protocol.ActionPutResponse{}, fmt.Errorf("action: unexpected response kind=%v", tr.Resp.Kind)
	}
	return *r, nil
}

// ActionRemoveWith deletes one action.
func ActionRemoveWith(ctx context.Context, c *Client, id protocol.ActionID) (protocol.ActionStatus, error) {
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ActionRemove}}
	req.SetActionRemove(protocol.ActionRemoveRequest{Id: id})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, err
	}
	r := resp.ActionRemove()
	if resp.Kind != protocol.TaskControlKind_ActionRemove || r == nil {
		return 0, fmt.Errorf("action rm: unexpected response kind=%v", resp.Kind)
	}
	return r.Status, nil
}

// ResolveActionRef finds the action ref names: its full 32-hex id, or its
// name. A ref that is one action's id and another's name is refused rather
// than guessed: the two run different commands.
func ResolveActionRef(list []protocol.ActionRecord, ref string) (protocol.ActionRecord, error) {
	var byID, byName *protocol.ActionRecord
	for i := range list {
		if ActionIDHex(list[i].Id) == strings.ToLower(ref) {
			byID = &list[i]
		}
		if string(list[i].Definition.Name) == ref {
			byName = &list[i]
		}
	}
	switch {
	case byID != nil && byName != nil && byID.Id != byName.Id:
		return protocol.ActionRecord{}, fmt.Errorf("%q is both an action's id and another action's name; name it by the other's id", ref)
	case byID != nil:
		return *byID, nil
	case byName != nil:
		return *byName, nil
	}
	return protocol.ActionRecord{}, fmt.Errorf("no action %q (`action ls` lists them)", ref)
}

// ActionInvocationFromRecord builds the invocation of rec with values by
// param name, checked with the server's rule: a missing, unknown or refused
// value is an error here, before anything is sent.
func ActionInvocationFromRecord(rec protocol.ActionRecord, values map[string]string) (protocol.ActionInvocation, error) {
	vs, err := actiondef.Values(&rec.Definition, values)
	if err != nil {
		return protocol.ActionInvocation{}, err
	}
	if _, err := actiondef.Resolve(&rec.Definition, vs); err != nil {
		return protocol.ActionInvocation{}, err
	}
	inv := protocol.ActionInvocation{Id: rec.Id}
	inv.SetValues(vs)
	return inv, nil
}

// ActionInvocationFor resolves ref through action_list and builds its
// invocation: the one funnel `exec --action` takes on every surface.
func (c *Client) ActionInvocationFor(ctx context.Context, ref string, values map[string]string) (protocol.ActionInvocation, error) {
	list, err := ActionListWith(ctx, c)
	if err != nil {
		return protocol.ActionInvocation{}, err
	}
	rec, err := ResolveActionRef(list, ref)
	if err != nil {
		return protocol.ActionInvocation{}, err
	}
	inv, err := ActionInvocationFromRecord(rec, values)
	if err != nil {
		return protocol.ActionInvocation{}, fmt.Errorf("action %s: %w", rec.Definition.Name, err)
	}
	return inv, nil
}

// ActionTemplateWords is def's template as the words `action new … --`
// takes: ParseTemplate of them gives the template back. A literal that
// starts with '{' is escaped with a backslash.
func ActionTemplateWords(def *protocol.ActionDefinition) []string {
	out := make([]string, 0, len(def.Template))
	for i := range def.Template {
		e := &def.Template[i]
		if l := e.Literal(); l != nil {
			w := string(l.Bytes)
			if strings.HasPrefix(w, "{") {
				w = `\` + w
			}
			out = append(out, w)
			continue
		}
		if pi := e.ParamIndex(); pi != nil && int(*pi) < len(def.Params) {
			out = append(out, "{"+string(def.Params[*pi].Name)+"}")
		}
	}
	return out
}

// ActionTemplateString is the template for display, quoted as an exec argv
// is (ExecArgvString).
func ActionTemplateString(def *protocol.ActionDefinition) string {
	return ExecArgvString(ActionTemplateWords(def))
}

// ActionParamSpec is one param as --param takes it: "env=choice:a,b" or
// "ref=token".
func ActionParamSpec(p *protocol.ActionParam) string {
	if ch := p.Choices(); p.Type == protocol.ActionParamType_Choice && ch != nil {
		var cs []string
		for _, c := range ch.Choices {
			cs = append(cs, string(c.Bytes))
		}
		return string(p.Name) + "=choice:" + strings.Join(cs, ",")
	}
	return string(p.Name) + "=" + p.Type.String()
}

func actionTime(ms uint64) string {
	return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
}

// FormatActions renders the listing: per action, its id and counts, then the
// command it runs and each param; then the number of actions. Zero prints
// (params=0, actions=0): a count that is absent reads as "not reported".
func FormatActions(list []protocol.ActionRecord) string {
	var sb strings.Builder
	for i := range list {
		r := &list[i]
		d := &r.Definition
		fmt.Fprintf(&sb, "%s name=%s rev=%d params=%d edited=%s by=%s\n",
			ActionIDHex(r.Id), d.Name, r.Revision, len(d.Params), actionTime(r.EditedAtMs), r.EditedBy)
		fmt.Fprintf(&sb, "  run: %s\n", ActionTemplateString(d))
		for j := range d.Params {
			p := &d.Params[j]
			if ch := p.Choices(); ch != nil {
				var cs []string
				for _, c := range ch.Choices {
					cs = append(cs, fmt.Sprintf("%q", c.Bytes))
				}
				fmt.Fprintf(&sb, "  param %s: choice %s\n", p.Name, strings.Join(cs, " "))
			} else {
				fmt.Fprintf(&sb, "  param %s: %s\n", p.Name, p.Type)
			}
		}
		if len(d.Description) > 0 {
			fmt.Fprintf(&sb, "  description: %s\n", d.Description)
		}
	}
	fmt.Fprintf(&sb, "actions=%d\n", len(list))
	return sb.String()
}

type actionParamJSONRow struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Choices []string `json:"choices,omitempty"`
}

// FormatActionsJSON is one JSON object per action, carrying everything.
func FormatActionsJSON(list []protocol.ActionRecord) string {
	var sb strings.Builder
	for i := range list {
		r := &list[i]
		d := &r.Definition
		params := []actionParamJSONRow{}
		for j := range d.Params {
			p := &d.Params[j]
			row := actionParamJSONRow{Name: string(p.Name), Type: p.Type.String()}
			if ch := p.Choices(); ch != nil {
				for _, c := range ch.Choices {
					row.Choices = append(row.Choices, string(c.Bytes))
				}
			}
			params = append(params, row)
		}
		b, err := json.Marshal(struct {
			ID          string               `json:"id"`
			Name        string               `json:"name"`
			Revision    uint32               `json:"revision"`
			Params      []actionParamJSONRow `json:"params"`
			Template    []string             `json:"template"`
			Description string               `json:"description"`
			CreatedAt   string               `json:"created_at"`
			CreatedBy   string               `json:"created_by"`
			EditedAt    string               `json:"edited_at"`
			EditedBy    string               `json:"edited_by"`
		}{ActionIDHex(r.Id), string(d.Name), r.Revision, params, ActionTemplateWords(d), string(d.Description),
			actionTime(r.CreatedAtMs), string(r.CreatedBy), actionTime(r.EditedAtMs), string(r.EditedBy)})
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// FormatActionPut renders a put's answer; ok reports success. verb is "new"
// or "edit".
func FormatActionPut(r protocol.ActionPutResponse, verb, name string) (string, bool) {
	switch r.Status {
	case protocol.ActionStatus_Ok:
		return fmt.Sprintf("action %s %s: %s rev=%d", verb, name, ActionIDHex(r.Id), r.Revision), true
	case protocol.ActionStatus_HoldersUnreachable:
		if r.HoldersRefused > 0 {
			return fmt.Sprintf("action %s %s: refused: %d holder(s) of this action are outside what you could re-grant: %s", verb, name, r.HoldersRefused, r.Message), false
		}
		return fmt.Sprintf("action %s %s: refused: %s", verb, name, r.Message), false
	case protocol.ActionStatus_StaleRevision:
		return fmt.Sprintf("action %s %s: refused: %s", verb, name, r.Message), false
	case protocol.ActionStatus_NotFound:
		return fmt.Sprintf("action %s %s: no such action", verb, name), false
	}
	if len(r.Message) > 0 {
		return fmt.Sprintf("action %s %s: %s: %s", verb, name, r.Status, r.Message), false
	}
	return fmt.Sprintf("action %s %s: %s", verb, name, r.Status), false
}

// ExecRunActionLabel is "action=<name>@r<revision>" for an exec started from
// an action, "" for a raw argv. One rendering for every surface.
func ExecRunActionLabel(e *protocol.ExecRunInfo) string {
	if !e.ActionPresent() || e.ActionRevision() == nil || e.ActionName() == nil {
		return ""
	}
	return fmt.Sprintf("action=%s@r%d", *e.ActionName(), *e.ActionRevision())
}

// ActionPutInput is what `action new` / `action edit` carry, on every
// surface.
type ActionPutInput struct {
	Edit     bool
	Ref      string // edit: the action, by name or id
	Revision uint32 // edit: the revision being edited
	Name     string // "" on an edit keeps the current name
	// Description replaces the current one when DescriptionSet (an edit with
	// no --description keeps it; a new action without one has none).
	Description    string
	DescriptionSet bool
	Params         []protocol.ActionParam
	Template       []string // words, parsed with actiondef.ParseTemplate
	// Elements is the template as a screen's form builds it, one element per
	// row, so a literal needs no escaping. Used instead of Template; giving
	// both is an error.
	Elements []protocol.ActionElement
}

// RunActionPut is the one funnel the three surfaces put an action through:
// on an edit it resolves the ref and fills the kept name and description from
// the current record, builds the definition, checks it with the server's rule
// and sends it. It returns the line to show and whether it succeeded.
func (c *Client) RunActionPut(ctx context.Context, in ActionPutInput) (string, bool, error) {
	verb, id, expect := "new", protocol.ActionID{}, uint32(0)
	name, desc := in.Name, in.Description
	if in.Edit {
		verb, expect = "edit", in.Revision
		list, err := ActionListWith(ctx, c)
		if err != nil {
			return "", false, err
		}
		cur, err := ResolveActionRef(list, in.Ref)
		if err != nil {
			return "", false, fmt.Errorf("action edit: %w", err)
		}
		id = cur.Id
		if name == "" {
			name = string(cur.Definition.Name)
		}
		if !in.DescriptionSet {
			desc = string(cur.Definition.Description)
		}
	}
	def, err := buildActionDefinition(in, name, desc, verb)
	if err != nil {
		return "", false, err
	}
	r, err := ActionPutWith(ctx, c, id, expect, def)
	if err != nil {
		return "", false, err
	}
	line, ok := FormatActionPut(r, verb, name)
	return line, ok, nil
}

// buildActionDefinition is the definition RunActionPut sends: name and
// description as resolved, the params, and the template from the words or,
// from a screen, the elements.
func buildActionDefinition(in ActionPutInput, name, desc, verb string) (protocol.ActionDefinition, error) {
	var def protocol.ActionDefinition
	if !def.SetName([]byte(name)) {
		return def, fmt.Errorf("action %s: name too long", verb)
	}
	if desc != "" && !def.SetDescription([]byte(desc)) {
		return def, fmt.Errorf("action %s: description too long", verb)
	}
	if !def.SetParams(in.Params) {
		return def, fmt.Errorf("action %s: too many params", verb)
	}
	els := in.Elements
	switch {
	case els != nil && len(in.Template) != 0:
		return def, fmt.Errorf("action %s: give the command as words or as elements, not both", verb)
	case els == nil:
		var err error
		if els, err = actiondef.ParseTemplate(in.Template, in.Params); err != nil {
			return def, fmt.Errorf("action %s: %w", verb, err)
		}
	}
	if !def.SetTemplate(els) {
		return def, fmt.Errorf("action %s: the command has too many words", verb)
	}
	return def, nil
}

// RunActionRemove resolves ref and deletes it; the line to show and whether
// it succeeded.
func (c *Client) RunActionRemove(ctx context.Context, ref string) (string, bool, error) {
	list, err := ActionListWith(ctx, c)
	if err != nil {
		return "", false, err
	}
	rec, err := ResolveActionRef(list, ref)
	if err != nil {
		return "", false, fmt.Errorf("action rm: %w", err)
	}
	st, err := ActionRemoveWith(ctx, c, rec.Id)
	if err != nil {
		return "", false, err
	}
	if st != protocol.ActionStatus_Ok {
		return fmt.Sprintf("action rm %s: %s", rec.Definition.Name, st), false, nil
	}
	return fmt.Sprintf("action rm %s: removed %s (a grant naming it now runs nothing)", rec.Definition.Name, ActionIDHex(rec.Id)), true, nil
}

// ActionOverlapNames is the server's action_overlap as bit names, in a fixed
// order; [] (never nil) when there is none, so a JSON row always carries it.
func ActionOverlapNames(bits protocol.Capability) []string {
	out := []string{}
	for _, b := range []protocol.Capability{protocol.Capability_FileWrite, protocol.Capability_ExecCowrite, protocol.Capability_ExecControl} {
		if bits&b != 0 {
			out = append(out, b.String())
		}
	}
	return out
}

// ActionOverlapLabel is the row's warning (grant conditions spec, § Surfaces):
// the grant holds an ActionSet-conditioned exec_run and, over a target it also
// reaches, bits that can change what the action runs. "" when there is none —
// the warning is about a grant that has this shape, and most do not.
func ActionOverlapLabel(bits protocol.Capability) string {
	if bits == 0 {
		return ""
	}
	return "warn=action_overlap:" + strings.Join(ActionOverlapNames(bits), ",")
}
