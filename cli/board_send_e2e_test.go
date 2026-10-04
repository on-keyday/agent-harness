//go:build !js

package cli_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoardSend_E2E_OperatorStampAndReplyRoute(t *testing.T) {
	e, cid := startOperatorServerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b := e.Board()

	// An agent message the operator answers by seq alone.
	var agentTid protocol.TaskID
	agentTid.Id[0] = 0xcd
	parent, _, err := b.Send("shared", []byte("q"), protocol.RunnerID{}, agentTid, "h", "claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cli.BoardSend(ctx, cid, cli.BoardSendParams{InReplyTo: parent, ReplyTo: agentboard.OperatorTopic}, []byte("answer"))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := b.Retained(res.Seq)
	if !ok || m.SenderKind != protocol.SenderKind_Operator || m.Topic != agentboard.SelfTopic(agentTid) {
		t.Fatalf("retained = %+v, want operator kind on the author's chat topic", m)
	}
	if string(m.Payload) != "answer" || m.ReplyToTopic != agentboard.OperatorTopic {
		t.Fatalf("payload=%q reply_to=%q", m.Payload, m.ReplyToTopic)
	}

	// No route: an operator message without --reply-to cannot be answered by seq alone.
	bare, err := cli.BoardSend(ctx, cid, cli.BoardSendParams{Topic: "chat.cd000000"}, []byte("fyi"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cli.BoardSend(ctx, cid, cli.BoardSendParams{InReplyTo: bare.Seq}, []byte("re"))
	var nr *cli.NoReplyRouteError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want NoReplyRouteError", err)
	}

	// board wake on a topic nobody subscribes.
	if n, err := cli.BoardWake(ctx, cid, "chat.nobody0"); err != nil || n != 0 {
		t.Fatalf("wake = %d, %v; want 0, nil", n, err)
	}
}

func TestBoardSend_E2E_NoWakeIsRetained(t *testing.T) {
	e, cid := startOperatorServerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cli.BoardSend(ctx, cid, cli.BoardSendParams{Topic: "chat.q0000000", NoWake: true}, []byte("queued"))
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := e.Board().Retained(res.Seq); !ok || string(m.Payload) != "queued" {
		t.Fatalf("no-wake message not retained: %+v", m)
	}
}
