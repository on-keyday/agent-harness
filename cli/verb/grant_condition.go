package verb

import (
	"encoding/hex"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Grant conditions (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C4-C7, C9, C14): a condition bounds which object inside a target an
// override's bits reach. It attaches to one --scope-for override, the one whose
// capability list is exactly the condition's.
//
//	--grant-condition CAPS=ports:<p>[-<q>][,…]         forward_local / forward_remote (the port the runner dials for -L, binds for -R)
//	--grant-condition CAPS=actions:<32 hex>[,…]        exec_run (server-defined actions)
//
// ':' is accepted in place of '=' because that is how ConditionsLabel prints
// it, as --scope-for accepts the OverridesLabel spelling.

// GrantConditionHelp is the --grant-condition help line, shared by every verb
// that takes it.
const GrantConditionHelp = "bound which object a --scope-for override's capabilities reach, as CAPS=ports:P[-Q],… " +
	"(forward_local / forward_remote) or CAPS=actions:ID,… (exec_run); CAPS must equal one --scope-for's list. Repeatable"

// grantConditionValue backs --grant-condition: the raw spellings in order,
// parsed by parseGrantConditionList.
var grantConditionValue = &CustomValue{
	New: func() flag.Value { return &stringList{} },
	Get: func(v flag.Value) any {
		if l, ok := v.(*stringList); ok {
			return []string(*l)
		}
		return []string(nil)
	},
}

// ParseGrantCondition parses one --grant-condition value. It refuses what the
// server would: a kind on a bit it does not apply to, a list over its limit, a
// backwards range; and port 0, which no forward uses.
func ParseGrantCondition(str string) (protocol.GrantCondition, error) {
	fail := func(format string, args ...any) (protocol.GrantCondition, error) {
		return protocol.GrantCondition{}, fmt.Errorf("--grant-condition %q: %s", str, fmt.Sprintf(format, args...))
	}
	capsText, body, ok := strings.Cut(str, "=")
	if !ok {
		capsText, body, ok = strings.Cut(str, ":")
	}
	if !ok {
		return fail("want CAPS=ports:P[-Q],… or CAPS=actions:ID,…")
	}
	caps, err := ParseCaps(strings.TrimSpace(capsText))
	if err != nil {
		return fail("%v", err)
	}
	if caps == protocol.Capability_None {
		return fail("names no capability")
	}
	kindWord, list, ok := strings.Cut(strings.TrimSpace(body), ":")
	if !ok || strings.TrimSpace(list) == "" {
		return fail("want ports:P[-Q],… or actions:ID,… after the capabilities")
	}
	w := protocol.GrantCondition{Caps: caps}
	switch kindWord {
	case "ports":
		w.Kind = protocol.ConditionKind_PortRange
		var spans []protocol.PortSpan
		for _, one := range strings.Split(list, ",") {
			lo, hi, isRange := strings.Cut(strings.TrimSpace(one), "-")
			l, err := parsePort(lo)
			if err != nil {
				return fail("%v", err)
			}
			h := l
			if isRange {
				if h, err = parsePort(hi); err != nil {
					return fail("%v", err)
				}
			}
			if l > h {
				return fail("port range %d-%d runs backwards", l, h)
			}
			spans = append(spans, protocol.PortSpan{Lo: l, Hi: h})
		}
		var p protocol.PortRangeCondition
		p.SetSpans(spans)
		w.SetPortRange(p)
	case "actions":
		w.Kind = protocol.ConditionKind_ActionSet
		var ids []protocol.ActionID
		for _, one := range strings.Split(list, ",") {
			raw, err := hex.DecodeString(strings.TrimSpace(one))
			if err != nil || len(raw) != 16 {
				return fail("action id %q: want 32 hex characters", one)
			}
			var id protocol.ActionID
			copy(id.Id[:], raw)
			ids = append(ids, id)
		}
		var a protocol.ActionSetCondition
		a.SetActions(ids)
		w.SetActionSet(a)
	default:
		return fail("unknown condition kind %q (ports or actions)", kindWord)
	}
	// The server's normalisation and applicability rule, applied here so the
	// mistake costs a parse error rather than a round trip.
	c, err := authority.ConditionFromWire(w)
	if err != nil {
		return fail("%v", err)
	}
	return c.ToWire(caps), nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("port %q: want 1..65535", s)
	}
	return uint16(n), nil
}

// ParseGrantConditionList is parseGrantConditionList for a caller outside the
// declaration (the wasm bridge, which receives the spellings from a form).
func ParseGrantConditionList(specs []string) ([]protocol.GrantCondition, error) {
	return parseGrantConditionList(specs)
}

// parseGrantConditionList is --grant-condition, repeatable: at most
// authority.MaxGrantConditions, at most one per capability list.
func parseGrantConditionList(specs []string) ([]protocol.GrantCondition, error) {
	var out []protocol.GrantCondition
	for _, one := range specs {
		c, err := ParseGrantCondition(one)
		if err != nil {
			return nil, err
		}
		for _, prev := range out {
			if prev.Caps == c.Caps {
				return nil, fmt.Errorf("--grant-condition: %s named twice; one condition per --scope-for", CapsLabel(c.Caps))
			}
		}
		out = append(out, c)
	}
	if len(out) > authority.MaxGrantConditions {
		return nil, fmt.Errorf("--grant-condition: %d given; at most %d", len(out), authority.MaxGrantConditions)
	}
	return out, nil
}

// MarkGrantConditions returns a copy of overrides with the override each
// condition belongs to marked conditioned. Each condition's capability list
// must equal one override's exactly: a condition attaches to one --scope-for
// override, never to the base scope, and never to part of an override.
func MarkGrantConditions(overrides []protocol.ScopeOverride, conds []protocol.GrantCondition) ([]protocol.ScopeOverride, error) {
	out := append([]protocol.ScopeOverride(nil), overrides...)
	for _, c := range conds {
		i := -1
		for j, o := range out {
			if o.Caps == c.Caps {
				i = j
				break
			}
		}
		if i < 0 {
			return nil, fmt.Errorf("--grant-condition %s: no --scope-for names exactly %s; add --scope-for %s=<scope> (a condition attaches to one override)",
				CapsLabel(c.Caps), CapsLabel(c.Caps), CapsLabel(c.Caps))
		}
		out[i].SetConditioned(true)
	}
	return out, nil
}

// grantConditionFor is the body naming exactly caps, or nil.
func grantConditionFor(caps protocol.Capability, conds []protocol.GrantCondition) *authority.Condition {
	for _, c := range conds {
		if c.Caps == caps {
			if cond, err := authority.ConditionFromWire(c); err == nil {
				return cond
			}
			return &authority.Condition{}
		}
	}
	return nil
}

// conditionText is one conditioned override's condition: its body, or
// "(condition unknown)" when only the marker is in hand — a conditioned grant
// never reads as unconditioned (C7).
func conditionText(caps protocol.Capability, conds []protocol.GrantCondition) string {
	if c := grantConditionFor(caps, conds); c != nil {
		return c.Label()
	}
	return (&authority.Condition{}).Label()
}

// ConditionsLabel renders the conditioned overrides as CAPS:CONDITION joined
// by spaces, the spelling --grant-condition accepts. Empty when none is
// conditioned. Printed beside OverridesLabel, which stays the pasteable
// --scope-for form.
func ConditionsLabel(overrides []protocol.ScopeOverride, conds []protocol.GrantCondition) string {
	var parts []string
	for _, o := range overrides {
		if o.Conditioned() {
			parts = append(parts, CapsLabel(o.Caps)+":"+conditionText(o.Caps, conds))
		}
	}
	return strings.Join(parts, " ")
}

// ConditionsByCap is the condition on each held capability, for machine
// readers beside scope_by_cap: a bit with no entry is unconditioned. Keyed per
// bit, as scope_by_cap is, so a forward_local condition does not read as
// covering forward_remote.
func ConditionsByCap(caps protocol.Capability, overrides []protocol.ScopeOverride, conds []protocol.GrantCondition) map[string]string {
	out := map[string]string{}
	for _, o := range overrides {
		if !o.Conditioned() {
			continue
		}
		text := conditionText(o.Caps, conds)
		for _, bit := range GrantableCaps() {
			if bit == protocol.Capability_None || bit == protocol.Capability_All {
				continue
			}
			if o.Caps&bit == bit && caps&bit == bit {
				out[bit.String()] = text
			}
		}
	}
	return out
}
