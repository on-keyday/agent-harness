package verb

import (
	"flag"
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Server-defined actions on the command line
// (2026-11-26-grant-conditions-and-bound-enrollment-design.md, C12, C17).

// repeatedValue backs a flag that may be given more than once (--param,
// --value): the raw spellings in order, parsed by the verb's Convert.
var repeatedValue = &CustomValue{
	New: func() flag.Value { return &stringList{} },
	Get: func(v flag.Value) any {
		if l, ok := v.(*stringList); ok {
			return []string(*l)
		}
		return []string(nil)
	},
}

// parseParamList parses every --param with the server's rule.
func parseParamList(specs []string) ([]protocol.ActionParam, error) {
	out := make([]protocol.ActionParam, 0, len(specs))
	for _, s := range specs {
		p, err := actiondef.ParseParam(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ParseActionValues parses every --value NAME=VALUE. A value is taken
// verbatim after the first '=' (it may itself contain '='); a name given twice
// is refused rather than silently keeping one.
func ParseActionValues(specs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range specs {
		name, v, ok := strings.Cut(s, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--value %q: want NAME=VALUE", s)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--value %s given twice", name)
		}
		out[name] = v
	}
	return out, nil
}

// actionTemplateHelp is the template rules, said once for new and edit.
const actionTemplateHelp = "the command after `--`: each word is one argv element; a word that is exactly {NAME} " +
	"is replaced by that param's value as ONE element; \\{ at the start of a word keeps it literal; " +
	"{NAME} inside other text (--env={env}) is refused — there is no partial substitution"

// paramHelp is --param's help, for new and edit.
const paramHelp = "a parameter, repeatable: NAME=token (1-128 bytes of [0-9A-Za-z._-], not starting with -) or " +
	"NAME=choice:A,B,... (one of those literals)"

// actionPutFlags are the flags action new and action edit share.
func actionPutFlags(nameRequired bool, nameDefault string) []Flag {
	return []Flag{
		{Name: "name", Type: FlagString, Default: "", Field: "Name", Required: nameRequired,
			Help: "the action's name, [a-z0-9][a-z0-9._-]{0,63}, unique" + nameDefault},
		{Name: "description", Type: FlagString, Default: "", Field: "Description", PresenceField: "DescriptionSet",
			Help: "free text shown by `action ls` (at most 1024 bytes; on edit, omitted keeps the current one)"},
		{Name: "param", Type: FlagString, Custom: repeatedValue, Field: "Params",
			FieldType: "[]protocol.ActionParam", Convert: "parseParamList", Help: paramHelp},
	}
}
