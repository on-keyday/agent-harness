//go:build !js

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func credentialTaskInfo(name string, lastUsed uint64) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = 0xc7
	t.Kind = protocol.TaskKind_Credential
	t.Status = protocol.TaskStatus_Active
	t.CredentialExpiresAt = uint64(time.Date(2026, 12, 1, 3, 4, 5, 0, time.UTC).UnixNano())
	t.CredentialLastUsedAt = lastUsed
	t.Capabilities = protocol.Capability_ScheduleRun
	if name != "" {
		t.SetName([]byte(name))
	}
	return t
}

// A credential's row in `ls`: kind, status, expiry, name, last use. A zero
// last use is `unknown`, never elided (it is a reading: the server restarted).
func TestLsRowRendersACredential(t *testing.T) {
	line := taskLine(credentialTaskInfo("nightly", 0), nil)
	for _, want := range []string{"credential ", "Active", "expires=", "name=nightly", "last_used=unknown"} {
		if !strings.Contains(line, want) {
			t.Errorf("row lacks %q:\n%s", want, line)
		}
	}
	if !(strings.Index(line, "expires=") < strings.Index(line, "name=") && strings.Index(line, "name=") < strings.Index(line, "last_used=")) {
		t.Errorf("columns out of order (expires, name, last_used):\n%s", line)
	}
	used := uint64(time.Date(2026, 11, 30, 1, 2, 3, 0, time.UTC).UnixNano())
	line = taskLine(credentialTaskInfo("", used), nil)
	if strings.Contains(line, "name=") {
		t.Errorf("an unnamed credential prints name=:\n%s", line)
	}
	if !strings.Contains(line, "last_used="+time.Unix(0, int64(used)).Format(time.RFC3339)) {
		t.Errorf("last use not printed as RFC3339:\n%s", line)
	}
	// An ordinary task is untouched; a name on one (D21) shows without the
	// credential columns.
	var o protocol.TaskInfo
	o.Kind = protocol.TaskKind_Interactive
	if l := taskLine(o, nil); strings.Contains(l, "expires=") || strings.Contains(l, "last_used=") || strings.Contains(l, "name=") {
		t.Errorf("an ordinary row carries credential columns:\n%s", l)
	}
	o.SetName([]byte("labelled"))
	if l := taskLine(o, nil); !strings.Contains(l, "name=labelled") || strings.Contains(l, "expires=") {
		t.Errorf("a named ordinary task:\n%s", l)
	}
}

func TestLsJSONRendersACredential(t *testing.T) {
	cred := credentialTaskInfo("nightly", 0)
	ord := protocol.TaskInfo{Kind: protocol.TaskKind_Interactive}
	lr := &protocol.ListResultBody{Tasks: []protocol.TaskInfo{cred, ord}}
	var out bytes.Buffer
	renderListJSON(lr, &out)
	var doc struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(doc.Tasks) != 2 {
		t.Fatalf("%d tasks", len(doc.Tasks))
	}
	c := doc.Tasks[0]
	if c["kind"] != "credential" || c["status"] != "active" || c["name"] != "nightly" ||
		c["credential_last_used_at"] != "unknown" {
		t.Errorf("credential row = %v", c)
	}
	if _, ok := c["credential_expires_at"].(string); !ok {
		t.Errorf("credential_expires_at missing: %v", c)
	}
	for _, k := range []string{"name", "credential_expires_at", "credential_last_used_at"} {
		if _, ok := doc.Tasks[1][k]; ok {
			t.Errorf("an ordinary task row carries %q", k)
		}
	}
}

func TestWhoAmIRendersACredential(t *testing.T) {
	var r protocol.WhoAmIResponse
	r.PrincipalTaskId.Id[0] = 0xc7
	r.PrincipalKind = protocol.ClientKind_Credential
	r.CredentialExpiresAt = uint64(time.Date(2026, 12, 1, 3, 4, 5, 0, time.UTC).UnixNano())
	var out bytes.Buffer
	if err := WriteWhoAmI(&out, r, false); err != nil {
		t.Fatal(err)
	}
	want := "kind=credential expires=" + time.Unix(0, int64(r.CredentialExpiresAt)).Format(time.RFC3339)
	if !strings.Contains(out.String(), want) {
		t.Errorf("text lacks %q:\n%s", want, out.String())
	}
	out.Reset()
	if err := WriteWhoAmI(&out, r, true); err != nil {
		t.Fatal(err)
	}
	var j map[string]any
	if err := json.Unmarshal(out.Bytes(), &j); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if j["principal_kind"] != "credential" || j["credential_expires_at"] == "" {
		t.Errorf("json = %v", j)
	}
	// An agent's whoami is unchanged: no credential words.
	r.PrincipalKind = protocol.ClientKind_Agent
	out.Reset()
	WriteWhoAmI(&out, r, false)
	if strings.Contains(out.String(), "kind=credential") {
		t.Errorf("an agent whoami says credential:\n%s", out.String())
	}
}

// Each CredentialStatus has a cause the person can act on.
func TestEveryCredentialStatusHasAMessage(t *testing.T) {
	for st := protocol.CredentialStatus_TtlInvalid; st <= protocol.CredentialStatus_BadName; st++ {
		err := credentialStatusError("new", &protocol.CredentialResponse{Status: st})
		if err == nil || len(err.Error()) < len("credential new: ")+10 {
			t.Errorf("%v: %v", st, err)
		}
	}
	if err := credentialStatusError("rekey", &protocol.CredentialResponse{Status: protocol.CredentialStatus_NoSuchTask}); !strings.Contains(err.Error(), "no such credential within your reach") {
		t.Errorf("no_such_task: %v", err)
	}
}

// A name written before the server refused control characters still reaches
// a terminal through these three sites; each prints it sanitized.
func TestTaskNameIsSanitizedAtEveryCLIRenderSite(t *testing.T) {
	const hostile = "ci\x1b]0;pwned\x07\u009b2J"
	ti := credentialTaskInfo(hostile, 0)
	for site, out := range map[string]string{
		"ls row":            taskLine(ti, nil),
		"credential ls row": CredentialRowLine(ti),
		"board sender":      SenderLabel("credential", "c7000000000000000000000000000000", hostile),
	} {
		if strings.ContainsAny(out, "\x1b\x07\u009b") {
			t.Errorf("%s prints a control character: %q", site, out)
		}
		if !strings.Contains(out, "ci]0;pwned2J") {
			t.Errorf("%s lost the name's text: %q", site, out)
		}
	}
}
