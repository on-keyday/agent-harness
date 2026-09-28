package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCertPair writes a throwaway self-signed PEM pair and returns the paths.
func writeCertPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "harness-server-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestLoadListenTLS_NeitherFlagMeansPlaintext(t *testing.T) {
	cfg, err := loadListenTLS("", "", "127.0.0.1:8539")
	if err != nil || cfg != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", cfg, err)
	}
}

func TestLoadListenTLS_OneWithoutTheOtherIsAnError(t *testing.T) {
	certFile, keyFile := writeCertPair(t)
	for _, c := range []struct{ cert, key string }{{certFile, ""}, {"", keyFile}} {
		if _, err := loadListenTLS(c.cert, c.key, "127.0.0.1:8539"); err == nil || !strings.Contains(err.Error(), "together") {
			t.Errorf("cert=%q key=%q: err = %v, want a 'together' error", c.cert, c.key, err)
		}
	}
}

func TestLoadListenTLS_NeedsTheWSListener(t *testing.T) {
	certFile, keyFile := writeCertPair(t)
	if _, err := loadListenTLS(certFile, keyFile, ""); err == nil || !strings.Contains(err.Error(), "--listen") {
		t.Fatalf("err = %v, want a --listen error", err)
	}
}

func TestLoadListenTLS_UnreadablePairIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.pem")
	if _, err := loadListenTLS(missing, missing, "127.0.0.1:8539"); err == nil {
		t.Fatal("a missing certificate loaded")
	}
}

func TestLoadListenTLS_ValidPair(t *testing.T) {
	certFile, keyFile := writeCertPair(t)
	cfg, err := loadListenTLS(certFile, keyFile, "127.0.0.1:8539")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("certificates = %d, want 1", len(cfg.Certificates))
	}
}
