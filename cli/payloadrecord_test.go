package cli

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// TestPutPayloadFields pins the contract of the one function that shapes a
// message body into a JSON-Lines record. The agent record (inbox / read / wait
// / dispatch) and both board faces call it, so a change here reaches every
// surface at once. Before it existed, `payload_text` reached the agent inbox
// record and neither board record, and the agent-facing `agent thread --json`
// handed a prose body over as base64 alone.
func TestPutPayloadFields(t *testing.T) {
	const (
		onlyB64 = "b64"  // no readable form; payload_b64 is the only body
		asJSON  = "json" // payload embedded raw
		asProse = "text" // payload_text
	)

	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"empty body", nil, onlyB64},
		{"empty non-nil body", []byte{}, onlyB64},
		{"JSON object", []byte(`{"kind":"hello"}`), asJSON},
		{"JSON scalar", []byte(`42`), asJSON},
		{"JSON null", []byte(`null`), asJSON},
		{"ASCII prose", []byte("build is green"), asProse},
		{"prose with newline and ESC", []byte("line one\n\x1b[31mred"), asProse},
		{"NUL is still valid UTF-8", []byte("a\x00b"), asProse},
		{"invalid UTF-8", []byte{0xff, 0xfe, 'a'}, onlyB64},
	}

	for _, tc := range cases {
		for _, drop := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dropB64=%v", tc.name, drop), func(t *testing.T) {
				rec := map[string]any{}
				PutPayloadFields(rec, tc.body, drop)

				_, hasRaw := rec["payload"]
				_, hasText := rec["payload_text"]
				b64, hasB64 := rec["payload_b64"]

				switch tc.want {
				case asJSON:
					if !hasRaw || hasText {
						t.Fatalf("JSON body: payload=%v payload_text=%v, want raw only", hasRaw, hasText)
					}
				case asProse:
					if hasRaw || !hasText {
						t.Fatalf("prose body: payload=%v payload_text=%v, want text only", hasRaw, hasText)
					}
					if rec["payload_text"] != string(tc.body) {
						t.Fatalf("payload_text = %q, want %q", rec["payload_text"], tc.body)
					}
				case onlyB64:
					if hasRaw || hasText {
						t.Fatalf("unreadable body: payload=%v payload_text=%v, want neither", hasRaw, hasText)
					}
				}

				// payload_b64 is the exact bytes. It is dropped only when the
				// caller asked for the drop AND a readable form was written;
				// an unreadable body always keeps it, since it is then the
				// only body there is.
				wantB64 := !(drop && tc.want != onlyB64)
				if hasB64 != wantB64 {
					t.Fatalf("payload_b64 present=%v, want %v", hasB64, wantB64)
				}
				if hasB64 && b64 != base64.StdEncoding.EncodeToString(tc.body) {
					t.Fatalf("payload_b64 = %v, want the exact bytes", b64)
				}
			})
		}
	}
}
