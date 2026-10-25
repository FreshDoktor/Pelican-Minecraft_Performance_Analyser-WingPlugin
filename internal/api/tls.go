package api

import (
	"crypto/tls"
	"os"
	"sync"
	"time"
)

// CertReloader serves the Wings certificate and reloads it when certbot
// renews it (cert or key file changed).
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
