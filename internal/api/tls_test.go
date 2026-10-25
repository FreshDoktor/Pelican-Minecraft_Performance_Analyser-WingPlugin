package api

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
	"testing"
	"time"
)

func writePair(t *testing.T, dir, cn string, mod time.Time) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	cert, keyFile := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	os.Chtimes(cert, mod, mod)
	os.Chtimes(keyFile, mod, mod)
	return cert, keyFile
}

func TestCertReloader(t *testing.T) {
	dir := t.TempDir()
	cert, key := writePair(t, dir, "first", time.Now().Add(-time.Minute))
	cr, err := NewCertReloader(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	cr.interval = 0
	cn := func() string {
		c, _ := cr.GetCertificate(nil)
		x, _ := x509.ParseCertificate(c.Certificate[0])
		return x.Subject.CommonName
	}
	if cn() != "first" {
		t.Fatal("initial cert")
	}
	writePair(t, dir, "second", time.Now())
	if cn() != "second" {
		t.Error("renewed cert not loaded")
	}
	// A broken renewal keeps the old certificate.
	os.WriteFile(key, []byte("garbage"), 0o600)
	later := time.Now().Add(time.Minute)
	os.Chtimes(key, later, later)
	if cn() != "second" {
		t.Error("broken renewal must keep the old cert")
	}
	if _, err := NewCertReloader(filepath.Join(dir, "missing"), key); err == nil {
		t.Error("missing cert must fail")
	}
}
