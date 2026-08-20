package cli

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A capability names a verb; a scope names which tasks that verb may be
// pointed at. The grammar is deliberately the same in both directions, so what
// `ls` prints can be pasted straight back into a `--scope`:
//
//	(omitted)                 self + descendants — the default
//	subtree                   the same, written out
//	none                      self only
//	global                    every task on the server
//	descendants               subtree WITHOUT self
//	none-self                 the empty action set: holds the bit, points nowhere
//	ids:<id>[,<id>]           self + exactly those tasks
//	subtree+ids:<id>[,<id>]   self + descendants + those tasks
//	global/subtree            visibility global, actions on the subtree
//	subtree+vis-ids:<id>      acts on the subtree, additionally SEES that task
//
// A bare ids: list implies base none, so `ids:X` and `none+ids:X` parse
// identically. `global+ids:` is rejected rather than accepted-and-ignored: a
// scope the user wrote and the server silently drops is the invisible
// divergence this design refuses everywhere else. (The SERVER accepts that
// combination as a redundant no-op — a redundant grant must not become a
// failed spawn — so this is a client-side lint that catches a typo one round
// trip earlier, not a disagreement about the model.)
//
// `descendants` is a UI word, not a wire value: it is base=subtree with
// exclude_self set. Rendering picks exactly one spelling per value, so the
// round trip stays stable.
//
// The optional `<visibility>/` prefix is the visibility rank. Omitting it
// means visibility follows the action base, which is what pins the rank pair
// to the diagonal and makes every default legal by construction. exclude_self
// never appears on the visibility side: self is always visible.

const scopeIDHexLen = 32

// ScopeGrammar is the one-line syntax summary, shared by flag help and
// `harness-cli caps`.
const ScopeGrammar = "[<visibility>/]<action> where each is subtree (default) | none | global, " +
	"action also descendants (subtree without self) or <base>-self; " +
	"plus [+ids:<task-id>[,<task-id>]] and [+vis-ids:<task-id>[,<task-id>]]"

// ParseScope converts a --scope value into a wire TaskScope. Empty is the
// subtree default, which is also the zero value.
func ParseScope(str string) (protocol.TaskScope, error) {
	str = strings.TrimSpace(str)
	if str == "" {
		return protocol.TaskScope{Base: protocol.ScopeBase_Subtree}, nil
	}

	// Visibility prefix. Task ids are hex, so a slash can only be this
	// separator and splitting on the first one is unambiguous.
	visPart, actPart, hasVis := strings.Cut(str, "/")
	if !hasVis {
		actPart = str
	}

	// vis-ids ride on the action half's text, since they are written after it.
	actPart, visIDText, hasVisIDs := cutScopeIDList(actPart, "vis-ids:")

	out, err := parseScopeAction(actPart)
	if err != nil {
		return protocol.TaskScope{}, err
	}
	if hasVisIDs {
		ids, err := parseScopeIDs(visIDText, str)
		if err != nil {
			return protocol.TaskScope{}, err
		}
		out.VisIds = ids
		out.VisIdsLen = uint16(len(ids))
	}
	if hasVis {
		base, excludeSelf, err := parseScopeBaseWord(strings.TrimSpace(visPart))
		if err != nil {
			return protocol.TaskScope{}, err
		}
		if excludeSelf {
			return protocol.TaskScope{}, fmt.Errorf(
				"scope %q: self is always visible, so the visibility half cannot exclude it", str)
		}
		out.VisBase = base
		out.SetVisBasePresent(true)
	}
	return out, nil
}

// cutScopeIDList splits off a trailing "+<marker><ids>" clause.
func cutScopeIDList(s, marker string) (head, ids string, found bool) {
	i := strings.Index(s, marker)
	if i < 0 {
		return s, "", false
	}
	head = strings.TrimSuffix(strings.TrimSpace(s[:i]), "+")
	return head, s[i+len(marker):], true
}

// parseScopeBaseWord maps one base word, including the UI-only spellings that
// carry exclude_self.
func parseScopeBaseWord(w string) (protocol.ScopeBase, bool, error) {
	if w == "descendants" {
		return protocol.ScopeBase_Subtree, true, nil
	}
	base, excludeSelf := w, false
	if trimmed, ok := strings.CutSuffix(w, "-self"); ok {
		base, excludeSelf = trimmed, true
	}
	switch base {
	case "subtree":
		return protocol.ScopeBase_Subtree, excludeSelf, nil
	case "none":
		return protocol.ScopeBase_None, excludeSelf, nil
	case "global":
		return protocol.ScopeBase_Global, excludeSelf, nil
	default:
		return 0, false, fmt.Errorf("unknown scope base %q (valid: %s)", w, ScopeGrammar)
	}
}

// parseScopeAction parses the action half: a base word plus an optional id list.
func parseScopeAction(str string) (protocol.TaskScope, error) {
	str = strings.TrimSpace(str)
	baseText, idText, hasIDs := strings.Cut(str, "ids:")
	baseText = strings.TrimSuffix(strings.TrimSpace(baseText), "+")

	var out protocol.TaskScope
	switch {
	case baseText == "" && hasIDs:
		// A bare "ids:…" has no written base; none is the implied one.
		out.Base = protocol.ScopeBase_None
	case baseText == "":
		return protocol.TaskScope{}, fmt.Errorf("empty scope (valid: %s)", ScopeGrammar)
	default:
		base, excludeSelf, err := parseScopeBaseWord(baseText)
		if err != nil {
			return protocol.TaskScope{}, err
		}
		if base == protocol.ScopeBase_Global && hasIDs {
			return protocol.TaskScope{}, fmt.Errorf(
				"scope %q: ids are meaningless under global, which already covers every task", str)
		}
		out.Base = base
		out.SetExcludeSelf(excludeSelf)
	}
	if !hasIDs {
		return out, nil
	}
	ids, err := parseScopeIDs(idText, str)
	if err != nil {
		return protocol.TaskScope{}, err
	}
	out.Ids = ids
	out.IdsLen = uint16(len(ids))
	return out, nil
}

// parseScopeIDs validates, sorts and dedupes one comma-separated id list.
func parseScopeIDs(list, whole string) ([]protocol.TaskID, error) {
	seen := make(map[string]bool)
	var ids []string
	for _, raw := range strings.Split(list, ",") {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if len(id) != scopeIDHexLen {
			return nil, fmt.Errorf("scope id %q: want %d hex characters, got %d", raw, scopeIDHexLen, len(id))
		}
		if _, err := hex.DecodeString(id); err != nil {
			return nil, fmt.Errorf("scope id %q is not hex", raw)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("scope %q: ids: with no ids after it", whole)
	}
	sort.Strings(ids)
	out := make([]protocol.TaskID, 0, len(ids))
	for _, id := range ids {
		raw, _ := hex.DecodeString(id)
		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		out = append(out, tid)
	}
	return out, nil
}

// scopeBaseWord is the rendering half of parseScopeBaseWord: exactly one
// spelling per value, so ScopeLabel output parses back to what it came from.
func scopeBaseWord(base protocol.ScopeBase, excludeSelf bool) string {
	if !excludeSelf {
		return base.String()
	}
	if base == protocol.ScopeBase_Subtree {
		return "descendants"
	}
	return base.String() + "-self"
}

// ScopeLabel renders a TaskScope in the grammar ParseScope accepts. The zero
// value renders as "subtree".
func ScopeLabel(s protocol.TaskScope) string {
	out := scopeBaseWord(s.Base, s.ExcludeSelf())
	if n := len(s.Ids); n > 0 {
		list := "ids:" + joinTaskIDs(s.Ids)
		if s.Base == protocol.ScopeBase_None && !s.ExcludeSelf() {
			// none is the implied base of a bare id list; writing it would be noise.
			out = list
		} else {
			out += "+" + list
		}
	}
	if len(s.VisIds) > 0 {
		out += "+vis-ids:" + joinTaskIDs(s.VisIds)
	}
	if s.VisBasePresent() {
		out = s.VisBase.String() + "/" + out
	}
	return out
}

func joinTaskIDs(in []protocol.TaskID) string {
	ids := make([]string, 0, len(in))
	for _, id := range in {
		ids = append(ids, hex.EncodeToString(id.Id[:]))
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// ScopeSpec builds a `--scope` string from the pieces a graphical control can
// edit, carrying forward the halves it cannot.
//
// base is a rank word ("subtree"/"none"/"global"), excludeSelf is the flag that
// turns each into its without-self twin, and ids is the target list. carry is
// an existing scope string — a task's current scope on a re-grant, "" on a
// fresh spawn — and ONLY its visibility half (the `vis/` rank and `+vis-ids:`)
// is taken from it.
//
// It exists so a browser never re-implements this grammar. The WebUI used to
// serialize with a JS copy that knew three of the six bases and neither half of
// the visibility pair, so opening the re-grant dialog on a `descendants` task
// and pressing apply handed self back, and a `global/subtree` task lost its
// visibility rank — silently, in both cases.
func ScopeSpec(base string, excludeSelf bool, ids []string, carry string) (string, error) {
	rank, rankExcludes, err := parseScopeBaseWord(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	// The word may already carry the flag ("descendants"); the checkbox is the
	// other input to the same field, so either source setting it is enough.
	out := protocol.TaskScope{Base: rank}
	out.SetExcludeSelf(excludeSelf || rankExcludes)

	if len(ids) > 0 {
		parsed, err := parseScopeIDs(strings.Join(ids, ","), strings.Join(ids, ","))
		if err != nil {
			return "", err
		}
		out.Ids = parsed
		out.IdsLen = uint16(len(parsed))
	}

	if strings.TrimSpace(carry) != "" {
		prev, err := ParseScope(carry)
		if err != nil {
			return "", fmt.Errorf("carrying the visibility half of %q: %w", carry, err)
		}
		out.VisBase = prev.VisBase
		out.SetVisBasePresent(prev.VisBasePresent())
		out.VisIds = prev.VisIds
		out.VisIdsLen = prev.VisIdsLen
	}
	return ScopeLabel(out), nil
}

// ScopeForFlagUsage is the --scope-for help text, shared by every verb that
// takes --scope.
const ScopeForFlagUsage = "narrow ONE capability (or a comma-separated list of them) " +
	"below the task's own scope, as CAPS=SCOPE — e.g. exec_cowrite,file_write=descendants. " +
	"Repeatable; the capability lists must not overlap. Scope syntax is the --scope one, " +
	"minus the visibility half, which belongs to the task rather than to a verb"

// ParseScopeFor parses one --scope-for value: a capability list, '=', and a
// scope. The left side takes the same comma-separated form as --caps, so a
// grouped narrowing ("every write-ish bit gets descendants") is one flag
// rather than one per bit.
//
// ':' is accepted in place of '=' because that is the separator OverridesLabel
// prints, and ls output claims to be pasteable back as flags. Cutting on '='
// first keeps the two unambiguous: a scope may itself contain ':' (ids:, but
// never '='), so the fallback only ever fires on a label-spelled value.
func ParseScopeFor(str string) (protocol.Capability, protocol.ScopeOverride, error) {
	capsText, scopeText, ok := strings.Cut(str, "=")
	if !ok {
		capsText, scopeText, ok = strings.Cut(str, ":")
	}
	if !ok {
		return 0, protocol.ScopeOverride{}, fmt.Errorf(
			"--scope-for %q: want CAPS=SCOPE (e.g. exec_cowrite,file_write=descendants)", str)
	}
	caps, err := ParseCaps(strings.TrimSpace(capsText))
	if err != nil {
		return 0, protocol.ScopeOverride{}, fmt.Errorf("--scope-for %q: %w", str, err)
	}
	if caps == protocol.Capability_None {
		return 0, protocol.ScopeOverride{}, fmt.Errorf(
			"--scope-for %q: names no capability; an override matching nothing is dead weight", str)
	}
	sc, err := ParseScope(strings.TrimSpace(scopeText))
	if err != nil {
		return 0, protocol.ScopeOverride{}, fmt.Errorf("--scope-for %q: %w", str, err)
	}
	if sc.VisBasePresent() || len(sc.VisIds) > 0 {
		return 0, protocol.ScopeOverride{}, fmt.Errorf(
			"--scope-for %q: visibility belongs to the task, not to one capability", str)
	}
	ov := protocol.ScopeOverride{Caps: caps, Base: sc.Base, Ids: sc.Ids, IdsLen: sc.IdsLen}
	ov.SetExcludeSelf(sc.ExcludeSelf())
	return caps, ov, nil
}

// MergeScopeOverride appends one override, rejecting a mask that overlaps one
// already present. The server rejects it too; catching it here means a typo
// costs a parse error rather than a round trip and a spawn failure.
func MergeScopeOverride(in []protocol.ScopeOverride, ov protocol.ScopeOverride) ([]protocol.ScopeOverride, error) {
	for _, existing := range in {
		if overlap := existing.Caps & ov.Caps; overlap != 0 {
			return nil, fmt.Errorf(
				"--scope-for: %s named twice; each capability may carry at most one override",
				CapsLabel(overlap))
		}
	}
	return append(in, ov), nil
}

// OverridesLabel renders an override list joined by spaces, so what ls prints
// can be pasted back as --scope-for flags. It uses ':' rather than '=' to match
// the '+ids:' / '+vis-ids:' separator of the --scope grammar it sits beside;
// ParseScopeFor accepts both for exactly this reason.
func OverridesLabel(in []protocol.ScopeOverride) string {
	if len(in) == 0 {
		return ""
	}
	parts := make([]string, 0, len(in))
	for _, o := range in {
		sc := protocol.TaskScope{Base: o.Base, Ids: o.Ids, IdsLen: o.IdsLen}
		sc.SetExcludeSelf(o.ExcludeSelf())
		parts = append(parts, CapsLabel(o.Caps)+":"+ScopeLabel(sc))
	}
	return strings.Join(parts, " ")
}

// ResolvedScopeByCap renders the scope that actually applies to each capability
// the task holds, override merge already done.
//
// Machine readers get this rather than the raw pair, because re-deriving
// "which override covers this bit" in every consumer is how two consumers end
// up disagreeing. Empty when the mask is empty; `none` and `all` are not
// listed, being the absence and the union rather than grantable targets.
func ResolvedScopeByCap(caps protocol.Capability, base protocol.TaskScope, overrides []protocol.ScopeOverride) map[string]string {
	out := make(map[string]string)
	for _, bit := range GrantableCaps() {
		if bit == protocol.Capability_None || bit == protocol.Capability_All {
			continue
		}
		if caps&bit != bit {
			continue
		}
		out[bit.String()] = ScopeLabel(scopeForCap(base, overrides, bit))
	}
	return out
}

// scopeForCap mirrors the server's Scope.ForCap: the override covering the bit,
// else the base scope. Masks are disjoint, so the first hit is the only hit.
func scopeForCap(base protocol.TaskScope, overrides []protocol.ScopeOverride, bit protocol.Capability) protocol.TaskScope {
	for _, o := range overrides {
		if o.Caps&bit != 0 {
			sc := protocol.TaskScope{Base: o.Base, Ids: o.Ids, IdsLen: o.IdsLen}
			sc.SetExcludeSelf(o.ExcludeSelf())
			return sc
		}
	}
	// The visibility half belongs to the task, not to a verb: strip it so the
	// per-capability entry reports a TARGET set and nothing else.
	sc := protocol.TaskScope{Base: base.Base, Ids: base.Ids, IdsLen: base.IdsLen}
	sc.SetExcludeSelf(base.ExcludeSelf())
	return sc
}
