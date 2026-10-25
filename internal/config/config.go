// Package config reads the Wings configuration (token, TLS, tmpfs size) and
// the bridge's own optional mpa-bridge.yml.
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
)

// Bridge is mpa-bridge.yml. Every field is optional.
type Bridge struct {
	Host                  string   `yaml:"host"`
	Port                  int      `yaml:"port"`
	MaxConcurrentProfiles int      `yaml:"max_concurrent_profiles"`
	MaxProfileSeconds     int      `yaml:"max_profile_seconds"`
	AllowedIPs            []string `yaml:"allowed_ips"`
	WingsConfig           string   `yaml:"wings_config"`
	DockerSocket          string   `yaml:"docker_socket"`
	ProcRoot              string   `yaml:"proc_root"`
}

// Wings holds the fields of /etc/pelican/config.yml the bridge uses.
type Wings struct {
	TokenID string `yaml:"token_id"`
	Token   string `yaml:"token"`
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
	// Allowed is the parsed allowed_ips; empty means every address.
	Allowed []netip.Prefix
}

// Load reads mpa-bridge.yml from bridgePath (a missing file means defaults)
// and the Wings config it points to.
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
}

func (c *Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}
	if c.MaxConcurrentProfiles < 1 {
		return errors.New("max_concurrent_profiles must be at least 1")
	}
	if c.MaxProfileSeconds < 1 || c.MaxProfileSeconds > 3600 {
		return errors.New("max_profile_seconds must be between 1 and 3600")
	}
	c.Allowed = nil
	for _, s := range c.AllowedIPs {
		p, err := ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("allowed_ips: %w", err)
		}
		c.Allowed = append(c.Allowed, p)
	}
	return nil
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

// LoadWings reads the Wings config and resolves the token like Wings does:
// WINGS_TOKEN_ID / WINGS_TOKEN take precedence over the file, then Expand.
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
	if v := os.Getenv("WINGS_TOKEN"); v != "" {
		w.Token = v
	}
	w.TokenID = Expand(w.TokenID)
	w.Token = Expand(w.Token)
	if w.Token == "" {
		return nil, fmt.Errorf("%s: no token (token, WINGS_TOKEN)", path)
	}
	return &w, nil
}

// Expand mirrors config.Expand of Wings: environment variables first, then a
// "file://<path>" value is replaced by the file's content (trailing CR/LF
// removed). That allows file://${CREDENTIALS_DIRECTORY}/token with systemd's
// LoadCredential. Like Wings, an unreadable file yields ""; LoadWings then
// refuses an empty token.
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

// Credentials returns the current token; it re-reads the Wings config when the
// file changed (the panel can rotate the token while Wings runs).
type Credentials struct {
	path     string
	interval time.Duration

	mu      sync.Mutex
	id      string
	token   string
	modTime time.Time
	checked time.Time
}

// NewCredentials starts with the token of c.
func NewCredentials(c *Config) *Credentials {
	cr := &Credentials{path: c.WingsConfig, interval: 10 * time.Second, id: c.TokenID, token: c.Token, checked: time.Now()}
	if st, err := os.Stat(c.WingsConfig); err == nil {
		cr.modTime = st.ModTime()
	}
	return cr
}

// StaticCredentials never reloads (tests).
func StaticCredentials(id, token string) *Credentials {
	return &Credentials{id: id, token: token, interval: -1}
}

// Get returns token id and token.
func (c *Credentials) Get() (id, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.interval > 0 && time.Since(c.checked) >= c.interval {
		c.checked = time.Now()
		if st, err := os.Stat(c.path); err == nil && !st.ModTime().Equal(c.modTime) {
			if w, err := LoadWings(c.path); err == nil {
				c.id, c.token, c.modTime = w.TokenID, w.Token, st.ModTime()
			}
		}
	}
	return c.id, c.token
}
