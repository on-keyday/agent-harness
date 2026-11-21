package protocol

import (
	"bytes"
	"reflect"
	"testing"
)

// roundTrip encodes v, decodes it into out and checks the whole input was
// consumed and that re-encoding reproduces the bytes.
func roundTrip(t *testing.T, enc func() []byte, dec func([]byte) error, reenc func() []byte) {
	t.Helper()
	b := enc()
	if err := dec(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(reenc(), b) {
		t.Fatal("re-encode differs")
	}
}

func TestCredentialAuthRoundTrip(t *testing.T) {
	var ca CredentialAuth
	ca.TaskId.Id[0] = 0xc1
	for i := range ca.Sig {
		ca.Sig[i] = byte(i)
	}
	var orig PskAuthRequest
	orig.Role = AuthRole_Credential
	if !orig.SetCredentialAuth(ca) {
		t.Fatal("SetCredentialAuth returned false")
	}
	var got PskAuthRequest
	roundTrip(t, func() []byte { return orig.MustAppend(nil) },
		func(b []byte) error { return got.DecodeExact(b) },
		func() []byte { return got.MustAppend(nil) })
	if got.Role != AuthRole_Credential || got.BinderLen != 0 {
		t.Fatalf("role/binder = %v/%d", got.Role, got.BinderLen)
	}
	c := got.CredentialAuth()
	if c == nil || !reflect.DeepEqual(*c, ca) {
		t.Fatalf("credential_auth = %+v", c)
	}
	if AuthRole_Credential != 4 || PskAuthStatus_Revoked != 7 {
		t.Fatal("credential auth numbers moved")
	}
}

func TestCredentialIssueAndRekeyRoundTrip(t *testing.T) {
	var key [32]byte
	for i := range key {
		key[i] = byte(0x40 + i)
	}

	var issue CredentialIssueRequest
	issue.Pubkey = key
	issue.TtlMs = 3600_000
	issue.SetName([]byte("nightly-ci"))
	issue.RequestedCaps = Capability_Spawn | Capability_ScheduleRun
	issue.DelegateCaps = Capability_Spawn
	var req TaskControlRequest
	req.Header.Kind = TaskControlKind_CredentialIssue
	req.Header.RequestId = 7
	if !req.SetCredentialIssue(issue) {
		t.Fatal("SetCredentialIssue returned false")
	}
	var gotReq TaskControlRequest
	roundTrip(t, func() []byte { return req.MustAppend(nil) },
		func(b []byte) error { return gotReq.DecodeExact(b) },
		func() []byte { return gotReq.MustAppend(nil) })
	gi := gotReq.CredentialIssue()
	if gi == nil || gi.Pubkey != key || gi.TtlMs != 3600_000 || string(gi.Name) != "nightly-ci" ||
		gi.RequestedCaps != issue.RequestedCaps || gi.DelegateCaps != Capability_Spawn {
		t.Fatalf("issue = %+v", gi)
	}

	var rekey CredentialRekeyRequest
	rekey.TaskId.Id[3] = 0x33
	rekey.Pubkey = key
	rekey.TtlMs = 1000
	rekey.SetCapsPresent(true)
	rekey.RequestedCaps = Capability_ScheduleRun
	var req2 TaskControlRequest
	req2.Header.Kind = TaskControlKind_CredentialRekey
	if !req2.SetCredentialRekey(rekey) {
		t.Fatal("SetCredentialRekey returned false")
	}
	var gotReq2 TaskControlRequest
	roundTrip(t, func() []byte { return req2.MustAppend(nil) },
		func(b []byte) error { return gotReq2.DecodeExact(b) },
		func() []byte { return gotReq2.MustAppend(nil) })
	gr := gotReq2.CredentialRekey()
	if gr == nil || gr.TaskId != rekey.TaskId || gr.TtlMs != 1000 || !gr.CapsPresent() ||
		gr.ScopePresent() || gr.RequestedCaps != Capability_ScheduleRun {
		t.Fatalf("rekey = %+v", gr)
	}
}

func TestCredentialResponseRoundTrip(t *testing.T) {
	var r CredentialResponse
	r.Status = CredentialStatus_MaskedCap
	r.TaskId.Id[15] = 9
	r.ExpiresAt = 1700000000000000000
	r.SetMessage([]byte("schedule_edit cannot be held by a credential"))
	for _, k := range []TaskControlKind{TaskControlKind_CredentialIssue, TaskControlKind_CredentialRekey} {
		var resp TaskControlResponse
		resp.Kind = k
		resp.RequestId = 3
		var ok bool
		if k == TaskControlKind_CredentialIssue {
			ok = resp.SetCredentialIssue(r)
		} else {
			ok = resp.SetCredentialRekey(r)
		}
		if !ok {
			t.Fatalf("set arm for %v returned false", k)
		}
		var got TaskControlResponse
		roundTrip(t, func() []byte { return resp.MustAppend(nil) },
			func(b []byte) error { return got.DecodeExact(b) },
			func() []byte { return got.MustAppend(nil) })
		var gr *CredentialResponse
		if k == TaskControlKind_CredentialIssue {
			gr = got.CredentialIssue()
		} else {
			gr = got.CredentialRekey()
		}
		if gr == nil || gr.Status != CredentialStatus_MaskedCap || gr.TaskId != r.TaskId ||
			gr.ExpiresAt != r.ExpiresAt || string(gr.Message) != string(r.Message) {
			t.Fatalf("%v response = %+v", k, gr)
		}
	}
	if SetCapsStatus_MaskedCap == SetCapsStatus_InvalidScope {
		t.Fatal("masked_cap must be its own set_caps status")
	}
}

func TestTaskInfoCredentialTailRoundTrip(t *testing.T) {
	var ti TaskInfo
	ti.Kind = TaskKind_Credential
	ti.Status = TaskStatus_Active
	ti.EndReason = EndReason_Expired
	ti.OriginKind = ClientKind_Credential
	ti.CredentialExpiresAt = 1700000000000000001
	ti.CredentialLastUsedAt = 1700000000000000002
	ti.SetCredentialName([]byte("ci"))
	ti.SetSchedulePresent(true)
	ti.SetScheduleId(schedID(0x22))
	var got TaskInfo
	roundTrip(t, func() []byte { return ti.MustAppend(nil) },
		func(b []byte) error { return got.DecodeExact(b) },
		func() []byte { return got.MustAppend(nil) })
	if got.CredentialExpiresAt != ti.CredentialExpiresAt || got.CredentialLastUsedAt != ti.CredentialLastUsedAt ||
		string(got.CredentialName) != "ci" || got.Kind != TaskKind_Credential || got.Status != TaskStatus_Active ||
		got.EndReason != EndReason_Expired || got.ScheduleId() == nil {
		t.Fatalf("task info tail = %+v", got)
	}
	if TaskStatus_Active != 7 || TaskKind_Credential != 3 || EndReason_Expired != 11 {
		t.Fatal("credential enum values moved")
	}
}

func TestWhoAmICredentialTailRoundTrip(t *testing.T) {
	var w WhoAmIResponse
	w.PrincipalKind = ClientKind_Credential
	w.CredentialExpiresAt = 1700000000000000003
	w.SetServerRevision([]byte("abc"))
	var got WhoAmIResponse
	roundTrip(t, func() []byte { return w.MustAppend(nil) },
		func(b []byte) error { return got.DecodeExact(b) },
		func() []byte { return got.MustAppend(nil) })
	if got.PrincipalKind != ClientKind_Credential || got.CredentialExpiresAt != w.CredentialExpiresAt ||
		string(got.ServerRevision) != "abc" {
		t.Fatalf("whoami = %+v", got)
	}
}
