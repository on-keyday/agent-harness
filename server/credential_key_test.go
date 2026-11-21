package server

import (
	"crypto/ed25519"
	"math/big"
	"testing"
)

// The blocklist in credential_key.go is recited from libsodium, not derived,
// so this file derives it: each entry must decode to a point whose order
// divides 8, and the entries must name all eight such points. The arithmetic
// here is a second, independent implementation (affine twisted Edwards with
// a square root), so it is not the code under test checking itself.

var (
	edP = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	edD = func() *big.Int {
		d := new(big.Int).ModInverse(big.NewInt(121666), edP)
		d.Mul(d, big.NewInt(-121665))
		return d.Mod(d, edP)
	}()
	// sqrt(-1) = 2^((p-1)/4)
	edSqrtM1 = new(big.Int).Exp(big.NewInt(2), new(big.Int).Rsh(new(big.Int).Sub(edP, big.NewInt(1)), 2), edP)
)

type edPoint struct{ x, y *big.Int }

func edMod(v *big.Int) *big.Int { return v.Mod(v, edP) }

// edSqrt is a square root mod p (p = 5 mod 8), or nil.
func edSqrt(w *big.Int) *big.Int {
	e := new(big.Int).Rsh(new(big.Int).Add(edP, big.NewInt(3)), 3)
	r := new(big.Int).Exp(w, e, edP)
	sq := edMod(new(big.Int).Mul(r, r))
	switch {
	case sq.Cmp(edMod(new(big.Int).Set(w))) == 0:
		return r
	case sq.Cmp(edMod(new(big.Int).Neg(w))) == 0:
		return edMod(r.Mul(r, edSqrtM1))
	}
	return nil
}

// edDecode decodes an encoding the way Go's verifier does (non-canonical y
// reduced, x = 0 with the sign bit taken as x = 0); ok=false off the curve.
func edDecode(enc [32]byte) (edPoint, bool) {
	le := enc
	sign := le[31] >> 7
	le[31] &= 0x7f
	be := make([]byte, 32)
	for i := range le {
		be[31-i] = le[i]
	}
	y := edMod(new(big.Int).SetBytes(be))
	yy := edMod(new(big.Int).Mul(y, y))
	u := edMod(new(big.Int).Sub(yy, big.NewInt(1)))
	v := edMod(new(big.Int).Add(new(big.Int).Mul(edD, yy), big.NewInt(1)))
	w := edMod(new(big.Int).Mul(u, new(big.Int).ModInverse(v, edP)))
	x := edSqrt(w)
	if x == nil {
		return edPoint{}, false
	}
	if x.Bit(0) != uint(sign) {
		x = edMod(new(big.Int).Neg(x))
	}
	return edPoint{x, y}, true
}

func edAdd(a, b edPoint) edPoint {
	x1y2 := new(big.Int).Mul(a.x, b.y)
	y1x2 := new(big.Int).Mul(a.y, b.x)
	y1y2 := new(big.Int).Mul(a.y, b.y)
	x1x2 := new(big.Int).Mul(a.x, b.x)
	t := edMod(new(big.Int).Mul(edD, new(big.Int).Mul(x1x2, y1y2)))
	xn := edMod(new(big.Int).Add(x1y2, y1x2))
	yn := edMod(new(big.Int).Add(y1y2, x1x2))
	xd := edMod(new(big.Int).Add(big.NewInt(1), t))
	yd := edMod(new(big.Int).Sub(big.NewInt(1), t))
	return edPoint{
		edMod(xn.Mul(xn, new(big.Int).ModInverse(xd, edP))),
		edMod(yn.Mul(yn, new(big.Int).ModInverse(yd, edP))),
	}
}

func edIsIdentity(p edPoint) bool { return p.x.Sign() == 0 && p.y.Cmp(big.NewInt(1)) == 0 }

// edOrder is the order of p when it divides 8, else 0.
func edOrder(p edPoint) int {
	q := p
	for n := 1; n <= 8; n++ {
		if edIsIdentity(q) {
			return n
		}
		q = edAdd(q, p)
	}
	return 0
}

func withSign(e [32]byte, sign byte) [32]byte {
	e[31] = e[31]&0x7f | sign<<7
	return e
}

func TestSmallOrderBlocklistIsEveryPointOfOrderDividingEight(t *testing.T) {
	points := map[string]bool{}
	for i, e := range smallOrderEncodings {
		if e[31]&0x80 != 0 {
			t.Fatalf("entry %d has its sign bit set: entries are stored sign-masked", i)
		}
		for _, sign := range []byte{0, 1} {
			p, ok := edDecode(withSign(e, sign))
			if !ok {
				t.Fatalf("entry %d sign %d does not decode to a point", i, sign)
			}
			n := edOrder(p)
			if n == 0 || 8%n != 0 {
				t.Fatalf("entry %d sign %d has order %d, not a divisor of 8", i, sign, n)
			}
			points[p.x.String()+","+p.y.String()] = true
		}
	}
	if len(points) != 8 {
		t.Fatalf("the blocklist names %d distinct small-order points, want all 8", len(points))
	}
}

func TestCredentialKeyUsableRefusesEverySmallOrderEncoding(t *testing.T) {
	for i, e := range smallOrderEncodings {
		for _, sign := range []byte{0, 1} {
			if credentialKeyUsable(withSign(e, sign)) {
				t.Errorf("entry %d sign %d (small order) is usable", i, sign)
			}
		}
	}
}

func TestCredentialKeyUsableAcceptsGeneratedKeys(t *testing.T) {
	for i := 0; i < 64; i++ {
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		var k [32]byte
		copy(k[:], pub)
		if !credentialKeyUsable(k) {
			t.Fatalf("a generated key %x is refused", k)
		}
	}
}

// Off the curve: credentialKeyUsable agrees with the independent decoder for
// small y, and some of them do not decode at all.
func TestCredentialKeyUsableRefusesPointsOffTheCurve(t *testing.T) {
	off := 0
	for y := 2; y < 64; y++ {
		var e [32]byte
		e[0] = byte(y)
		_, onCurve := edDecode(e)
		if !onCurve {
			off++
		}
		if got := credentialKeyUsable(e); got != onCurve {
			t.Errorf("y=%d: usable=%v, on the curve=%v", y, got, onCurve)
		}
	}
	if off == 0 {
		t.Fatal("no small y was off the curve: the case is not exercised")
	}
}

// A non-canonical y (>= p) is refused even where it names a large-order
// point: no key a client generates is encoded that way.
func TestCredentialKeyUsableRefusesNonCanonicalY(t *testing.T) {
	for _, low := range []byte{0xef, 0xf0, 0xff} { // p+2, p+3, 2^255-1
		e := [32]byte{low}
		for i := 1; i < 31; i++ {
			e[i] = 0xff
		}
		e[31] = 0x7f
		if credentialKeyUsable(e) {
			t.Errorf("non-canonical y %x is usable", e)
		}
	}
}
