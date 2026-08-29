//go:build !js

package sshgw

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

const validID = "0123456789abcdef0123456789abcdef"

func TestParseUserName_Accepted(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want UserOpts
	}{
		{"bare is cowrite", validID, UserOpts{Mode: protocol.AttachMode_Cowrite}},
		{"control suffix", validID + ".control", UserOpts{Mode: protocol.AttachMode_Control}},
		{"view suffix", validID + ".view", UserOpts{Mode: protocol.AttachMode_View}},
		// An exec option alone leaves the attach mode at its default, because
		// it says nothing about attaching.
		{"detach alone", validID + ".detach",
			UserOpts{Mode: protocol.AttachMode_Cowrite, Detach: true}},
		{"sshd-parent alone", validID + ".sshd-parent",
			UserOpts{Mode: protocol.AttachMode_Cowrite, SshdParent: true}},
		// The form a remote editor's ~/.ssh/config User line carries.
		{"both exec options", validID + ".detach,sshd-parent",
			UserOpts{Mode: protocol.AttachMode_Cowrite, Detach: true, SshdParent: true}},
		// Order is not significant: this is a set, not a sequence.
		{"reversed", validID + ".sshd-parent,detach",
			UserOpts{Mode: protocol.AttachMode_Cowrite, Detach: true, SshdParent: true}},
		// Accepted rather than refused: one connection may open a shell
		// channel AND exec channels, so the combination is meaningful.
		{"attach mode with an exec option", validID + ".control,detach",
			UserOpts{Mode: protocol.AttachMode_Control, Detach: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, got, err := ParseUserName(tc.in)
			if err != nil {
				t.Fatalf("ParseUserName(%q) error: %v", tc.in, err)
			}
			if id != validID {
				t.Errorf("task id = %q, want %q", id, validID)
			}
			if got != tc.want {
				t.Errorf("opts = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseUserName_Rejected(t *testing.T) {
	// Uppercase is rejected rather than lowered: task ids are printed lowercase
	// everywhere in this system, so an uppercase name is more likely a typo
	// than a request, and silently accepting it would make two spellings of the
	// same session look like two different ones in any log of this gateway.
	for _, in := range []string{
		"",
		"root",
		validID[:31],
		validID + "0",
		"0123456789ABCDEF0123456789ABCDEF",
		validID + ".cowrite", // the bare form IS cowrite; a suffix for it would be a second spelling
		validID + ".Control",
		validID + ".control.view",
		validID + ".control,view", // two attach modes: the loser would be silent
		validID + ".view,control",
		validID + ".detach,", // an empty token is a typo, not an empty set
		validID + ".Detach",
		validID + ".sshd_parent", // the spelling is sshd-parent
		validID + ".detach,bogus",
		"prefix-" + validID,
		"01234567-89ab-cdef-0123-456789abcdef",
	} {
		if _, _, err := ParseUserName(in); err == nil {
			t.Errorf("ParseUserName(%q) = nil error, want a rejection", in)
		}
	}
}

// The rejection text is what the ssh client prints, and it is the only place an
// operator who typed the wrong thing finds out what the right thing is.
func TestParseUserName_ErrorNamesTheForms(t *testing.T) {
	_, _, err := ParseUserName("root")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"control", "view", "detach", "sshd-parent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
