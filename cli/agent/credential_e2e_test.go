package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/connauth"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The agent verbs work under a loaded credential (credential-tasks D10): a
// script with only HARNESS_CREDENTIAL sends to a peer's chat topic and reads
// its own.
func TestAgentVerbsRunUnderCredential(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	var ticketB [16]byte
	ticketB[0] = 0xB2
	tidB := mkTidE2E(0x32)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9301, 32)
	e2eRegisterTask(t, board, ridB, tidB, ticketB, "claude")

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var k [32]byte
	copy(k[:], pub)
	idB := hex.EncodeToString(tidB.Id[:])
	idHex, err := srv.Tasks().CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, authority.Grant{
		Caps:  protocol.Capability_BoardPeer,
		Scope: authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{idB}},
	}, k, time.Now().Add(time.Hour), "ci")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(idHex)
	var credTID protocol.TaskID
	copy(credTID.Id[:], raw)
	cred := connauth.Credential{TaskID: credTID, Key: priv}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The script's env: the server and the credential, nothing of an agent.
	for _, v := range []string{"HARNESS_RUNNER_ID", "HARNESS_TASK_ID", "HARNESS_AUTH_TICKET", "HARNESS_OPERATOR_PSK", connauth.CredentialFileEnv} {
		t.Setenv(v, "")
	}
	t.Setenv("HARNESS_SERVER_CID", "ws:"+addr+"-*")
	t.Setenv(connauth.CredentialEnv, cred.Secret())

	// send to a peer's chat topic.
	var sendOut bytes.Buffer
	if err := agent.Send(ctx, []string{"--topic", "chat." + idB[:8], "--data", "from the script"}, nil, &sendOut); err != nil {
		t.Fatalf("agent send under a credential: %v", err)
	}

	// The peer received it. It is a real agent: the credential is out of ITS
	// env (a loaded credential would win over its agent identity).
	t.Setenv(connauth.CredentialEnv, "")
	restore := setAgentEnv(addr, ridB, tidB, ticketB)
	var waitOut bytes.Buffer
	werr := agent.Wait(ctx, []string{"--topic", "chat." + idB[:8], "--timeout", "3s"}, &waitOut)
	restore()
	if werr != nil {
		t.Fatalf("peer wait: %v", werr)
	}
	if !strings.Contains(waitOut.String(), "from the script") {
		t.Fatalf("the peer did not receive the credential's message:\n%s", waitOut.String())
	}

	// Own inbox: something lands on the credential's chat topic, and
	// `agent wait` and `agent read --self` under the credential read it.
	if _, _, err := board.Send("chat."+idHex[:8], []byte("to the script"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"HARNESS_RUNNER_ID", "HARNESS_TASK_ID", "HARNESS_AUTH_TICKET"} {
		t.Setenv(v, "")
	}
	t.Setenv(connauth.CredentialEnv, cred.Secret())
	var inOut bytes.Buffer
	if err := agent.Wait(ctx, []string{"--topic", "chat." + idHex[:8], "--timeout", "3s"}, &inOut); err != nil {
		t.Fatalf("agent wait on the credential's own chat: %v", err)
	}
	if !strings.Contains(inOut.String(), "to the script") {
		t.Fatalf("the credential did not read its own inbox:\n%s", inOut.String())
	}
	// --self resolves to the credential's task id, not to HARNESS_TASK_ID.
	var readOut bytes.Buffer
	if err := agent.Read(ctx, []string{"--self"}, &readOut); err != nil {
		t.Fatalf("agent read --self under a credential: %v", err)
	}
	if !strings.Contains(readOut.String(), "to the script") {
		t.Fatalf("agent read --self did not name the credential's own chat:\n%s", readOut.String())
	}
}
