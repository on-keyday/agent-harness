package verb

import (
	"strings"
	"testing"
)

// Every verb with a command line is a tool unless it says why it cannot be one
// (spec D10): a verb with neither answer is a decision nobody made.
func TestEveryCLIVerbHasAnMCPVerdict(t *testing.T) {
	for _, s := range Verbs {
		if !s.CmdlineSurfaces.Has(CLI) {
			if s.MCP != nil || s.MCPNotATool != "" {
				t.Errorf("%s has no command line but carries an MCP verdict", s.FlagSetName())
			}
			continue
		}
		switch {
		case s.MCP == nil && strings.TrimSpace(s.MCPNotATool) == "":
			t.Errorf("%s: neither MCP nor MCPNotATool -- declare it a tool, or say why it cannot be one", s.FlagSetName())
		case s.MCP != nil && s.MCPNotATool != "":
			t.Errorf("%s: both MCP and MCPNotATool", s.FlagSetName())
		}
	}
	if sp, ok := Lookup("mcp"); !ok || sp.MCP != nil || sp.MCPNotATool == "" {
		t.Error("mcp must be MCPNotATool: the server cannot be a tool of itself")
	}
}

// A tool's Sends feeds the capability filter and the client's guard; nil is a
// forgotten declaration, an empty non-nil slice is "sends nothing".
func TestToolsDeclareSends(t *testing.T) {
	for _, s := range Verbs {
		if s.MCP != nil && s.Sends == nil {
			t.Errorf("%s is a tool with no Sends (use []protocol.TaskControlKind{} for a verb that sends nothing)", s.FlagSetName())
		}
	}
}

func TestMCPFlagListsNameRealFlags(t *testing.T) {
	for _, s := range Verbs {
		if s.MCP == nil {
			continue
		}
		c := s.For(CLI)
		has := map[string]bool{}
		for _, f := range c.Flags {
			has[f.Name] = true
		}
		for _, list := range [][]string{s.MCP.Omit, s.MCP.WriteFlags} {
			for _, n := range list {
				if !has[n] {
					t.Errorf("%s: MCPTool names flag %q, which the verb does not have", s.FlagSetName(), n)
				}
			}
		}
		if (len(s.MCP.Omit) > 0 || len(s.MCP.Fixed) > 0) && strings.TrimSpace(s.MCP.Why) == "" {
			t.Errorf("%s: Omit/Fixed without Why", s.FlagSetName())
		}
		if len(s.MCP.WriteFlags) > 0 && s.MCP.SideEffect != MCPRead {
			t.Errorf("%s: WriteFlags on a verb that already always writes", s.FlagSetName())
		}
		if s.MCP.SideEffect != MCPRead && s.MCP.SideEffect != MCPWrite {
			t.Errorf("%s: SideEffect %q", s.FlagSetName(), s.MCP.SideEffect)
		}
	}
}

func TestToolNamesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, s := range Verbs {
		if s.MCP == nil {
			continue
		}
		name := strings.Join(s.Path, "_")
		if prev, dup := seen[name]; dup {
			t.Errorf("tool name %q is both %s and %s", name, prev, s.FlagSetName())
		}
		seen[name] = s.FlagSetName()
	}
}
