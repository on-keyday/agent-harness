//go:build !js

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// makeConnInfo builds a ConnInfo for tests, setting CidLen/RemoteAddrLen via
// the protocol SetX helpers so the length-prefix fields are consistent.
func makeConnInfo(cid, remoteAddr string, role protocol.ConnRole, principalID [16]byte, connectedAt time.Time, identified bool) protocol.ConnInfo {
	ci := protocol.ConnInfo{
		Role:        role,
		ConnectedAt: uint64(connectedAt.UnixNano()),
	}
	ci.SetCid([]byte(cid))
	ci.SetRemoteAddr([]byte(remoteAddr))
	ci.PrincipalTask = protocol.TaskID{Id: principalID}
	ci.SetIdentified(identified)
	return ci
}

// TestConnInfoTextLine_Identified checks the text formatter for a fully
// identified agent connection: addr, role, short principal id, age, no unident.
func TestConnInfoTextLine_Identified(t *testing.T) {
	var principal [16]byte
	principal[0] = 0xab
	principal[1] = 0xcd
	now := time.Now()
	// 90 seconds ago so age shows as non-zero
	connectedAt := now.Add(-90 * time.Second)

	ci := makeConnInfo("cid1", "203.0.113.5:5000", protocol.ConnRole_Agent, principal, connectedAt, true)

	line := connInfoTextLine(&ci)

	if !strings.Contains(line, "203.0.113.5:5000") {
		t.Errorf("expected remote addr in line: %q", line)
	}
	if !strings.Contains(line, "agent") {
		t.Errorf("expected role 'agent' in line: %q", line)
	}
	// Short principal id — first 8 hex chars of abcd...
	if !strings.Contains(line, "abcd") {
		t.Errorf("expected short principal id starting with 'abcd' in line: %q", line)
	}
	// Age present and correct: the conn was ~90s ago, so connAge emits the
	// minutes token "1m" (e.g. "1m30s"). Assert the actual token so this
	// fails if the age string is missing — a bare "s"/"m" check would pass
	// vacuously since "s" already appears in the addr (...:5000) and "agent".
	if !strings.Contains(line, "1m") {
		t.Errorf("expected age token '1m' (90s ago) in line: %q", line)
	}
	// No unident marker
	if strings.Contains(line, "unident") {
		t.Errorf("unexpected 'unident' marker for identified conn: %q", line)
	}
}

// TestConnInfoTextLine_Unidentified checks that an unidentified connection
// shows the unident marker and role "unspecified".
func TestConnInfoTextLine_Unidentified(t *testing.T) {
	var zeroPrincipal [16]byte
	ci := makeConnInfo("cid2", "198.51.100.7:22222", protocol.ConnRole_Unspecified, zeroPrincipal,
		time.Now().Add(-5*time.Second), false)

	line := connInfoTextLine(&ci)

	if !strings.Contains(line, "198.51.100.7:22222") {
		t.Errorf("expected remote addr in line: %q", line)
	}
	if !strings.Contains(line, "unident") {
		t.Errorf("expected 'unident' marker for unidentified conn: %q", line)
	}
}

// TestConnInfoJSONLine_ValidJSON verifies the JSON line is valid JSON.
func TestConnInfoJSONLine_ValidJSON(t *testing.T) {
	var principal [16]byte
	principal[0] = 0xde
	principal[1] = 0xad

	ci := makeConnInfo("cid3", "10.0.0.1:9000", protocol.ConnRole_Cli, principal,
		time.Now().Add(-10*time.Second), true)

	line := connInfoJSONLine(&ci)

	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("connInfoJSONLine produced invalid JSON: %v\noutput: %q", err, line)
	}
}

// TestConnInfoJSONLine_Fields checks that the JSON line contains the
// expected fields: remote_addr, role, age_sec, identified, principal_task.
func TestConnInfoJSONLine_Fields(t *testing.T) {
	var principal [16]byte
	principal[0] = 0xbe
	principal[1] = 0xef

	ci := makeConnInfo("cid4", "172.16.0.2:8080", protocol.ConnRole_Webui, principal,
		time.Now().Add(-30*time.Second), true)

	line := connInfoJSONLine(&ci)

	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	for _, key := range []string{"remote_addr", "role", "age_sec", "identified", "principal_task"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q in JSON: %s", key, line)
		}
	}

	if m["role"] != "webui" {
		t.Errorf("expected role=webui in JSON, got %q: %s", m["role"], line)
	}
	if m["remote_addr"] != "172.16.0.2:8080" {
		t.Errorf("expected remote_addr in JSON: %s", line)
	}
	if m["identified"] != true {
		t.Errorf("expected identified=true in JSON: %s", line)
	}
}

// TestConnInfoJSONLine_Unidentified checks that unidentified=false is
// represented in JSON and the unident marker exists in the text form.
func TestConnInfoJSONLine_Unidentified(t *testing.T) {
	var zeroPrincipal [16]byte
	ci := makeConnInfo("cid5", "192.168.1.1:443", protocol.ConnRole_Unspecified, zeroPrincipal,
		time.Now().Add(-2*time.Second), false)

	line := connInfoJSONLine(&ci)
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["identified"] != false {
		t.Errorf("expected identified=false in JSON for unidentified conn: %s", line)
	}
}
