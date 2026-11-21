package server

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The credential verbs (docs/superpowers/specs/2026-11-21-credential-tasks-design.md):
// issue creates a credential task from a public key (D13), re-key registers a
// new key and expiry on one (D18). Both need credential_issue, checked
// centrally (requiredCap); the attenuation and the target gates are here.

func (h *TaskHandler) handleCredentialIssue(conn ConnHandle, requestID uint32, cid string, req *protocol.CredentialIssueRequest) {
	h.sendCredential(conn, protocol.TaskControlKind_CredentialIssue, requestID, h.credentialIssue(cid, req, time.Now()))
}

func (h *TaskHandler) handleCredentialRekey(conn ConnHandle, requestID uint32, cid string, req *protocol.CredentialRekeyRequest) {
	h.sendCredential(conn, protocol.TaskControlKind_CredentialRekey, requestID, h.credentialRekey(cid, req, time.Now()))
}

func (h *TaskHandler) sendCredential(conn ConnHandle, kind protocol.TaskControlKind, requestID uint32, r protocol.CredentialResponse) {
	resp := protocol.TaskControlResponse{Kind: kind, RequestId: requestID}
	if kind == protocol.TaskControlKind_CredentialIssue {
		resp.SetCredentialIssue(r)
	} else {
		resp.SetCredentialRekey(r)
	}
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

func credentialRefusal(st protocol.CredentialStatus, format string, args ...any) protocol.CredentialResponse {
	r := protocol.CredentialResponse{Status: st}
	if format != "" {
		r.SetMessage([]byte(fmt.Sprintf(format, args...)))
	}
	return r
}

// credentialTTL is ttl_ms as a duration: required, and at most
// CredentialMaxTTL (D7). Compared in milliseconds, so a huge ttl_ms cannot
// overflow into a short Duration.
func credentialTTL(ms uint64) (time.Duration, *protocol.CredentialResponse) {
	if ms == 0 {
		r := credentialRefusal(protocol.CredentialStatus_TtlInvalid, "an expiry is required: pass a ttl")
		return 0, &r
	}
	if ms > uint64(CredentialMaxTTL/time.Millisecond) {
		r := credentialRefusal(protocol.CredentialStatus_TtlTooLong, "a ttl of %d ms exceeds the server's bound of %d days",
			ms, CredentialMaxTTL/(24*time.Hour))
		return 0, &r
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// credentialKeyRefusal is bad_pubkey for a key no credential may hold
// (credentialKeyUsable).
func credentialKeyRefusal(pub [32]byte) *protocol.CredentialResponse {
	if credentialKeyUsable(pub) {
		return nil
	}
	r := credentialRefusal(protocol.CredentialStatus_BadPubkey,
		"not a usable Ed25519 public key: off the curve, non-canonical, or of small order")
	return &r
}

// maskedRefusal is masked_cap when a grant half the request writes names a
// bit no credential may hold (D14). Refused rather than masked: a typed grant
// either takes effect or errors. The store masks too, as the last line.
func maskedRefusal(caps protocol.Capability) *protocol.CredentialResponse {
	if m := caps & CredentialMaskedCaps; m != 0 {
		r := credentialRefusal(protocol.CredentialStatus_MaskedCap, "a credential can never hold %s", authority.CapsLabel(m))
		return &r
	}
	return nil
}

func issueGrantRequest(req *protocol.CredentialIssueRequest) grantRequest {
	return grantRequest{
		Caps: req.RequestedCaps, Scope: authority.ScopeFromWire(req.Scope, req.Overrides),
		DelegateCaps: req.DelegateCaps, DelegateScope: authority.ScopeFromWire(req.DelegateScope, nil),
		SpawnDepth: req.SpawnDepth, MaxDescendants: req.MaxDescendants,
	}
}

func rekeyGrantRequest(req *protocol.CredentialRekeyRequest) grantRequest {
	return grantRequest{
		Caps: req.RequestedCaps, Scope: authority.ScopeFromWire(req.Scope, req.Overrides),
		DelegateCaps: req.DelegateCaps, DelegateScope: authority.ScopeFromWire(req.DelegateScope, nil),
		CapsPresent: req.CapsPresent(), ScopePresent: req.ScopePresent(),
		DelegateCapsPresent: req.DelegateCapsPresent(), DelegateScopePresent: req.DelegateScopePresent(),
		SpawnDepth: req.SpawnDepth, MaxDescendants: req.MaxDescendants,
		SpawnDepthPresent: req.SpawnDepthPresent(), MaxDescendantsPresent: req.MaxDescendantsPresent(),
	}
}

func credentialOK(id string, expiresAt time.Time) protocol.CredentialResponse {
	return protocol.CredentialResponse{Status: protocol.CredentialStatus_Ok, TaskId: taskIDFromHex(id), ExpiresAt: uint64(expiresAt.UnixNano())}
}

// credentialIssue creates a credential task through the spawn grant path
// (D13): a child of the caller, holding at most what the caller may delegate
// within its scope, counted against its spawn limits. The expiry is the
// server's clock plus the ttl, never a client's absolute time.
func (h *TaskHandler) credentialIssue(cid string, req *protocol.CredentialIssueRequest, now time.Time) protocol.CredentialResponse {
	ttl, refusal := credentialTTL(req.TtlMs)
	if refusal != nil {
		return *refusal
	}
	if refusal := credentialKeyRefusal(req.Pubkey); refusal != nil {
		return *refusal
	}
	if refusal := maskedRefusal(req.RequestedCaps | req.DelegateCaps); refusal != nil {
		return *refusal
	}
	g := h.granter(cid, h.callerCaps(cid))
	u, err := h.grantFor(g, issueGrantRequest(req), "")
	if err != nil {
		return credentialRefusal(protocol.CredentialStatus_ScopeNotPermitted, "%s", err.Error())
	}
	expiresAt := now.Add(ttl)
	id, err := h.Tasks.CreateCredential(h.lookupPrincipal(cid), h.lookupClientKind(cid), u.To, req.Pubkey, expiresAt, string(req.Name))
	if err != nil {
		return credentialRefusal(protocol.CredentialStatus_SpawnLimit, "%s", err.Error())
	}
	slog.Info("credential issued", "task", id, "caps", u.To.Caps.String(), "expires_at", expiresAt)
	return credentialOK(id, expiresAt)
}

// credentialRekey is a credential's resume (D18): a new key and a new expiry
// under the same id, gated as issue rather than as resume. The caller must
// reach the credential through its credential_issue scope, and must be able
// to issue the credential's authority now — the recorded grant, or the
// narrowed one the request writes — so a re-key confers nothing a fresh
// issue would not.
func (h *TaskHandler) credentialRekey(cid string, req *protocol.CredentialRekeyRequest, now time.Time) protocol.CredentialResponse {
	idHex := hex.EncodeToString(req.TaskId.Id[:])
	if !h.inScope(cid, protocol.Capability_CredentialIssue, idHex) {
		return credentialRefusal(protocol.CredentialStatus_NoSuchTask, "")
	}
	rec, ok := h.Tasks.Get(idHex)
	if !ok {
		return credentialRefusal(protocol.CredentialStatus_NoSuchTask, "")
	}
	if rec.Kind != protocol.TaskKind_Credential {
		return credentialRefusal(protocol.CredentialStatus_NotCredential, "task %s is a %s task, not a credential", short(idHex), rec.Kind)
	}
	ttl, refusal := credentialTTL(req.TtlMs)
	if refusal != nil {
		return *refusal
	}
	if refusal := credentialKeyRefusal(req.Pubkey); refusal != nil {
		return *refusal
	}
	if req.Pubkey == rec.CredentialPubKey {
		// A re-key always takes a new key: one replaced is never valid again.
		return credentialRefusal(protocol.CredentialStatus_BadPubkey, "the credential already holds this key; a re-key takes a new one")
	}
	var written protocol.Capability
	if req.CapsPresent() {
		written |= req.RequestedCaps
	}
	if req.DelegateCapsPresent() {
		written |= req.DelegateCaps
	}
	if refusal := maskedRefusal(written); refusal != nil {
		return *refusal
	}

	callerCaps := h.callerCaps(cid)
	g := h.granter(cid, callerCaps)
	u, err := h.grantFor(g, rekeyGrantRequest(req), idHex)
	if err != nil {
		return credentialRefusal(protocol.CredentialStatus_ScopeNotPermitted, "%s", err.Error())
	}
	// Issue-equivalence, over the authority the credential will hold.
	caps, scope := rec.Capabilities, rec.Scope
	if u.Caps {
		caps = u.To.Caps
	}
	if u.Scope {
		scope = u.To.Scope
	}
	if lacking := caps &^ g.ChildCaps(caps); lacking != 0 {
		return credentialRefusal(protocol.CredentialStatus_ScopeNotPermitted,
			"the credential holds %s, which you could not issue; narrow it with --caps", authority.CapsLabel(lacking))
	}
	issuable, err := g.ChildScope(scope, caps)
	if err != nil {
		return credentialRefusal(protocol.CredentialStatus_ScopeNotPermitted, "%s", err.Error())
	}
	if authority.IsNarrowing(caps, scope, caps, issuable) {
		return credentialRefusal(protocol.CredentialStatus_ScopeNotPermitted,
			"the credential's scope %s reaches further than you could issue (%s); narrow it with --scope", scope, issuable)
	}

	expiresAt := now.Add(ttl)
	after, err := h.Tasks.RekeyCredential(idHex, req.Pubkey, expiresAt, &u,
		AuthorityWrite{Source: protocol.AuthoritySource_Resume, Actor: h.lookupPrincipal(cid)})
	switch {
	case errors.Is(err, ErrNoSuchTask):
		return credentialRefusal(protocol.CredentialStatus_NoSuchTask, "")
	case errors.Is(err, ErrNotCredential):
		return credentialRefusal(protocol.CredentialStatus_NotCredential, "task %s is not a credential", short(idHex))
	case err != nil:
		return credentialRefusal(protocol.CredentialStatus_NoSuchTask, "%s", err.Error())
	}
	// The old key's connections already hold nothing (principalRecord compares
	// the key per request); closing them tells their peers at once (D18: every
	// connection authenticated with it is closed). OnCancel does not do it
	// here: a live credential's re-key is no cancel.
	closed := 0
	if h.DropConnsForPrincipal != nil {
		closed = h.DropConnsForPrincipal(idHex)
	}
	slog.Info("credential re-keyed", "task", idHex, "caps", after.Capabilities.String(), "expires_at", expiresAt, "conns_closed", closed)
	return credentialOK(idHex, expiresAt)
}

// credentialResumeRefusal is the answer every generic resume path gives a
// credential task: it has its own resume, the re-key, and none of them may
// queue it for a runner.
func credentialResumeRefusal(idHex string) string {
	return "a credential task is re-keyed, not resumed: credential rekey " + idHex
}
