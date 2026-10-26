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
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// SelfSignedValidity is how long a self-signed certificate is valid. The
// panel pins its key, so neither its name nor its age matter to it.
const SelfSignedValidity = 10 * 365 * 24 * time.Hour

// CertReloader serves a certificate (own, the one of Wings or the
// self-signed one) and reloads it when it is renewed or replaced (cert or key
// file changed).
type CertReloader struct {
	CertFile, KeyFile string

	mu       sync.Mutex
	cert     *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	checked  time.Time
	interval time.Duration
}

// NewCertReloader loads the pair once and fails if it is unusable.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	c := &CertReloader{CertFile: certFile, KeyFile: keyFile, interval: 10 * time.Second}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *CertReloader) load() error {
	cs, err := os.Stat(c.CertFile)
	if err != nil {
		return err
	}
	ks, err := os.Stat(c.KeyFile)
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return err
	}
	c.cert, c.certMod, c.keyMod = &pair, cs.ModTime(), ks.ModTime()
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. On a failed reload
// (e.g. cert written but key not yet) the previous certificate stays in use.
func (c *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) >= c.interval {
		c.checked = time.Now()
		cs, err1 := os.Stat(c.CertFile)
		ks, err2 := os.Stat(c.KeyFile)
		if err1 == nil && err2 == nil && (!cs.ModTime().Equal(c.certMod) || !ks.ModTime().Equal(c.keyMod)) {
			old := c.cert
			if err := c.load(); err != nil {
				c.cert = old
			}
		}
	}
	return c.cert, nil
}

// PublicKeyPin returns the pin of the leaf certificate of c in the format of
// curl's --pinnedpubkey (CURLOPT_PINNEDPUBLICKEY), which the panel uses:
// "sha256//" and the base64 SHA-256 of its DER SubjectPublicKeyInfo.
func PublicKeyPin(c *tls.Certificate) (string, error) {
	if c == nil || len(c.Certificate) == 0 {
		return "", errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return "sha256//" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

// Pin returns the pin of the certificate served right now, see PublicKeyPin.
func (c *CertReloader) Pin() (string, error) {
	cert, err := c.GetCertificate(nil)
	if err != nil {
		return "", err
	}
	return PublicKeyPin(cert)
}

// EnsureSelfSigned creates a self-signed certificate (ECDSA P-256, valid for
// SelfSignedValidity, for the host name and localhost) at certFile and keyFile
// unless both exist; created tells whether it did. Existing files are reused
// as they are. Only one of them is an error: the bridge neither overwrites a
// file nor follows a link it did not create (O_EXCL), the admin removes it.
// The key is written 0600, the certificate 0644.
func EnsureSelfSigned(certFile, keyFile string) (created bool, err error) {
	_, certErr := os.Lstat(certFile)
	_, keyErr := os.Lstat(keyFile)
	switch {
	case certErr == nil && keyErr == nil:
		return false, nil
	case certErr == nil || keyErr == nil:
		have, missing := certFile, keyFile
		if keyErr == nil {
			have, missing = keyFile, certFile
		}
		return false, fmt.Errorf("%s exists but %s does not: remove %s to create a new self-signed certificate", have, missing, have)
	case !errors.Is(certErr, os.ErrNotExist):
		return false, certErr
	case !errors.Is(keyErr, os.ErrNotExist):
		return false, keyErr
	}
	certPEM, keyPEM, err := selfSigned(time.Now())
	if err != nil {
		return false, err
	}
	// The key first: a certificate without its key is useless, the other way
	// round it is removed again.
	if err := writeNew(keyFile, keyPEM, 0o600); err != nil {
		return false, err
	}
	if err := writeNew(certFile, certPEM, 0o644); err != nil {
		os.Remove(keyFile)
		return false, err
	}
	return true, nil
}

// selfSigned returns a new self-signed certificate and its key as PEM.
func selfSigned(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	names := []string{"localhost"}
	if h, err := os.Hostname(); err == nil && validHostname(h) && !strings.EqualFold(h, "localhost") {
		names = []string{h, "localhost"}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"MPA Bridge"}},
		DNSNames:              names,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:             now.Add(-time.Hour), // a node clock slightly behind
		NotAfter:              now.Add(SelfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// validHostname reports whether h can be a DNS name of the certificate.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// writeNew writes a file that must not exist yet; O_EXCL also refuses a
// symlink in its place. perm is set explicitly, whatever the umask.
func writeNew(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}
