package objproto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"io"
	"time"

	"github.com/on-keyday/agent-harness/objproto/packet"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const AES128GCM = packet.CommonKeyKind_Aes128Gcm
const AES192GCM = packet.CommonKeyKind_Aes192Gcm
const AES256GCM = packet.CommonKeyKind_Aes256Gcm
const ChaCha20Poly1305 = packet.CommonKeyKind_Chacha20Poly1305

func NewHandshake(key []byte, kind packet.KeyKind, commonKeyKind packet.CommonKeyKind, offset uint16) (*packet.Handshake, error) {
	probe := &packet.Handshake{
		KeyKind:       kind,
		CommonKeyKind: commonKeyKind,
		Len:           uint16(len(key)),
		KeyShare:      key,
	}
	if kind == packet.KeyKind_Offset {
		probe.SetOffset(offset)
	}
	return probe, nil
}

func NewECDHHandshake(curve ecdh.Curve, commonKeyKind packet.CommonKeyKind) ([]byte, *packet.Handshake, error) {
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	kind := packet.KeyKind_Offset
	switch curve {
	case ecdh.X25519():
		kind = packet.KeyKind_X25519
	case ecdh.P256():
		kind = packet.KeyKind_P256
	case ecdh.P384():
		kind = packet.KeyKind_P384
	case ecdh.P521():
		kind = packet.KeyKind_P521
	default:
		return nil, nil, fmt.Errorf("unsupported curve: %v", curve)
	}
	probeData, err := NewHandshake(priv.PublicKey().Bytes(), kind, commonKeyKind, 0)
	if err != nil {
		return nil, nil, err
	}
	return priv.Bytes(), probeData, nil
}

func CurveFromKeyKind(kind packet.KeyKind) (ecdh.Curve, error) {
	switch kind {
	case packet.KeyKind_X25519:
		return ecdh.X25519(), nil
	case packet.KeyKind_P256:
		return ecdh.P256(), nil
	case packet.KeyKind_P384:
		return ecdh.P384(), nil
	case packet.KeyKind_P521:
		return ecdh.P521(), nil
	default:
		return nil, fmt.Errorf("unsupported key kind: %v", kind)
	}
}

func DoECDHHandshake(ctx context.Context, sess Endpoint, cid ConnectionID, curve ecdh.Curve, commonKeyKind packet.CommonKeyKind) (Connection, error) {
	priv, probe, err := NewECDHHandshake(curve, commonKeyKind)
	if err != nil {
		return nil, err
	}
	ch, err := sess.SendHandshake(cid, priv, probe)
	if err != nil {
		return nil, err
	}
	active, err := ch.WaitWithTimeout(ctx, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return active, nil
}

func DeriveKey(secret []byte, context string, keyLen int) (key []byte, err error) {
	hash := sha512.New
	hkdf := hkdf.New(hash, secret, nil, []byte(context))
	key = make([]byte, keyLen)
	_, err = io.ReadFull(hkdf, key)
	return key, err
}

// ComputePSKBinder derives a transcript-bound authenticator from the PSK, in
// the style of the TLS 1.3 PSK binder: HMAC over the objproto handshake
// transcript, keyed by a key derived from the PSK.
//
// objproto's handshake is deliberately unauthenticated (it provides the
// "encrypt first" half of a TLS-1.3-style exchange and exports the transcript
// via Connection.GetTranscript so an upper layer can authenticate). Binding the
// PSK proof to that transcript is what closes the gap: it turns the PSK from a
// replayable bearer secret (sent verbatim inside an unauthenticated channel)
// into a channel authenticator. An active MITM that relays two separate
// handshakes ends up with two different transcripts, so a binder captured on
// one leg does not validate on the other, and the PSK itself never crosses the
// wire. Both ends derive the same transcript (clientHandshake ‖ serverAck), so
// the binders match for a genuine end-to-end handshake.
func ComputePSKBinder(psk, transcript []byte) ([]byte, error) {
	binderKey, err := DeriveKey(psk, "ksdk-psk-binder", 32)
	if err != nil {
		return nil, fmt.Errorf("psk binder key derive: %w", err)
	}
	mac := hmac.New(sha512.New, binderKey)
	mac.Write(transcript)
	return mac.Sum(nil), nil
}

func NewAEADFromCommonKeyKind(kind packet.CommonKeyKind, key []byte) (cipher.AEAD, error) {
	switch kind {
	case packet.CommonKeyKind_Aes128Gcm:
		if len(key) < 16 {
			return nil, fmt.Errorf("invalid key length for AES-128-GCM: %d", len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("failed to create AES-128-GCM cipher: %w", err)
		}
		return cipher.NewGCM(block)
	case packet.CommonKeyKind_Aes192Gcm:
		if len(key) < 24 {
			return nil, fmt.Errorf("invalid key length for AES-192-GCM: %d", len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("failed to create AES-192-GCM cipher: %w", err)
		}
		return cipher.NewGCM(block)
	case packet.CommonKeyKind_Aes256Gcm:
		if len(key) < 32 {
			return nil, fmt.Errorf("invalid key length for AES-256-GCM: %d", len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("failed to create AES-256-GCM cipher: %w", err)
		}
		return cipher.NewGCM(block)
	case packet.CommonKeyKind_Chacha20Poly1305:
		if len(key) < 32 {
			return nil, fmt.Errorf("invalid key length for ChaCha20-Poly1305: %d", len(key))
		}
		return chacha20poly1305.New(key)
	default:
		return nil, fmt.Errorf("unsupported common key kind: %v", kind)
	}
}

func ECDHFromHandshake(selfPrivate []byte, probe *packet.Handshake) ([]byte, packet.CommonKeyKind, error) {
	curve, err := CurveFromKeyKind(probe.KeyKind)
	if err != nil {
		return nil, 0, err
	}
	peerPub, err := curve.NewPublicKey(probe.KeyShare)
	if err != nil {
		return nil, 0, err
	}
	selfPriv, err := curve.NewPrivateKey(selfPrivate)
	if err != nil {
		return nil, 0, err
	}
	shared, err := selfPriv.ECDH(peerPub)
	if err != nil {
		return nil, 0, err
	}
	return shared, probe.CommonKeyKind, nil
}
