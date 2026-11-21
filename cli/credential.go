package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
)

// Credential tasks (docs/superpowers/specs/2026-11-21-credential-tasks-design.md):
// an expiring identity for a script outside the harness. This file is the
// client's half that carries no I/O on the host: the credential value, its
// handshake, the verbs' requests and the rows they render. Where a credential
// is read from, and where an issued secret is written, are credential_source.go
// and credential_cli.go (native builds only: a browser is an operator surface).

// Environment names a script carries its credential in. Read in this order:
// --credential-file, CredentialFileEnv, CredentialEnv (first match wins).
const (
	CredentialFileEnv = "HARNESS_CREDENTIAL_FILE"
	CredentialEnv     = "HARNESS_CREDENTIAL"
)

// Credential is what a script holds: the task it is, and the key that proves
// it. Its string form `<32 hex task id>:<64 hex Ed25519 seed>` is the one
// secret; the server stores the public half only.
type Credential struct {
	TaskID protocol.TaskID
	Key    ed25519.PrivateKey
}

// ParseCredential reads the string form. An error never repeats the input:
// its second half is a secret.
func ParseCredential(s string) (Credential, error) {
	const form = "credential: want <32 hex task id>:<64 hex seed>"
	s = strings.TrimSpace(s)
	id, seed, ok := strings.Cut(s, ":")
	if !ok || len(id) != 32 || len(seed) != 2*ed25519.SeedSize {
		return Credential{}, errors.New(form)
	}
	rawID, err := hex.DecodeString(id)
	if err != nil {
		return Credential{}, errors.New(form)
	}
	rawSeed, err := hex.DecodeString(seed)
	if err != nil {
		return Credential{}, errors.New(form)
	}
	var tid protocol.TaskID
	copy(tid.Id[:], rawID)
	return Credential{TaskID: tid, Key: ed25519.NewKeyFromSeed(rawSeed)}, nil
}

// Secret is the credential string, seed included: the one place a secret is
// handed over (a printed `secret:` line, a credential file). It is not String:
// anything that formats a Credential with %v, %s or %#v (a log line, an error)
// gets the redacted form below, never the seed.
func (c Credential) Secret() string {
	return hex.EncodeToString(c.TaskID.Id[:]) + ":" + hex.EncodeToString(c.Key.Seed())
}

// String is the redacted form: the task, no seed. GoString too, so %#v cannot
// print the key either.
func (c Credential) String() string {
	return "credential " + hex.EncodeToString(c.TaskID.Id[:])[:8]
}

func (c Credential) GoString() string { return c.String() }

// LogValue keeps the seed out of a structured log line.
func (c Credential) LogValue() slog.Value {
	return slog.StringValue(c.String())
}

// SendCredentialHandshake is the credential's counterpart of
// SendMergedHandshake: PskAuthRequest{role=credential, credential_auth=
// {task id, signature over this connection's transcript}}, then the server's
// answer. There is no binder and no ClientHello: the credential names the
// principal itself, and nothing else the process holds (the operator PSK, an
// agent's ticket) is sent.
func SendCredentialHandshake(ctx context.Context, sendFn func([]byte) error, transcript []byte, cred Credential, serverKey ed25519.PublicKey, respCh <-chan PskAuthReply) error {
	if len(cred.Key) != ed25519.PrivateKeySize {
		return fmt.Errorf("credential: no key")
	}
	req := protocol.PskAuthRequest{Role: protocol.AuthRole_Credential}
	req.SetBinder(nil)
	auth := protocol.CredentialAuth{TaskId: cred.TaskID}
	copy(auth.Sig[:], SignCredentialAuth(cred.Key, transcript))
	if !req.SetCredentialAuth(auth) {
		return fmt.Errorf("psk: SetCredentialAuth failed")
	}
	data, err := req.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
		return fmt.Errorf("psk: encode: %w", err)
	}
	if err := sendFn(data); err != nil {
		return fmt.Errorf("psk: send: %w", err)
	}
	select {
	case reply := <-respCh:
		// Signature first, then status (CheckPskAuthReply): a forged
		// rejection must not turn a retry into a fatal one.
		return CheckPskAuthReply(reply, serverKey, transcript)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// credentialRefusalError words the server's refusal of a credential's
// handshake for the script's operator, and leaves every other error alone. The
// wrapped *PskRejectedError stays reachable (errors.As), so PersistLoop's
// fatal/retryable split is unchanged: revoked, expired and bad_ticket are
// fatal.
func credentialRefusalError(cred Credential, err error) error {
	var rej *PskRejectedError
	if !errors.As(err, &rej) {
		return err
	}
	word := EnumWireName(rej.Code.String())
	var why string
	switch rej.Code {
	case protocol.PskAuthStatus_Revoked:
		why = "the credential was revoked; `credential rekey` gives its id a new key"
	case protocol.PskAuthStatus_Expired:
		why = "the credential expired; `credential rekey` gives its id a new key and expiry"
	case protocol.PskAuthStatus_BadTicket:
		why = "the key was not accepted: the task id is unknown, is not a credential task, or the seed is not its key"
	case protocol.PskAuthStatus_NoIdentity:
		why = "the server does not know credential connections: it needs upgrading"
	default:
		return err
	}
	return fmt.Errorf("credential %s refused (%s): %s: %w", hex.EncodeToString(cred.TaskID.Id[:])[:8], word, why, err)
}

// --- the requests ---

// credentialStatusGloss is what each CredentialStatus means to the person who
// typed the verb, used when the server sent no message of its own. Every
// status has one.
var credentialStatusGloss = map[protocol.CredentialStatus]string{
	protocol.CredentialStatus_TtlInvalid:        "a credential needs an expiry: pass --ttl",
	protocol.CredentialStatus_TtlTooLong:        "the ttl is longer than the server allows (at most 90d)",
	protocol.CredentialStatus_BadPubkey:         "not a usable Ed25519 public key",
	protocol.CredentialStatus_ScopeNotPermitted: "the caps or scope asked for are more than you may issue (credential_issue and your own caps and scope bound them)",
	protocol.CredentialStatus_SpawnLimit:        "your spawn limits do not allow another task",
	protocol.CredentialStatus_NotCredential:     "that task is not a credential",
	protocol.CredentialStatus_NoSuchTask:        "no such credential within your reach",
	protocol.CredentialStatus_MaskedCap:         "a credential can never hold credential_issue or schedule_edit",
	protocol.CredentialStatus_BadName:           "--name is at most 64 bytes of text, with no control characters",
}

// credentialStatusError is the error for a credential answer: nil for ok,
// otherwise `credential <verb>: <cause>`. A server message, when there is one,
// is the more exact; no_such_task always reads "no such credential within
// your reach", since an unreachable id and a missing one are the same answer.
func credentialStatusError(verbName string, r *protocol.CredentialResponse) error {
	if r.Status == protocol.CredentialStatus_Ok {
		return nil
	}
	what := credentialStatusGloss[r.Status]
	if what == "" {
		what = EnumWireName(r.Status.String())
	}
	if len(r.Message) > 0 && r.Status != protocol.CredentialStatus_NoSuchTask {
		what += ": " + string(r.Message)
	}
	return fmt.Errorf("credential %s: %s", verbName, what)
}

// credentialTTLMs is --ttl on the wire: milliseconds, a duration and never a
// clock time (the server adds it to its own clock).
func credentialTTLMs(s string) (uint64, error) {
	d, err := verb.ParseCredentialTTL(s)
	if err != nil {
		return 0, err
	}
	return uint64(d / time.Millisecond), nil
}

// credentialIssueRequest builds the wire request for credential new / rekey
// from the parsed action and the public key.
func credentialIssueRequest(a verb.CredentialIssueAction, pub ed25519.PublicKey) (*protocol.TaskControlRequest, error) {
	ttl, err := credentialTTLMs(a.TTL)
	if err != nil {
		return nil, err
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("credential: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	var pk [32]byte
	copy(pk[:], pub)
	var scope protocol.TaskScope
	var overrides []protocol.ScopeOverride
	if a.ScopePresent {
		if a.Scope != nil {
			scope = *a.Scope
		}
		overrides = a.Overrides
	}
	// Each half is written only behind its presence bit (a typed flag), so a
	// re-key narrows only what it names and an issue's unnamed halves are the
	// spawn defaults (no caps, subtree scope, nothing delegated).
	var caps, dcaps protocol.Capability
	if a.CapsPresent && a.Caps != nil {
		caps = *a.Caps
	}
	if a.DelegateCapsPresent && a.DelegateCaps != nil {
		dcaps = *a.DelegateCaps
	}
	var dscope protocol.TaskScope
	if a.DelegateScopePresent && a.DelegateScope != nil {
		dscope = *a.DelegateScope
	}
	var depth, desc protocol.SpawnLimit
	if a.SpawnDepthPresent && a.SpawnDepth != nil {
		depth = *a.SpawnDepth
	}
	if a.MaxDescendantsPresent && a.MaxDescendants != nil {
		desc = *a.MaxDescendants
	}

	if a.Sub == verb.SubRekey {
		tid, err := parseTaskIDHex(a.ID)
		if err != nil {
			return nil, fmt.Errorf("credential id: %w", err)
		}
		rk := protocol.CredentialRekeyRequest{
			TaskId: tid, Pubkey: pk, TtlMs: ttl,
			RequestedCaps: caps, Scope: scope, DelegateCaps: dcaps, DelegateScope: dscope,
			SpawnDepth: depth, MaxDescendants: desc,
		}
		rk.SetOverrides(overrides)
		rk.SetCapsPresent(a.CapsPresent)
		rk.SetScopePresent(a.ScopePresent)
		rk.SetDelegateCapsPresent(a.DelegateCapsPresent)
		rk.SetDelegateScopePresent(a.DelegateScopePresent)
		rk.SetSpawnDepthPresent(a.SpawnDepthPresent)
		rk.SetMaxDescendantsPresent(a.MaxDescendantsPresent)
		req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_CredentialRekey}}
		req.SetCredentialRekey(rk)
		return req, nil
	}
	is := protocol.CredentialIssueRequest{
		Pubkey: pk, TtlMs: ttl,
		RequestedCaps: caps, Scope: scope, DelegateCaps: dcaps, DelegateScope: dscope,
		SpawnDepth: depth, MaxDescendants: desc,
	}
	if !is.SetName([]byte(a.Name)) || len(a.Name) > 64 {
		return nil, fmt.Errorf("--name is %d bytes, at most 64", len(a.Name))
	}
	is.SetOverrides(overrides)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_CredentialIssue}}
	req.SetCredentialIssue(is)
	return req, nil
}

// CredentialIssued is a successful issue or re-key: the task, and when the
// server's clock ends it.
type CredentialIssued struct {
	TaskID    protocol.TaskID
	ExpiresAt time.Time
}

// CredentialIssue sends credential new or rekey and returns the credential
// task and its expiry.
func (c *Client) CredentialIssue(ctx context.Context, a verb.CredentialIssueAction, pub ed25519.PublicKey) (CredentialIssued, error) {
	verbName := "new"
	if a.Sub == verb.SubRekey {
		verbName = "rekey"
	}
	req, err := credentialIssueRequest(a, pub)
	if err != nil {
		return CredentialIssued{}, fmt.Errorf("credential %s: %w", verbName, err)
	}
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return CredentialIssued{}, err
	}
	if resp.Kind != req.Header.Kind {
		return CredentialIssued{}, fmt.Errorf("credential %s: unexpected response kind %v", verbName, resp.Kind)
	}
	r := resp.CredentialIssue()
	if a.Sub == verb.SubRekey {
		r = resp.CredentialRekey()
	}
	if r == nil {
		return CredentialIssued{}, fmt.Errorf("credential %s: empty response", verbName)
	}
	if err := credentialStatusError(verbName, r); err != nil {
		return CredentialIssued{}, err
	}
	return CredentialIssued{TaskID: r.TaskId, ExpiresAt: time.Unix(0, int64(r.ExpiresAt))}, nil
}

// CredentialHandover is an issue or re-key as its issuer receives it: the
// task and its expiry, and -- only when the key pair was generated on this
// side -- the credential whose Secret() is handed over once. Cred is nil when
// the holder's public key was registered (--pubkey): no secret exists here.
type CredentialHandover struct {
	CredentialIssued
	Cred *Credential
}

// IssueCredential runs credential new / rekey with either way of D3: --pubkey
// registers the holder's key, otherwise a pair is generated here and only its
// public half is sent. It is the core every surface shares; where the secret
// then goes (stdout, a --out file, the WebUI's one-time view) is the caller's.
func IssueCredential(ctx context.Context, c *Client, a verb.CredentialIssueAction) (CredentialHandover, error) {
	verbName := "new"
	if a.Sub == verb.SubRekey {
		verbName = "rekey"
	}
	var pub ed25519.PublicKey
	var priv ed25519.PrivateKey
	if a.Pubkey != "" {
		raw, err := hex.DecodeString(a.Pubkey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return CredentialHandover{}, fmt.Errorf("credential %s: --pubkey: want 64 hex characters", verbName)
		}
		pub = raw
	} else {
		var err error
		if pub, priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
			return CredentialHandover{}, err
		}
	}
	got, err := c.CredentialIssue(ctx, a, pub)
	if err != nil {
		return CredentialHandover{}, err
	}
	h := CredentialHandover{CredentialIssued: got}
	if priv != nil {
		h.Cred = &Credential{TaskID: got.TaskID, Key: priv}
	}
	return h, nil
}

// CredentialIssuedLine is the first line every surface prints for an issue or
// a re-key: `credential <id8>: issued, expires <RFC3339>`. No secret.
func CredentialIssuedLine(got CredentialIssued) string {
	return fmt.Sprintf("credential %s: issued, expires %s", hex.EncodeToString(got.TaskID.Id[:])[:8], got.ExpiresAt.Format(time.RFC3339))
}

// CredentialBindHint is what follows CredentialIssuedLine when the holder's
// public key was registered: the full id (the id8 above is not enough to
// bind) and the command the holder runs where `credential keygen` ran.
func CredentialBindHint(id string) string {
	return fmt.Sprintf("task id: %s\nnext, on the host that ran credential keygen:\n  harness-cli credential bind %s --file <your seed file>", id, id)
}

// --- the rows ---

// CredentialRows are the credential tasks in a snapshot, in its order.
func CredentialRows(tasks []protocol.TaskInfo) []protocol.TaskInfo {
	var out []protocol.TaskInfo
	for _, t := range tasks {
		if t.Kind == protocol.TaskKind_Credential {
			out = append(out, t)
		}
	}
	return out
}

// CredentialExpiryLabel is a credential's expiry as RFC3339, in the zone of
// the machine that prints it. A credential row always has one.
func CredentialExpiryLabel(t *protocol.TaskInfo) string {
	return time.Unix(0, int64(t.CredentialExpiresAt)).Format(time.RFC3339)
}

// CredentialLastUsedLabel is the last accepted handshake as RFC3339, or
// `unknown`: the server keeps it in memory only, so a restart clears it and
// zero means "not since the server started", never "never used".
func CredentialLastUsedLabel(t *protocol.TaskInfo) string {
	if t.CredentialLastUsedAt == 0 {
		return "unknown"
	}
	return time.Unix(0, int64(t.CredentialLastUsedAt)).Format(time.RFC3339)
}

// CredentialTimeCell is a credential time for a table cell, where RFC3339
// does not fit: `2006-01-02 15:04` on the reader's clock, or `unknown` for a
// zero reading (a last use the server has not seen since it started). The
// detail views carry the full RFC3339 labels above.
func CredentialTimeCell(ns uint64) string {
	if ns == 0 {
		return "unknown"
	}
	return time.Unix(0, int64(ns)).Format("2006-01-02 15:04")
}

// credentialAboutText is a credential row's About text (TaskNowText): it has
// no prompt, title or progress, so the row says what it is and when it ends,
// by name -- or by its id8, as a board sender does, when it has none. The
// name is an issuer's text and reaches a terminal, so it is sanitized.
func credentialAboutText(t protocol.TaskInfo) string {
	who := hex.EncodeToString(t.Id.Id[:])[:8]
	if len(t.Name) > 0 {
		who = taskNameText(t.Name)
	}
	return "credential " + who + " expires " + CredentialExpiryLabel(&t)
}

// taskNameText is a task name as a terminal may print it. The server refuses
// a control character in a name (bad_name), but a row written before that
// check, or by a server without it, still carries one; this is the same rule
// the TUI applies.
func taskNameText(name []byte) string {
	n := string(name)
	return vtgrid.SanitizeTitle(n, len(n))
}

// CredentialRowLine is one `credential ls` row.
func CredentialRowLine(t protocol.TaskInfo) string {
	name := "-"
	if len(t.Name) > 0 {
		name = taskNameText(t.Name)
	}
	return fmt.Sprintf("%s  %s  name=%s  expires=%s  last_used=%s  caps=%s  scope=%s",
		hex.EncodeToString(t.Id.Id[:]), strings.TrimSpace(taskStatusStr(t.Status)), name,
		CredentialExpiryLabel(&t), CredentialLastUsedLabel(&t), CapsLabel(t.Capabilities), ScopeLabel(t.Scope))
}

// credentialJSON is a `credential ls --json` row. Every field is always
// present; the two times are RFC3339 strings, last use `unknown` when the
// server has not seen the credential since it started.
type credentialJSON struct {
	ID                   string `json:"id"`
	Status               string `json:"status"`
	Name                 string `json:"name"`
	CredentialExpiresAt  string `json:"credential_expires_at"`
	CredentialLastUsedAt string `json:"credential_last_used_at"`
	Caps                 string `json:"caps"`
	Scope                string `json:"scope"`
	CreatedBy            string `json:"created_by"`
}

// CredentialListText writes `credential ls`; CredentialListJSON is its
// --json document. A surface that fetched the snapshot itself renders through
// these.
func CredentialListText(out io.Writer, tasks []protocol.TaskInfo) {
	rows := CredentialRows(tasks)
	if len(rows) == 0 {
		fmt.Fprintln(out, "no credentials")
	}
	for _, t := range rows {
		fmt.Fprintln(out, CredentialRowLine(t))
	}
}

func CredentialListJSON(out io.Writer, tasks []protocol.TaskInfo) error {
	rows := []credentialJSON{}
	for _, t := range CredentialRows(tasks) {
		rows = append(rows, credentialJSON{
			ID:                   hex.EncodeToString(t.Id.Id[:]),
			Status:               taskStatusJSON(t.Status),
			Name:                 string(t.Name),
			CredentialExpiresAt:  CredentialExpiryLabel(&t),
			CredentialLastUsedAt: CredentialLastUsedLabel(&t),
			Caps:                 CapsLabel(t.Capabilities),
			Scope:                ScopeLabel(t.Scope),
			CreatedBy:            taskIDHexOrEmpty(t.CreatorTaskId.Id[:]),
		})
	}
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", b)
	return err
}

// CredentialLsWith runs credential ls.
func CredentialLsWith(ctx context.Context, c *Client, asJSON bool, out io.Writer) error {
	lr, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		return CredentialListJSON(out, lr.Tasks)
	}
	CredentialListText(out, lr.Tasks)
	return nil
}

// CredentialRevokeWith runs credential revoke: the existing cancel, once the
// id is known to be a credential in the caller's reach. The cancel verb ends
// any task, so the kind is checked here, and an id that is not a credential
// answers as one outside reach does.
func CredentialRevokeWith(ctx context.Context, c *Client, id string, out io.Writer) error {
	tid, err := parseTaskIDHex(id)
	if err != nil {
		return fmt.Errorf("credential id: %w", err)
	}
	lr, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, t := range CredentialRows(lr.Tasks) {
		if t.Id == tid {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("credential revoke: no such credential within your reach")
	}
	if err := c.Cancel(ctx, hex.EncodeToString(tid.Id[:])); err != nil {
		return fmt.Errorf("credential revoke: %w", err)
	}
	fmt.Fprintf(out, "credential %s: revoked; its connections are closed and its next handshake is refused (credential rekey brings the id back with a new key)\n", hex.EncodeToString(tid.Id[:])[:8])
	return nil
}
