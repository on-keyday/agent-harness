package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/trsf/wire"
)

func TestPSKGate_NoPSKConfig(t *testing.T) {
	g := newPSKGate(nil)
	if !g.Authed() {
		t.Fatal("gate with nil PSK must be pre-authenticated")
	}
	isPSK, shouldClose := g.Check([]byte{byte(wire.ApplicationPayloadKind_TaskControl), 0x00}, func([]byte) {})
	if isPSK || shouldClose {
		t.Errorf("no-PSK gate: isPSK=%v shouldClose=%v, want false false", isPSK, shouldClose)
	}
}

func TestPSKGate_CorrectPSK(t *testing.T) {
	psk := []byte("s3cr3t")
	g := newPSKGate(psk)
	if g.Authed() {
		t.Fatal("gate with PSK must not be pre-authenticated")
	}
	var sent []byte
	data := append([]byte{byte(wire.ApplicationPayloadKind_PskAuth)}, psk...)
	isPSK, shouldClose := g.Check(data, func(b []byte) { sent = append(sent, b...) })
	if !isPSK || shouldClose {
		t.Errorf("correct PSK: isPSK=%v shouldClose=%v, want true false", isPSK, shouldClose)
	}
	if !g.Authed() {
		t.Error("gate must be authed after correct PSK")
	}
	if len(sent) < 2 || wire.PskAuthStatus(sent[1]) != wire.PskAuthStatus_Ok {
		t.Errorf("response = %v, want [PskAuth Ok]", sent)
	}
}

func TestPSKGate_WrongPSK(t *testing.T) {
	g := newPSKGate([]byte("s3cr3t"))
	var sent []byte
	data := append([]byte{byte(wire.ApplicationPayloadKind_PskAuth)}, []byte("wrong")...)
	isPSK, shouldClose := g.Check(data, func(b []byte) { sent = append(sent, b...) })
	if !isPSK || !shouldClose {
		t.Errorf("wrong PSK: isPSK=%v shouldClose=%v, want true true", isPSK, shouldClose)
	}
	if g.Authed() {
		t.Error("gate must not be authed after wrong PSK")
	}
	if len(sent) < 2 || wire.PskAuthStatus(sent[1]) != wire.PskAuthStatus_BadPsk {
		t.Errorf("response = %v, want [PskAuth BadPsk]", sent)
	}
}

func TestPSKGate_NonPSKMessageBeforeAuth(t *testing.T) {
	g := newPSKGate([]byte("s3cr3t"))
	isPSK, shouldClose := g.Check([]byte{byte(wire.ApplicationPayloadKind_TaskControl), 0x00}, func([]byte) {})
	if isPSK || !shouldClose {
		t.Errorf("non-PSK before auth: isPSK=%v shouldClose=%v, want false true", isPSK, shouldClose)
	}
}

func TestPSKGate_AlreadyAuthed(t *testing.T) {
	g := newPSKGate(nil) // pre-authed
	data := append([]byte{byte(wire.ApplicationPayloadKind_PskAuth)}, []byte("anything")...)
	isPSK, shouldClose := g.Check(data, func([]byte) {})
	if isPSK || shouldClose {
		t.Errorf("authed gate: isPSK=%v shouldClose=%v, want false false", isPSK, shouldClose)
	}
}
