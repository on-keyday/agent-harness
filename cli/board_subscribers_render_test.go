package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Contacts are what an operator reads before granting board_peer, so every
// row carries them — an empty list as "-" / [], never an absent field, because
// "no contacts" is a measurement.
func TestRenderSubscriberRowsCarriesContacts(t *testing.T) {
	rows := []BoardSubscriberRow{
		{TaskHex: "aa", Hostname: "h", AgentProfile: "claude", Contacts: []string{"rr.x", "chat.bbbbbbbb"}},
		{TaskHex: "bb"},
	}
	var text bytes.Buffer
	if err := renderSubscriberRows(&text, rows, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(text.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " contacts=rr.x,chat.bbbbbbbb") || !strings.HasSuffix(lines[1], " contacts=-") {
		t.Errorf("text rows = %q", lines)
	}

	var js bytes.Buffer
	if err := renderSubscriberRows(&js, rows, true); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&js)
	for i, want := range [][]string{{"rr.x", "chat.bbbbbbbb"}, {}} {
		var rec map[string]json.RawMessage
		if err := dec.Decode(&rec); err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := json.Unmarshal(rec["contacts"], &got); err != nil || got == nil || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("row %d contacts = %s, want %q as a list", i, rec["contacts"], want)
		}
	}
}
