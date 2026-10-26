// Package config reads the bridge's mpa-bridge.yml (with its own token and
// TLS settings) and the Wings configuration (token id, TLS, tmpfs size).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Default paths.
const (
	DefaultBridgePath = "/etc/pelican/mpa-bridge.yml"
	DefaultWingsPath  = "/etc/pelican/config.yml"
	// The self-signed certificate the bridge creates on its first start, in
	// the StateDirectory of the unit (/etc is read-only to the service).
	DefaultSelfSignedCert = "/var/lib/mpa-bridge/tls.crt"
	DefaultSelfSignedKey  = "/var/lib/mpa-bridge/tls.key"
)

// Sources of the certificate the bridge serves, see CertFiles.
const (
	CertOwn        = "own"         // tls.cert and tls.key
	CertWings      = "wings"       // api.ssl of Wings
	CertSelfSigned = "self-signed" // created by the bridge
)

// MinTokenLength is the minimum length of the bridge token.
const MinTokenLength = 32

// Bridge is mpa-bridge.yml. Every field but token is optional.
type Bridge struct {
	// Token is the bearer token the panel sends, independent of the Wings token.
	Token                 string   `yaml:"token"`
	Host                  string   `yaml:"host"`
	Port                  int      `yaml:"port"`
	MaxConcurrentProfiles int      `yaml:"max_concurrent_profiles"`
	MaxProfileSeconds     int      `yaml:"max_profile_seconds"`
	MaxTickMonitors       int      `yaml:"max_tick_monitors"`
	AllowedIPs            []string `yaml:"allowed_ips"`
	// AllowAll lets every address connect, instead of AllowedIPs (required otherwise).
	AllowAll     bool   `yaml:"allow_all"`
	WingsConfig  string `yaml:"wings_config"`
	DockerSocket string `yaml:"docker_socket"`
	ProcRoot     string `yaml:"proc_root"`
	TLS          TLS    `yaml:"tls"`
}

// TLS is the tls section of mpa-bridge.yml. The bridge serves HTTPS unless
// Disable is set, see CertFiles.
type TLS struct {
	// Cert and Key are an own certificate (PEM, the chain in Cert).
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
	// Disable serves plain HTTP, e.g. behind a proxy on the node that
	// terminates TLS. The token then crosses the network in cleartext.
	Disable bool `yaml:"disable"`
	// SelfSignedCert and SelfSignedKey are where the self-signed certificate
	// is kept (default /var/lib/mpa-bridge/tls.crt and tls.key).
	SelfSignedCert string `yaml:"self_signed_cert"`
	SelfSignedKey  string `yaml:"self_signed_key"`
}

// Wings holds the fields of /etc/pelican/config.yml the bridge uses. The Wings
// token is deliberately not read: the bridge has its own.
type Wings struct {
	TokenID string `yaml:"token_id"`
	API     struct {
		SSL struct {
			Enabled bool   `yaml:"enabled"`
			Cert    string `yaml:"cert"`
			Key     string `yaml:"key"`
		} `yaml:"ssl"`
	} `yaml:"api"`
	Docker struct {
		TmpfsSize *uint `yaml:"tmpfs_size"`
	} `yaml:"docker"`
}

// Config is the merged configuration.
type Config struct {
	Bridge
	Wings
	// TmpfsSizeBytes is docker.tmpfs_size (MB, default 100) in bytes.
	TmpfsSizeBytes int64
	// Allowed is the parsed allowed_ips; empty only with allow_all.
	Allowed []netip.Prefix
}

// Load reads mpa-bridge.yml from bridgePath (a missing file means defaults,
// then the token must come from MPA_BRIDGE_TOKEN) and the Wings config it
// points to. The token is resolved like Wings resolves its own: MPA_BRIDGE_TOKEN
// takes precedence over the file, then Expand.
func Load(bridgePath string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(bridgePath)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, &c.Bridge); err != nil {
			return nil, fmt.Errorf("%s: %w", bridgePath, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	if v := os.Getenv("MPA_BRIDGE_TOKEN"); v != "" {
		c.Token = v
	}
	c.Token = Expand(c.Token)
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", bridgePath, err)
	}
	w, err := LoadWings(c.WingsConfig)
	if err != nil {
		return nil, err
	}
	c.setWings(w)
	return c, nil
}

func (c *Config) setWings(w *Wings) {
	c.Wings = *w
	size := uint(100)
	if w.Docker.TmpfsSize != nil {
		size = *w.Docker.TmpfsSize
	}
	c.TmpfsSizeBytes = int64(size) << 20
}

func (c *Config) applyDefaults() {
	if c.Host == "" {
		c.Host = "0.0.0.0"
	}
	if c.Port == 0 {
		c.Port = 8090
	}
	if c.MaxConcurrentProfiles == 0 {
		c.MaxConcurrentProfiles = 2
	}
	if c.MaxProfileSeconds == 0 {
		c.MaxProfileSeconds = 120
	}
	if c.MaxTickMonitors == 0 {
		c.MaxTickMonitors = 10
	}
	if c.WingsConfig == "" {
		c.WingsConfig = DefaultWingsPath
	}
	if c.DockerSocket == "" {
		c.DockerSocket = "/var/run/docker.sock"
		if h, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok && h != "" {
			c.DockerSocket = h
		}
	}
	if c.ProcRoot == "" {
		c.ProcRoot = "/proc"
	}
	if c.TLS.SelfSignedCert == "" {
		c.TLS.SelfSignedCert = DefaultSelfSignedCert
	}
	if c.TLS.SelfSignedKey == "" {
		c.TLS.SelfSignedKey = DefaultSelfSignedKey
	}
}

func (c *Config) validate() error {
	if c.Token == "" {
		return errors.New("no token (token, MPA_BRIDGE_TOKEN): run install.sh again, it adds one, or set token to a random value of at least 32 characters")
	}
	if len(c.Token) < MinTokenLength {
		return fmt.Errorf("token is shorter than %d characters: run install.sh again with token removed, or set a longer random value", MinTokenLength)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}
	if c.MaxConcurrentProfiles < 1 {
		return errors.New("max_concurrent_profiles must be at least 1")
	}
	if c.MaxProfileSeconds < 1 || c.MaxProfileSeconds > 3600 {
		return errors.New("max_profile_seconds must be between 1 and 3600")
	}
	if c.MaxTickMonitors < 1 {
		return errors.New("max_tick_monitors must be at least 1")
	}
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		return errors.New("tls: cert and key go together, set both or neither")
	}
	if c.TLS.Disable && c.TLS.Cert != "" {
		return errors.New("tls: disable and cert/key exclude each other")
	}
	c.Allowed = nil
	for _, s := range c.AllowedIPs {
		p, err := ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("allowed_ips: %w", err)
		}
		c.Allowed = append(c.Allowed, p)
	}
	// Without allowed_ips every address on the internet could try tokens and
	// learn the version and token_id, so open access must be asked for.
	switch {
	case c.AllowAll && len(c.Allowed) > 0:
		return errors.New("allow_all and allowed_ips exclude each other: remove allow_all to only let allowed_ips connect")
	case !c.AllowAll && len(c.Allowed) == 0:
		return errors.New("allowed_ips is empty: set it to the IP(s) the panel connects from (IPs or CIDRs), " +
			"or set allow_all: true to let every address connect (only the token protects the bridge then)")
	}
	return nil
}

// CertFiles returns the certificate and key the bridge serves and their
// source: tls.cert and tls.key, else the Wings certificate if Wings serves
// HTTPS (api.ssl.enabled), else the self-signed pair, which the bridge creates
// on its first start. ok is false with tls.disable: plain HTTP.
func (c *Config) CertFiles() (source, cert, key string, ok bool) {
	switch {
	case c.TLS.Disable:
		return "", "", "", false
	case c.TLS.Cert != "":
		return CertOwn, c.TLS.Cert, c.TLS.Key, true
	case c.API.SSL.Enabled:
		return CertWings, c.API.SSL.Cert, c.API.SSL.Key, true
	default:
		return CertSelfSigned, c.TLS.SelfSignedCert, c.TLS.SelfSignedKey, true
	}
}

// ParsePrefix parses an IP ("10.0.0.5") or CIDR ("10.0.0.0/24").
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// LoadWings reads the Wings config and resolves the token id like Wings does:
// WINGS_TOKEN_ID takes precedence over the file, then Expand.
func LoadWings(path string) (*Wings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var w Wings
	if err := yaml.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if v := os.Getenv("WINGS_TOKEN_ID"); v != "" {
		w.TokenID = v
	}
	w.TokenID = Expand(w.TokenID)
	return &w, nil
}

// Expand mirrors config.Expand of Wings: environment variables first, then a
// "file://<path>" value is replaced by the file's content (trailing CR/LF
// removed). That allows file://${CREDENTIALS_DIRECTORY}/token with systemd's
// LoadCredential. Like Wings, an unreadable file yields ""; Load then refuses
// the empty token.
func Expand(v string) string {
	v = os.ExpandEnv(v)
	if p, ok := strings.CutPrefix(v, "file://"); ok {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		// Wings trims "\r" and then "\n", which keeps the CR of a CRLF file; trim both.
		v = string(bytes.TrimRight(b, "\r\n"))
	}
	return v
}

// Credentials returns the current bridge token and Wings token id; it reloads
// both when mpa-bridge.yml or the Wings config changed (a new token in
// mpa-bridge.yml, or Wings re-configured by the panel). An invalid edit keeps
// the previous values.
type Credentials struct {
	bridgePath string
	wingsPath  string
	interval   time.Duration

	mu         sync.Mutex
	id         string
	token      string
	bridgeTime time.Time
	wingsTime  time.Time
	checked    time.Time
}

// NewCredentials starts with the token and token id of c, read from bridgePath.
func NewCredentials(bridgePath string, c *Config) *Credentials {
	cr := &Credentials{bridgePath: bridgePath, wingsPath: c.WingsConfig, interval: 10 * time.Second,
		id: c.TokenID, token: c.Token, checked: time.Now()}
	cr.bridgeTime, cr.wingsTime = modTime(bridgePath), modTime(c.WingsConfig)
	return cr
}

// StaticCredentials never reloads (tests).
func StaticCredentials(id, token string) *Credentials {
	return &Credentials{id: id, token: token, interval: -1}
}

func modTime(path string) time.Time {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// Get returns token id and token.
func (c *Credentials) Get() (id, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.interval > 0 && time.Since(c.checked) >= c.interval {
		c.checked = time.Now()
		bt, wt := modTime(c.bridgePath), modTime(c.wingsPath)
		if !bt.Equal(c.bridgeTime) || !wt.Equal(c.wingsTime) {
			if cfg, err := Load(c.bridgePath); err == nil {
				c.id, c.token, c.wingsPath = cfg.TokenID, cfg.Token, cfg.WingsConfig
				c.bridgeTime, c.wingsTime = bt, wt
			}
		}
	}
	return c.id, c.token
}
