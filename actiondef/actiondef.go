// Package actiondef is the one validation rule for server-defined actions
// (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C12, C17): the server applies it before storing a definition and before
// resolving an invocation, and the CLI and the wasm build apply it before
// sending, so a client refuses a bad definition with the server's own words.
//
// Pure functions over the wire types; no I/O.
package actiondef

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The limits of § Actions, Wire. A choice is at most 255 bytes so that
// ActionValue (len :u8) can carry it at invocation.
const (
	MaxNameLen        = 64
	MaxParamNameLen   = 32
	MaxParams         = 16
	MaxTemplate       = 64
	MaxChoices        = 32
	MaxChoiceLen      = 255
	MaxLiteralLen     = 4096
	MaxDescriptionLen = 1024
	MaxTokenLen       = 128
)

// ValidToken is the token rule (C12): 1-128 bytes of [0-9A-Za-z._-], not
// starting with '-' (a value that reads as an option). Fixed code: no caller
// supplies a pattern, so no caller can widen it.
func ValidToken(v []byte) bool {
	if len(v) == 0 || len(v) > MaxTokenLen || v[0] == '-' {
		return false
	}
	for _, c := range v {
		if !tokenByte(c) {
			return false
		}
	}
	return true
}

func tokenByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '_' || c == '-'
}

// validName is [a-z0-9][a-z0-9._-]{0,63}.
func validName(n []byte) bool {
	if len(n) == 0 || len(n) > MaxNameLen {
		return false
	}
	for i, c := range n {
		lowerOrDigit := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if i == 0 && !lowerOrDigit || i > 0 && !lowerOrDigit && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// validParamName is [a-z][a-z0-9_]{0,31}.
func validParamName(n []byte) bool {
	if len(n) == 0 || len(n) > MaxParamNameLen {
		return false
	}
	for i, c := range n {
		lower := c >= 'a' && c <= 'z'
		if i == 0 && !lower || i > 0 && !lower && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

func checkLiteral(what string, b []byte, max int) error {
	if len(b) == 0 || len(b) > max {
		return fmt.Errorf("%s must be 1-%d bytes, got %d", what, max, len(b))
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return fmt.Errorf("%s contains a NUL byte", what)
	}
	return nil
}

// Validate reports the first rule def breaks (C12, C17, the limits above),
// naming the rule.
func Validate(def *protocol.ActionDefinition) error {
	if !validName(def.Name) {
		return fmt.Errorf("name %q must match [a-z0-9][a-z0-9._-]{0,63}", def.Name)
	}
	if len(def.Description) > MaxDescriptionLen {
		return fmt.Errorf("description is %d bytes, at most %d", len(def.Description), MaxDescriptionLen)
	}
	if len(def.Params) > MaxParams {
		return fmt.Errorf("%d params, at most %d", len(def.Params), MaxParams)
	}
	seen := map[string]bool{}
	for i := range def.Params {
		p := &def.Params[i]
		if !validParamName(p.Name) {
			return fmt.Errorf("param name %q must match [a-z][a-z0-9_]{0,31}", p.Name)
		}
		if seen[string(p.Name)] {
			return fmt.Errorf("param name %q is declared twice", p.Name)
		}
		seen[string(p.Name)] = true
		switch p.Type {
		case protocol.ActionParamType_Token:
		case protocol.ActionParamType_Choice:
			ch := p.Choices()
			if ch == nil || len(ch.Choices) == 0 || len(ch.Choices) > MaxChoices {
				n := 0
				if ch != nil {
					n = len(ch.Choices)
				}
				return fmt.Errorf("param %q has %d choices, want 1-%d", p.Name, n, MaxChoices)
			}
			dup := map[string]bool{}
			for _, c := range ch.Choices {
				if err := checkLiteral(fmt.Sprintf("a choice of param %q", p.Name), c.Bytes, MaxChoiceLen); err != nil {
					return err
				}
				if dup[string(c.Bytes)] {
					return fmt.Errorf("param %q lists the choice %q twice", p.Name, c.Bytes)
				}
				dup[string(c.Bytes)] = true
			}
		default:
			return fmt.Errorf("param %q has an unknown type %d", p.Name, p.Type)
		}
	}
	if len(def.Template) == 0 || len(def.Template) > MaxTemplate {
		return fmt.Errorf("template has %d elements, want 1-%d", len(def.Template), MaxTemplate)
	}
	used := make([]bool, len(def.Params))
	for i := range def.Template {
		e := &def.Template[i]
		switch e.Kind {
		case protocol.ActionElementKind_Literal:
			l := e.Literal()
			if l == nil {
				return fmt.Errorf("template element %d is a literal with no bytes", i)
			}
			if err := checkLiteral(fmt.Sprintf("template literal %d", i), l.Bytes, MaxLiteralLen); err != nil {
				return err
			}
		case protocol.ActionElementKind_Param:
			if i == 0 {
				return errors.New("template element 0 is a param: the program must be a literal, so no value chooses what runs")
			}
			pi := e.ParamIndex()
			if pi == nil || int(*pi) >= len(def.Params) {
				return fmt.Errorf("template element %d: param index out of range", i)
			}
			used[*pi] = true
		default:
			return fmt.Errorf("template element %d has an unknown kind %d", i, e.Kind)
		}
	}
	for i, u := range used {
		if !u {
			return fmt.Errorf("param %q is not used in the template", def.Params[i].Name)
		}
	}
	return nil
}

// checkValue checks one value against its param.
func checkValue(p *protocol.ActionParam, v []byte) error {
	switch p.Type {
	case protocol.ActionParamType_Token:
		if !ValidToken(v) {
			return fmt.Errorf("value %q for %s is not a token (1-128 bytes of [0-9A-Za-z._-], not starting with -)", v, p.Name)
		}
	case protocol.ActionParamType_Choice:
		if ch := p.Choices(); ch != nil {
			for _, c := range ch.Choices {
				if bytes.Equal(c.Bytes, v) {
					return nil
				}
			}
		}
		return fmt.Errorf("value %q for %s is not one of its choices", v, p.Name)
	default:
		return fmt.Errorf("param %s has an unknown type", p.Name)
	}
	return nil
}

// Resolve checks values against def's params and returns the argv: literals
// as written, each param element replaced by its value as ONE element.
func Resolve(def *protocol.ActionDefinition, values []protocol.ActionValue) ([]string, error) {
	if len(values) != len(def.Params) {
		return nil, fmt.Errorf("%d values for %d params", len(values), len(def.Params))
	}
	for i := range def.Params {
		if err := checkValue(&def.Params[i], values[i].Bytes); err != nil {
			return nil, err
		}
	}
	argv := make([]string, 0, len(def.Template))
	for i := range def.Template {
		e := &def.Template[i]
		switch e.Kind {
		case protocol.ActionElementKind_Literal:
			l := e.Literal()
			if l == nil {
				return nil, fmt.Errorf("template element %d is a literal with no bytes", i)
			}
			argv = append(argv, string(l.Bytes))
		case protocol.ActionElementKind_Param:
			pi := e.ParamIndex()
			if pi == nil || int(*pi) >= len(values) {
				return nil, fmt.Errorf("template element %d: param index out of range", i)
			}
			argv = append(argv, string(values[*pi].Bytes))
		default:
			return nil, fmt.Errorf("template element %d has an unknown kind", i)
		}
	}
	return argv, nil
}

// ParseTemplate turns CLI words into elements:
//   - a word that is exactly "{name}" for a declared param is a reference;
//   - a word starting with "\{" is a literal with that backslash dropped;
//   - a word that contains "{name}" for a declared param anywhere else (e.g.
//     "--env={env}") is refused: there is no partial substitution, and
//     passing it through as a literal would surprise whoever wrote it;
//   - every other word is a literal.
func ParseTemplate(words []string, params []protocol.ActionParam) ([]protocol.ActionElement, error) {
	index := map[string]uint8{}
	for i, p := range params {
		index[string(p.Name)] = uint8(i)
	}
	out := make([]protocol.ActionElement, 0, len(words))
	for _, w := range words {
		if strings.HasPrefix(w, `\{`) {
			out = append(out, literalElement(w[1:]))
			continue
		}
		if strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") {
			if i, ok := index[w[1:len(w)-1]]; ok {
				e := protocol.ActionElement{Kind: protocol.ActionElementKind_Param}
				e.SetParamIndex(i)
				out = append(out, e)
				continue
			}
		}
		for name := range index {
			if strings.Contains(w, "{"+name+"}") {
				return nil, fmt.Errorf("word %q puts {%s} inside other text; a param value is always a whole argument (write it as its own word)", w, name)
			}
		}
		out = append(out, literalElement(w))
	}
	return out, nil
}

func literalElement(s string) protocol.ActionElement {
	var l protocol.ActionLiteral
	l.SetBytes([]byte(s))
	e := protocol.ActionElement{Kind: protocol.ActionElementKind_Literal}
	e.SetLiteral(l)
	return e
}

// ParseParam parses one --param: "NAME=token" or "NAME=choice:A,B,...". A
// choice containing a comma cannot be written here.
func ParseParam(spec string) (protocol.ActionParam, error) {
	name, typ, ok := strings.Cut(spec, "=")
	if !ok {
		return protocol.ActionParam{}, fmt.Errorf("--param %q: want NAME=token or NAME=choice:A,B,...", spec)
	}
	if !validParamName([]byte(name)) {
		return protocol.ActionParam{}, fmt.Errorf("--param %q: name must match [a-z][a-z0-9_]{0,31}", spec)
	}
	var p protocol.ActionParam
	p.SetName([]byte(name))
	switch {
	case typ == "token":
		p.Type = protocol.ActionParamType_Token
	case strings.HasPrefix(typ, "choice:"):
		list := strings.TrimPrefix(typ, "choice:")
		if list == "" {
			return protocol.ActionParam{}, fmt.Errorf("--param %q: a choice needs at least one value", spec)
		}
		var ls []protocol.ActionLiteral
		for _, c := range strings.Split(list, ",") {
			var l protocol.ActionLiteral
			l.SetBytes([]byte(c))
			ls = append(ls, l)
		}
		var ch protocol.ActionChoices
		if !ch.SetChoices(ls) {
			return protocol.ActionParam{}, fmt.Errorf("--param %q: too many choices", spec)
		}
		p.Type = protocol.ActionParamType_Choice
		p.SetChoices(ch)
	default:
		return protocol.ActionParam{}, fmt.Errorf("--param %q: type must be token or choice:A,B,...", spec)
	}
	return p, nil
}

// Values orders named values by def's params, refusing a missing or an
// unknown name. The values themselves are checked by Resolve.
func Values(def *protocol.ActionDefinition, byName map[string]string) ([]protocol.ActionValue, error) {
	out := make([]protocol.ActionValue, 0, len(def.Params))
	for i := range def.Params {
		name := string(def.Params[i].Name)
		v, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("no value for param %s (--value %s=...)", name, name)
		}
		var av protocol.ActionValue
		if !av.SetBytes([]byte(v)) {
			return nil, fmt.Errorf("value for %s is longer than 255 bytes", name)
		}
		out = append(out, av)
	}
	for name := range byName {
		known := false
		for i := range def.Params {
			known = known || string(def.Params[i].Name) == name
		}
		if !known {
			return nil, fmt.Errorf("action %s has no param %s", def.Name, name)
		}
	}
	return out, nil
}
