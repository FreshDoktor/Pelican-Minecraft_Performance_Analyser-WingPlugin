package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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

func TestEnsureSelfSigned(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "mpa-bridge.crt"), filepath.Join(dir, "mpa-bridge.key")
	created, err := EnsureSelfSigned(cert, key)
	if err != nil || !created {
		t.Fatalf("first start: %v %v", created, err)
	}
	cr, err := NewCertReloader(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := cr.GetCertificate(nil)
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if pub, ok := x.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("key: %T", x.PublicKey)
	}
	if x.NotAfter.Before(time.Now().AddDate(9, 0, 0)) || x.NotBefore.After(time.Now()) {
		t.Errorf("validity: %v to %v", x.NotBefore, x.NotAfter)
	}
	if err := x.VerifyHostname("localhost"); err != nil {
		t.Error(err)
	}
	if h, err := os.Hostname(); err == nil && validHostname(h) {
		if err := x.VerifyHostname(h); err != nil {
			t.Error(err)
		}
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{key: 0o600, cert: 0o644} {
			if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
				t.Errorf("%s: %v %v, want %v", p, st.Mode().Perm(), err, want)
			}
		}
	}

	// Later starts reuse the pair as it is.
	before, _ := os.ReadFile(key)
	created, err = EnsureSelfSigned(cert, key)
	after, _ := os.ReadFile(key)
	if err != nil || created || string(before) != string(after) {
		t.Errorf("second start must reuse: %v %v", created, err)
	}

	// Only one of the two: neither is touched.
	os.Remove(cert)
	if _, err := EnsureSelfSigned(cert, key); err == nil {
		t.Error("a key without certificate must fail")
	}
	if b, _ := os.ReadFile(key); string(b) != string(before) {
		t.Error("existing key overwritten")
	}
	if _, err := os.Stat(cert); err == nil {
		t.Error("certificate written next to a foreign key")
	}
	os.Remove(key)
	os.WriteFile(cert, []byte("foreign"), 0o644)
	if _, err := EnsureSelfSigned(cert, key); err == nil {
		t.Error("a certificate without key must fail")
	}
	if b, _ := os.ReadFile(cert); string(b) != "foreign" {
		t.Error("existing certificate overwritten")
	}
	if _, err := os.Stat(key); err == nil {
		t.Error("key written next to a foreign certificate")
	}
}

func TestWriteNewRefusesExistingAndLinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mpa-bridge.key")
	os.WriteFile(p, []byte("planted"), 0o644)
	if err := writeNew(p, []byte("key"), 0o600); err == nil {
		t.Error("existing file must not be overwritten")
	}
	if b, _ := os.ReadFile(p); string(b) != "planted" {
		t.Errorf("overwritten: %q", b)
	}

	// A planted link, dangling or not, is not followed (symlinks need privileges on Windows).
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if err := writeNew(link, []byte("key"), 0o600); err == nil {
		t.Error("a dangling link must not be followed")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("link target written")
	}
	if _, err := EnsureSelfSigned(filepath.Join(dir, "link.crt"), link); err == nil {
		t.Error("a planted link in place of the key must fail")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("link target written")
	}
}

func TestPublicKeyPin(t *testing.T) {
	dir := t.TempDir()
	cert, key := writePair(t, dir, "pin", time.Now())
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := PublicKeyPin(&pair)
	if err != nil {
		t.Fatal(err)
	}

	// Independently: decode the PEM file and marshal the public key again.
	b, _ := os.ReadFile(cert)
	block, _ := pem.Decode(b)
	x, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(x.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	if want := "sha256//" + base64.StdEncoding.EncodeToString(sum[:]); pin != want {
		t.Errorf("pin %q, want %q", pin, want)
	}
	if !regexp.MustCompile(`^sha256//[A-Za-z0-9+/]{43}=$`).MatchString(pin) {
		t.Errorf("pin format: %q (the panel checks it with this pattern)", pin)
	}

	cr, err := NewCertReloader(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cr.Pin(); err != nil || got != pin {
		t.Errorf("reloader pin %q %v", got, err)
	}
	if _, err := PublicKeyPin(&tls.Certificate{}); err == nil {
		t.Error("empty certificate must fail")
	}
}
