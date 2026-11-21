package server

import (
	"math/big"
)

// credentialKeyUsable reports whether pub may be stored as a credential's key
// (CredentialStatus.bad_pubkey otherwise): the canonical encoding of a point
// on the Ed25519 curve, and not one of small order.
//
// Small order is the reason this exists. Go's ed25519.Verify accepts a
// signature R, S under public key A when [S]B = R + [k]A; with A of order
// dividing 8, [k]A takes at most 8 values, so a signature made with no key at
// all verifies with high probability (measured: 62 of 100 forgeries under the
// all-zero key). The gate checks the signature before anything else, so such a
// key would let anyone holding the task id authenticate as the credential.
//
// Off-curve and non-canonical encodings are refused as well: Verify rejects
// every signature under an off-curve key, so the credential could never be
// used, and no client generates a non-canonical one.
//
// Run at issue and re-key only, the two writers of a credential's key; it is
// big-number arithmetic and stays off the per-request path.
func credentialKeyUsable(pub [32]byte) bool {
	return !hasSmallOrder(pub) && onCurveCanonical(pub)
}

// smallOrderEncodings is libsodium's blocklist of Ed25519 points of small
// order (ge25519_has_small_order, src/libsodium/crypto_core/ed25519/ref10/
// ed25519_ref10.c), compared with the sign bit (bit 255) masked off so both
// signs of each x are covered. The y values: 0 (order 4, x = ±sqrt(-1)),
// 1 (the identity), the two of order 8, p-1 (order 2), and the
// non-canonical p and p+1, which decode to 0 and 1. With both signs that is
// every one of the 8 points of order dividing 8; credential_key_test.go
// derives them rather than trusting this list.
var smallOrderEncodings = [][32]byte{
	// 0 (order 4)
	{},
	// 1 (order 1)
	{0x01},
	// order 8
	{0x26, 0xe8, 0x95, 0x8f, 0xc2, 0xb2, 0x27, 0xb0, 0x45, 0xc3, 0xf4, 0x89, 0xf2, 0xef, 0x98, 0xf0,
		0xd5, 0xdf, 0xac, 0x05, 0xd3, 0xc6, 0x33, 0x39, 0xb1, 0x38, 0x02, 0x88, 0x6d, 0x53, 0xfc, 0x05},
	// order 8
	{0xc7, 0x17, 0x6a, 0x70, 0x3d, 0x4d, 0xd8, 0x4f, 0xba, 0x3c, 0x0b, 0x76, 0x0d, 0x10, 0x67, 0x0f,
		0x2a, 0x20, 0x53, 0xfa, 0x2c, 0x39, 0xcc, 0xc6, 0x4e, 0xc7, 0xfd, 0x77, 0x92, 0xac, 0x03, 0x7a},
	// p-1 (order 2)
	{0xec, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	// p (= 0, order 4)
	{0xed, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	// p+1 (= 1, order 1)
	{0xee, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
}

func hasSmallOrder(pub [32]byte) bool {
	pub[31] &= 0x7f
	for _, e := range smallOrderEncodings {
		if pub == e {
			return true
		}
	}
	return false
}

var (
	ed25519P = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	// d = -121665 / 121666 mod p.
	ed25519D = func() *big.Int {
		d := new(big.Int).ModInverse(big.NewInt(121666), ed25519P)
		d.Mul(d, big.NewInt(-121665))
		return d.Mod(d, ed25519P)
	}()
	ed25519Half = new(big.Int).Rsh(new(big.Int).Sub(ed25519P, big.NewInt(1)), 1) // (p-1)/2
)

// onCurveCanonical decodes pub as RFC 8032 section 5.1.3 does, without
// recovering x: y must be below p, and x^2 = (y^2 - 1) / (d y^2 + 1) must be
// a square mod p (Euler's criterion). x = 0 with the sign bit set is the one
// other encoding the RFC refuses.
func onCurveCanonical(pub [32]byte) bool {
	sign := pub[31] >> 7
	pub[31] &= 0x7f
	be := make([]byte, 32)
	for i, b := range pub {
		be[31-i] = b
	}
	p := ed25519P
	y := new(big.Int).SetBytes(be)
	if y.Cmp(p) >= 0 {
		return false
	}
	yy := new(big.Int).Mul(y, y)
	yy.Mod(yy, p)
	u := new(big.Int).Sub(yy, big.NewInt(1))
	u.Mod(u, p)
	v := new(big.Int).Mul(ed25519D, yy)
	v.Add(v, big.NewInt(1))
	v.Mod(v, p)
	// v is never 0: d is not a square mod p, so d y^2 = -1 has no solution.
	xx := new(big.Int).ModInverse(v, p)
	if xx == nil {
		return false
	}
	xx.Mul(xx, u)
	xx.Mod(xx, p)
	if xx.Sign() == 0 {
		return sign == 0
	}
	return new(big.Int).Exp(xx, ed25519Half, p).Cmp(big.NewInt(1)) == 0
}
