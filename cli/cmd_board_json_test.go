package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestEmitBoardMessageJSON checks the `board read --json` record shape: the
// fields that overlap with `agent inbox --json` (seq, in_reply_to, topic, from,
// payload_b64, payload) plus the operator-only columns the text row carries
// (reply_to_topic, received_at, shown_to, and the retracted trio).
func TestEmitBoardMessageJSON(t *testing.T) {
	// Two subscribers to chat.abc: one shown up to seq 10, the other only to
	// seq 3. For a message at seq 7 that is shown_to = 1/2 (ShownTo counts a
	// subscriber as reached when its watermark >= the message seq).
	subs := []BoardSubscriberRow{
		{TaskHex: "aa", Patterns: []BoardSubscriberPattern{{Name: "chat.abc", Shown: 10}}},
		{TaskHex: "bb", Patterns: []BoardSubscriberPattern{{Name: "chat.abc", Shown: 3}}},
	}

	t.Run("json payload embeds raw + base64, plus operator columns", func(t *testing.T) {
		payload := []byte(`{"kind":"hello","n":1}`)
		var buf bytes.Buffer
		emitBoardMessageJSON(&buf, "chat.abc", BoardMessage{
			Seq:              7,
			InReplyTo:        3,
			FromTaskHex:      "deadbeef",
			FromHostname:     "gmkhost",
			FromAgentProfile: "claude",
			ReplyToTopic:     "chat.xyz",
			ReceivedAtMs:     1_700_000_000_000,
			Payload:          payload,
		}, subs)

		var rec struct {
			Seq          uint64 `json:"seq"`
			InReplyTo    uint64 `json:"in_reply_to"`
			Topic        string `json:"topic"`
			ReplyToTopic string `json:"reply_to_topic"`
			ReceivedAtMs uint64 `json:"received_at_ms"`
			ReceivedAt   string `json:"received_at"`
			Retracted    bool   `json:"retracted"`
			ShownTo      struct {
				Shown int `json:"shown"`
				Total int `json:"total"`
			} `json:"shown_to"`
			From struct {
				TaskID   string `json:"task_id"`
				Hostname string `json:"hostname"`
				Agent    string `json:"agent"`
			} `json:"from"`
			PayloadB64    string          `json:"payload_b64"`
			Payload       json.RawMessage `json:"payload"`
			RetractedAtMs *uint64         `json:"retracted_at_ms"`
			RetractedBy   *string         `json:"retracted_by"`
		}
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("emitted line is not valid JSON: %v\nline: %s", err, buf.String())
		}
		if rec.Seq != 7 || rec.InReplyTo != 3 || rec.Topic != "chat.abc" {
			t.Fatalf("scalar fields wrong: %+v", rec)
		}
		if rec.ReplyToTopic != "chat.xyz" {
			t.Fatalf("reply_to_topic wrong: %q", rec.ReplyToTopic)
		}
		if rec.ShownTo.Shown != 1 || rec.ShownTo.Total != 2 {
			t.Fatalf("shown_to wrong: %+v (want shown=1 total=2)", rec.ShownTo)
		}
		if rec.From.TaskID != "deadbeef" || rec.From.Hostname != "gmkhost" || rec.From.Agent != "claude" {
			t.Fatalf("from block wrong: %+v", rec.From)
		}
		if rec.Retracted {
			t.Fatalf("retracted should be false")
		}
		if rec.RetractedAtMs != nil || rec.RetractedBy != nil {
			t.Fatalf("retracted extras must be omitted when not retracted")
		}
		if got := base64.StdEncoding.EncodeToString(payload); rec.PayloadB64 != got {
			t.Fatalf("payload_b64 mismatch: got %q want %q", rec.PayloadB64, got)
		}
		if !bytes.Equal(rec.Payload, payload) {
			t.Fatalf("payload should be embedded raw: got %s want %s", rec.Payload, payload)
		}
		if rec.ReceivedAt == "" {
			t.Fatalf("received_at (RFC3339) should be present")
		}
	})

	t.Run("non-JSON payload omits the raw payload field", func(t *testing.T) {
		payload := []byte("not json \x00 bytes")
		var buf bytes.Buffer
		emitBoardMessageJSON(&buf, "t", BoardMessage{Seq: 1, Payload: payload}, nil)

		var rec map[string]json.RawMessage
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		if _, ok := rec["payload"]; ok {
			t.Fatalf("payload (raw) must be omitted for a non-JSON body")
		}
		b64, ok := rec["payload_b64"]
		if !ok {
			t.Fatalf("payload_b64 must always be present")
		}
		var s string
		_ = json.Unmarshal(b64, &s)
		if got := base64.StdEncoding.EncodeToString(payload); s != got {
			t.Fatalf("payload_b64 mismatch: got %q want %q", s, got)
		}
		// shown_to is present even with nil subs (0/0), for addressability.
		if _, ok := rec["shown_to"]; !ok {
			t.Fatalf("shown_to must be present (0/0) even without subscriber data")
		}
	})

	t.Run("retracted message carries retracted_at_ms and retracted_by", func(t *testing.T) {
		var buf bytes.Buffer
		emitBoardMessageJSON(&buf, "t", BoardMessage{
			Seq: 2, Retracted: true, RetractedAtMs: 1_700_000_000_500,
			RetractedBy: protocol.RetractedBy_Author,
		}, nil)
		var rec struct {
			Retracted     bool    `json:"retracted"`
			RetractedAtMs *uint64 `json:"retracted_at_ms"`
			RetractedBy   string  `json:"retracted_by"`
		}
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		if !rec.Retracted {
			t.Fatalf("retracted should be true")
		}
		if rec.RetractedAtMs == nil || *rec.RetractedAtMs != 1_700_000_000_500 {
			t.Fatalf("retracted_at_ms wrong: %v", rec.RetractedAtMs)
		}
		// retracted_by reuses RetractedByLabel; the author path renders "author".
		if rec.RetractedBy != RetractedByLabel(BoardMessage{RetractedBy: protocol.RetractedBy_Author}) {
			t.Fatalf("retracted_by should match RetractedByLabel, got %q", rec.RetractedBy)
		}
	})

	t.Run("empty payload stays valid and JSON-Lines terminated", func(t *testing.T) {
		var buf bytes.Buffer
		emitBoardMessageJSON(&buf, "t", BoardMessage{Seq: 9}, nil)
		if n := buf.Len(); n == 0 || buf.Bytes()[n-1] != '\n' {
			t.Fatalf("record must end in a newline (JSON Lines)")
		}
		var rec map[string]any
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		if _, ok := rec["payload"]; ok {
			t.Fatalf("payload (raw) must be omitted for an empty body")
		}
	})
}
