package cli

import (
	"encoding/hex"
	"fmt"
	"reflect"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The data the TUI and WebUI screens for actions and runner administration
// render (docs/superpowers/specs/2026-11-27-admin-screens-actions-runner-keys-design.md).
// Plain, JSON-tagged structs, so the wasm bridge marshals them as they are;
// every write still goes through the funnels the CLI uses.

// ActionParamView is one param of an action. Type is "choice" or "token".
type ActionParamView struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Choices []string `json:"choices"`
}

// ActionElementView is one argv element: a literal, or a reference to a param
// by name.
type ActionElementView struct {
	Literal string `json:"literal"`
	Param   string `json:"param"`
	IsParam bool   `json:"isParam"`
}

// ActionView is one action as a screen lists and edits it. TemplateText is the
// CLI's spelling of the command.
type ActionView struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	Revision     uint32              `json:"revision"`
	Params       []ActionParamView   `json:"params"`
	Template     []ActionElementView `json:"template"`
	TemplateText string              `json:"templateText"`
	EditedBy     string              `json:"editedBy"`
	EditedAtMs   uint64              `json:"editedAtMs"`
}

// ActionViews is the list as screens show it, in the server's order.
func ActionViews(list []protocol.ActionRecord) []ActionView {
	out := make([]ActionView, 0, len(list))
	for i := range list {
		r := &list[i]
		d := &r.Definition
		v := ActionView{
			ID: ActionIDHex(r.Id), Name: string(d.Name), Description: string(d.Description),
			Revision: r.Revision, TemplateText: ActionTemplateString(d),
			EditedBy: string(r.EditedBy), EditedAtMs: r.EditedAtMs,
			Params: []ActionParamView{}, Template: []ActionElementView{},
		}
		for j := range d.Params {
			p := &d.Params[j]
			pv := ActionParamView{Name: string(p.Name), Type: EnumWireName(p.Type.String()), Choices: []string{}}
			if ch := p.Choices(); ch != nil {
				for _, c := range ch.Choices {
					pv.Choices = append(pv.Choices, string(c.Bytes))
				}
			}
			v.Params = append(v.Params, pv)
		}
		for j := range d.Template {
			e := &d.Template[j]
			if l := e.Literal(); l != nil {
				v.Template = append(v.Template, ActionElementView{Literal: string(l.Bytes)})
			} else if ix := e.ParamIndex(); ix != nil && int(*ix) < len(d.Params) {
				v.Template = append(v.Template, ActionElementView{IsParam: true, Param: string(d.Params[*ix].Name)})
			}
		}
		out = append(out, v)
	}
	return out
}

// ActionForm is what a screen's form submits. Ref set = an edit of that
// action at Revision.
type ActionForm struct {
	Ref         string              `json:"ref"`
	Revision    uint32              `json:"revision"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Params      []ActionParamView   `json:"params"`
	Template    []ActionElementView `json:"template"`
}

// ActionPutInputFromForm turns a form into the input RunActionPut sends, and
// refuses — with the server's own rule, so the message names it — anything
// the server would refuse. The template travels as elements, never as words.
func ActionPutInputFromForm(f ActionForm) (ActionPutInput, error) {
	in := ActionPutInput{Edit: f.Ref != "", Ref: f.Ref, Revision: f.Revision, Name: f.Name,
		Description: f.Description, DescriptionSet: true}
	index := map[string]uint8{}
	for i, pv := range f.Params {
		var p protocol.ActionParam
		p.SetName([]byte(pv.Name))
		switch pv.Type {
		case "token":
			p.Type = protocol.ActionParamType_Token
		case "choice":
			p.Type = protocol.ActionParamType_Choice
			var ls []protocol.ActionLiteral
			for _, c := range pv.Choices {
				var l protocol.ActionLiteral
				l.SetBytes([]byte(c))
				ls = append(ls, l)
			}
			var ch protocol.ActionChoices
			ch.SetChoices(ls)
			p.SetChoices(ch)
		default:
			return in, fmt.Errorf("param %q: unknown type %q (choice or token)", pv.Name, pv.Type)
		}
		index[pv.Name] = uint8(i)
		in.Params = append(in.Params, p)
	}
	in.Elements = []protocol.ActionElement{}
	for _, ev := range f.Template {
		if ev.IsParam {
			ix, ok := index[ev.Param]
			if !ok {
				return in, fmt.Errorf("the command names param %q, which is not declared", ev.Param)
			}
			e := protocol.ActionElement{Kind: protocol.ActionElementKind_Param}
			e.SetParamIndex(ix)
			in.Elements = append(in.Elements, e)
			continue
		}
		var l protocol.ActionLiteral
		l.SetBytes([]byte(ev.Literal))
		e := protocol.ActionElement{Kind: protocol.ActionElementKind_Literal}
		e.SetLiteral(l)
		in.Elements = append(in.Elements, e)
	}
	def, err := buildActionDefinition(in, in.Name, in.Description, "form")
	if err != nil {
		return in, err
	}
	if err := actiondef.Validate(&def); err != nil {
		return in, err
	}
	if err := restatable(&def); err != nil {
		return in, err
	}
	return in, nil
}

// restatable refuses a definition the command line could not write back:
// `action edit` restates the template as words (ActionTemplateWords), and
// ParseTemplate of them must give the same elements. A literal with a
// declared {param} inside other text is refused there, and one starting with
// \{ would come back without its backslash.
func restatable(def *protocol.ActionDefinition) error {
	words := ActionTemplateWords(def)
	els, err := actiondef.ParseTemplate(words, def.Params)
	if err != nil {
		return fmt.Errorf("the command cannot be written on the command line: %w", err)
	}
	for i := range def.Template {
		if i >= len(els) || !reflect.DeepEqual(els[i], def.Template[i]) {
			return fmt.Errorf("argument %d %q cannot be written on the command line: a word starting with \\{ is read as an escaped {", i, words[i])
		}
	}
	return nil
}

// RunnerKeyView is one enrolled key. Bound holds full task ids; Runners the
// connected runners on the key, as "<id8> <hostname>".
type RunnerKeyView struct {
	Key          string   `json:"key"`
	Prefix       string   `json:"prefix"`
	Label        string   `json:"label"`
	EnrolledBy   string   `json:"enrolledBy"`
	EnrolledAtMs int64    `json:"enrolledAtMs"`
	Bound        []string `json:"bound"`
	Runners      []string `json:"runners"`
}

func runnerUserLabel(u RunnerKeyUser) string {
	return hex.EncodeToString(u.ID.Id[:4]) + " " + u.Hostname
}

// RunnerKeyViews is `runner keys` as screens show it.
func RunnerKeyViews(rows []RunnerKeyRow) []RunnerKeyView {
	out := make([]RunnerKeyView, 0, len(rows))
	for _, r := range rows {
		v := RunnerKeyView{Key: hex.EncodeToString(r.Key), Prefix: KeyPrefix16(r.Key), Label: r.Label,
			EnrolledBy: r.EnrolledBy, Bound: []string{}, Runners: []string{}}
		if !r.EnrolledAt.IsZero() {
			v.EnrolledAtMs = r.EnrolledAt.UnixMilli()
		}
		for _, id := range r.Bound {
			v.Bound = append(v.Bound, hex.EncodeToString(id.Id[:]))
		}
		for _, u := range r.Runners {
			v.Runners = append(v.Runners, runnerUserLabel(u))
		}
		out = append(out, v)
	}
	return out
}

// RunnerDialView is one dial record. ID is the 8-hex form `runner dial rm`
// takes.
type RunnerDialView struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	KeyPrefix  string   `json:"keyPrefix"`
	State      string   `json:"state"`
	LastError  string   `json:"lastError"`
	Candidates []string `json:"candidates"`
}

// RunnerDialViews is `runner dial ls` as screens show it.
func RunnerDialViews(rows []RunnerDialRow) []RunnerDialView {
	out := make([]RunnerDialView, 0, len(rows))
	for _, r := range rows {
		out = append(out, RunnerDialView{ID: FormatDialID(r.ID), Label: r.Label, KeyPrefix: KeyPrefix16(r.Key),
			State: EnumWireName(r.State.String()), LastError: r.LastError, Candidates: append([]string{}, r.Candidates...)})
	}
	return out
}

// ForceOffer is the answer to a destructive request a screen may repeat with
// force: NeedsForce only on in_use, with what is in the way. Text is the CLI's
// line.
type ForceOffer struct {
	Text       string   `json:"text"`
	OK         bool     `json:"ok"`
	NeedsForce bool     `json:"needsForce"`
	InTheWay   []string `json:"inTheWay"`
}

// UnenrollOffer is an unenroll's answer; in the way are the runners on the key.
func UnenrollOffer(st protocol.RunnerKeyStatus, res RunnerUnenrollResult) ForceOffer {
	text, ok := FormatRunnerUnenroll(st, res)
	o := ForceOffer{Text: text, OK: ok, InTheWay: []string{}}
	if st == protocol.RunnerKeyStatus_InUse {
		o.NeedsForce = true
		for _, u := range res.Runners {
			o.InTheWay = append(o.InTheWay, runnerUserLabel(u))
		}
	}
	return o
}

// KickOffer is a kick's answer; in the way are the runner's tasks.
func KickOffer(st protocol.RunnerKickStatus, res RunnerKickResult) ForceOffer {
	text, ok := FormatRunnerKick(st, res)
	o := ForceOffer{Text: text, OK: ok, InTheWay: []string{}}
	if st == protocol.RunnerKickStatus_InUse {
		o.NeedsForce = true
		for _, id := range res.Tasks {
			o.InTheWay = append(o.InTheWay, hex.EncodeToString(id.Id[:]))
		}
	}
	return o
}
