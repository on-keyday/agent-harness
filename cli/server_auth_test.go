package cli_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signedResp(t *testing.T, priv ed25519.PrivateKey, transcript []byte, st protocol.PskAuthStatus) protocol.PskAuthResponse {
	t.Helper()
	resp := protocol.PskAuthResponse{Status: st}
	if !resp.SetServerSig(cli.SignServerAuth(priv, transcript, st)) {
		t.Fatal("SetServerSig failed")
	}
	return resp
}

func TestVerifyServerAuth(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, _ := newKey(t)
	tr := []byte("transcript-A")
	ok := signedResp(t, priv, tr, protocol.PskAuthStatus_Ok)

	if err := cli.VerifyServerAuth(pub, tr, &ok); err != nil {
		t.Fatalf("genuine signature: %v", err)
	}
	if err := cli.VerifyServerAuth(nil, tr, &protocol.PskAuthResponse{}); err != nil {
		t.Fatalf("no key configured must not verify: %v", err)
	}

	var sae *cli.ServerAuthError
	cases := map[string]error{
		"other key":        cli.VerifyServerAuth(otherPub, tr, &ok),
		"other transcript": cli.VerifyServerAuth(pub, []byte("transcript-B"), &ok),
		"missing sig":      cli.VerifyServerAuth(pub, tr, &protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}),
	}
	// Status is inside the signed bytes: an ok signature on a bad_psk status fails.
	swapped := ok
	swapped.Status = protocol.PskAuthStatus_BadPsk
	cases["status swapped"] = cli.VerifyServerAuth(pub, tr, &swapped)
	for name, err := range cases {
		if !errors.As(err, &sae) {
			t.Errorf("%s: err = %v, want *cli.ServerAuthError", name, err)
		}
	}
}

// A signature over the same transcript and status but without the context
// string must not verify: the context string is part of the signed message.
func TestVerifyServerAuth_ContextStringIsSigned(t *testing.T) {
	pub, priv := newKey(t)
	tr := []byte("transcript")
	raw := append(append([]byte{}, tr...), byte(protocol.PskAuthStatus_Ok))
	resp := protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}
	resp.SetServerSig(ed25519.Sign(priv, raw))
	if err := cli.VerifyServerAuth(pub, tr, &resp); err == nil {
		t.Fatal("a signature without the context string verified")
	}
}

func TestParseServerPubKey(t *testing.T) {
	pub, _ := newKey(t)
	h := hex.EncodeToString(pub)
	got, err := cli.ParseServerPubKey(h + "\n")
	if err != nil || !got.Equal(pub) {
		t.Fatalf("trailing newline: got %x, %v", got, err)
	}
	if _, err := cli.ParseServerPubKey(h[:62]); err == nil || !strings.Contains(err.Error(), "32") {
		t.Fatalf("short key: err = %v, want a message naming 32 bytes", err)
	}
	if _, err := cli.ParseServerPubKey("zz" + h[2:]); err == nil {
		t.Fatal("non-hex key parsed")
	}
}

func TestParseServerKeySeed(t *testing.T) {
	_, priv := newKey(t)
	got, err := cli.ParseServerKeySeed(hex.EncodeToString(priv.Seed()) + "\n")
	if err != nil || !got.Equal(priv) {
		t.Fatalf("seed round trip: %v", err)
	}
	if _, err := cli.ParseServerKeySeed("abcd"); err == nil {
		t.Fatal("short seed parsed")
	}
}

func TestResolveServerPubKey(t *testing.T) {
	pub, _ := newKey(t)
	h := hex.EncodeToString(pub)
	file := filepath.Join(t.TempDir(), "server-key.pub")
	if err := os.WriteFile(file, []byte(h+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if k, err := cli.ResolveServerPubKey("", ""); k != nil || err != nil {
		t.Fatalf("nothing configured: got %x, %v; want nil, nil", k, err)
	}
	if k, err := cli.ResolveServerPubKey("", file); err != nil || !k.Equal(pub) {
		t.Fatalf("file: %v", err)
	}
	// The value wins over the file, even a file that does not exist.
	if k, err := cli.ResolveServerPubKey(h, filepath.Join(t.TempDir(), "absent")); err != nil || !k.Equal(pub) {
		t.Fatalf("value over file: %v", err)
	}
	// A configured file that cannot be read is an error, not "unset".
	if _, err := cli.ResolveServerPubKey("", filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("unreadable configured file resolved to no key")
	}
}

func TestCheckPskAuthReply(t *testing.T) {
	pub, priv := newKey(t)
	tr := []byte("tr")

	ok := cli.PskAuthReply{Resp: signedResp(t, priv, tr, protocol.PskAuthStatus_Ok)}
	if err := cli.CheckPskAuthReply(ok, pub, tr); err != nil {
		t.Fatalf("verified ok: %v", err)
	}

	bad := cli.PskAuthReply{Resp: signedResp(t, priv, tr, protocol.PskAuthStatus_BadPsk)}
	var rej *cli.PskRejectedError
	if err := cli.CheckPskAuthReply(bad, pub, tr); !errors.As(err, &rej) || rej.Code != protocol.PskAuthStatus_BadPsk {
		t.Fatalf("verified bad_psk: err = %v, want PskRejectedError(bad_psk)", err)
	}

	// Forged rejection: must be ServerAuthError, NOT PskRejectedError, or an
	// impostor could make a runner exit for good.
	_, otherPriv := newKey(t)
	forged := cli.PskAuthReply{Resp: signedResp(t, otherPriv, tr, protocol.PskAuthStatus_BadPsk)}
	err := cli.CheckPskAuthReply(forged, pub, tr)
	var sae *cli.ServerAuthError
	if !errors.As(err, &sae) || errors.As(err, &rej) {
		t.Fatalf("forged bad_psk: err = %v, want ServerAuthError only", err)
	}
}

func TestDecodePskAuthReply(t *testing.T) {
	_, priv := newKey(t)
	resp := signedResp(t, priv, []byte("tr"), protocol.PskAuthStatus_Ok)
	payload, err := resp.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := cli.DecodePskAuthReply(payload); r.Err != nil || len(r.Resp.ServerSig) != 64 {
		t.Fatalf("new-format payload: %+v", r)
	}
	// An old server sends the status byte alone.
	r := cli.DecodePskAuthReply([]byte{byte(protocol.PskAuthStatus_Ok)})
	if r.Err == nil || !strings.Contains(r.Err.Error(), "server response did not decode") {
		t.Fatalf("old-format payload: err = %v", r.Err)
	}
	if err := cli.CheckPskAuthReply(r, nil, nil); err != r.Err {
		t.Fatalf("CheckPskAuthReply must surface the decode error even with no key: %v", err)
	}
}
