package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const wingsYAML = `debug: false
uuid: 4b9c1e0a-0000-4000-8000-000000000000
token_id: Xy12Ab34Cd56Ef78
token: %s
api:
  host: 0.0.0.0
  port: 8080
  ssl:
    enabled: false
    cert: /etc/letsencrypt/live/wing.example.com/fullchain.pem
    key: /etc/letsencrypt/live/wing.example.com/privkey.pem
system:
  user:
    uid: 999
    gid: 988
docker:
  tmpfs_size: 150
`

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func wings(t *testing.T, token string) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "config.yml", fmt.Sprintf(wingsYAML, token))
}

func TestExpand(t *testing.T) {
	dir := t.TempDir()
	tokenFile := writeFile(t, dir, "token", "s3cret-from-file\r\n")
	t.Setenv("MPA_TEST_TOKEN", "s3cret-from-env")
	t.Setenv("MPA_TEST_DIR", dir)
	cases := map[string]string{
		"plain":                          "plain",
		"${MPA_TEST_TOKEN}":              "s3cret-from-env",
		"$MPA_TEST_TOKEN":                "s3cret-from-env",
		"file://" + tokenFile:            "s3cret-from-file",
		"file://${MPA_TEST_DIR}/token":   "s3cret-from-file",
		"file://" + dir + "/missing":     "",
		"${MPA_TEST_UNSET_VARIABLE_XYZ}": "",
	}
	for in, want := range cases {
		if got := Expand(in); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}

const testToken = "0123456789abcdef0123456789abcdef"

func TestLoadWings(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	w, err := LoadWings(wings(t, "abc123"))
	if err != nil {
		t.Fatal(err)
	}
	if w.TokenID != "Xy12Ab34Cd56Ef78" || w.API.SSL.Enabled ||
		w.API.SSL.Cert != "/etc/letsencrypt/live/wing.example.com/fullchain.pem" || *w.Docker.TmpfsSize != 150 {
		t.Errorf("unexpected %+v", w)
	}

	t.Setenv("WINGS_TOKEN_ID", "id-env")
	w, err = LoadWings(wings(t, "abc123"))
	if err != nil || w.TokenID != "id-env" {
		t.Errorf("env must win: %+v %v", w, err)
	}

	// The Wings token is not needed (and not read) any more.
	t.Setenv("WINGS_TOKEN_ID", "")
	if _, err := LoadWings(wings(t, `""`)); err != nil {
		t.Errorf("empty Wings token: %v", err)
	}
}

func TestLoadToken(t *testing.T) {
	t.Setenv("MPA_BRIDGE_TOKEN", "")
	wp := wings(t, testToken)
	dir := t.TempDir()
	load := func(token string) (*Config, error) {
		t.Helper()
		return Load(writeFile(t, dir, "mpa-bridge.yml", "wings_config: "+wp+"\ntoken: "+token+"\n"))
	}

	c, err := load(testToken)
	if err != nil || c.Token != testToken {
		t.Fatalf("token: %+v %v", c, err)
	}
	// The Wings token is never used, even when it is the same value.
	if _, err := Load(writeFile(t, dir, "mpa-bridge.yml", "wings_config: "+wp+"\n")); err == nil {
		t.Error("no token must fail")
	}
	for _, bad := range []string{`""`, "too-short", "file://" + dir + "/missing"} {
		if _, err := load(bad); err == nil {
			t.Errorf("token %q must fail", bad)
		}
	}

	writeFile(t, dir, "tok", testToken+"\n")
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	c, err = load("file://${CREDENTIALS_DIRECTORY}/tok")
	if err != nil || c.Token != testToken {
		t.Errorf("file token: %+v %v", c, err)
	}

	env := strings.Repeat("e", MinTokenLength)
	t.Setenv("MPA_BRIDGE_TOKEN", env)
	c, err = load(testToken)
	if err != nil || c.Token != env {
		t.Errorf("env must win: %+v %v", c, err)
	}
	c, err = Load(filepath.Join(dir, "missing.yml"))
	if err == nil || !strings.Contains(err.Error(), "config.yml") {
		t.Errorf("missing bridge file with MPA_BRIDGE_TOKEN: defaults, then the Wings config at /etc/pelican: %v", err)
	}
}

func TestLoadDefaultsAndBridgeFile(t *testing.T) {
	t.Setenv("MPA_BRIDGE_TOKEN", "")
	t.Setenv("DOCKER_HOST", "")
	wp := wings(t, "abc")
	dir := t.TempDir()
	base := "wings_config: " + wp + "\ntoken: " + testToken + "\n"

	bp := writeFile(t, dir, "mpa-bridge.yml", base)
	c, err := Load(bp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8090 || c.Host != "0.0.0.0" || c.MaxConcurrentProfiles != 2 || c.MaxProfileSeconds != 120 ||
		c.DockerSocket != "/var/run/docker.sock" || c.ProcRoot != "/proc" || len(c.Allowed) != 0 {
		t.Errorf("defaults: %+v", c.Bridge)
	}
	if c.TmpfsSizeBytes != 150<<20 || c.TokenID != "Xy12Ab34Cd56Ef78" {
		t.Errorf("wings: %d %q", c.TmpfsSizeBytes, c.TokenID)
	}

	bp = writeFile(t, dir, "mpa-bridge.yml", base+"port: 9000\nallowed_ips: [10.0.0.5, 192.168.0.0/24, '::ffff:10.0.0.6']\nmax_profile_seconds: 60\n")
	c, err = Load(bp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 || c.MaxProfileSeconds != 60 || len(c.Allowed) != 3 {
		t.Errorf("bridge: %+v", c.Bridge)
	}
	if c.Allowed[0].String() != "10.0.0.5/32" || c.Allowed[1].String() != "192.168.0.0/24" || c.Allowed[2].String() != "10.0.0.6/32" {
		t.Errorf("allowed: %v", c.Allowed)
	}

	for _, bad := range []string{"port: 70000", "allowed_ips: [nope]", "max_profile_seconds: -1", "max_concurrent_profiles: -2"} {
		bp = writeFile(t, dir, "mpa-bridge.yml", base+bad+"\n")
		if _, err := Load(bp); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.yml")); err == nil {
		t.Error("missing bridge file has no token")
	}
}

func TestCertFiles(t *testing.T) {
	t.Setenv("MPA_BRIDGE_TOKEN", "")
	dir := t.TempDir()
	plain := wings(t, "abc")
	ssl := writeFile(t, t.TempDir(), "config.yml", strings.Replace(fmt.Sprintf(wingsYAML, "abc"), "enabled: false", "enabled: true", 1))
	load := func(wp, tlsYAML string) (*Config, error) {
		t.Helper()
		return Load(writeFile(t, dir, "mpa-bridge.yml", "wings_config: "+wp+"\ntoken: "+testToken+"\n"+tlsYAML))
	}
	const own = "tls:\n  cert: /etc/ssl/bridge.pem\n  key: /etc/ssl/bridge.key\n"
	const wingsCert, wingsKey = "/etc/letsencrypt/live/wing.example.com/fullchain.pem", "/etc/letsencrypt/live/wing.example.com/privkey.pem"
	cases := []struct {
		name, wings, tls  string
		source, cert, key string
		ok                bool
	}{
		{"own before wings", ssl, own, CertOwn, "/etc/ssl/bridge.pem", "/etc/ssl/bridge.key", true},
		{"own without wings", plain, own, CertOwn, "/etc/ssl/bridge.pem", "/etc/ssl/bridge.key", true},
		{"wings", ssl, "", CertWings, wingsCert, wingsKey, true},
		{"self-signed", plain, "", CertSelfSigned, DefaultSelfSignedCert, DefaultSelfSignedKey, true},
		{"self-signed elsewhere", plain, "tls:\n  self_signed_cert: /srv/b.crt\n  self_signed_key: /srv/b.key\n", CertSelfSigned, "/srv/b.crt", "/srv/b.key", true},
		{"disabled", plain, "tls:\n  disable: true\n", "", "", "", false},
		{"disabled despite wings", ssl, "tls:\n  disable: true\n", "", "", "", false},
	}
	for _, tc := range cases {
		c, err := load(tc.wings, tc.tls)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		source, cert, key, ok := c.CertFiles()
		if source != tc.source || cert != tc.cert || key != tc.key || ok != tc.ok {
			t.Errorf("%s: %q %q %q %v", tc.name, source, cert, key, ok)
		}
	}

	for _, bad := range []string{
		"tls:\n  cert: /etc/ssl/bridge.pem\n",
		"tls:\n  key: /etc/ssl/bridge.key\n",
		"tls:\n  disable: true\n  cert: /etc/ssl/bridge.pem\n  key: /etc/ssl/bridge.key\n",
		"tls:\n  disable: true\n  cert: /etc/ssl/bridge.pem\n",
		"tls:\n  disable: true\n  key: /etc/ssl/bridge.key\n",
	} {
		if _, err := load(plain, bad); err == nil || !strings.Contains(err.Error(), "tls") {
			t.Errorf("%q must fail: %v", bad, err)
		}
	}
}

func TestCredentialsReload(t *testing.T) {
	t.Setenv("MPA_BRIDGE_TOKEN", "")
	t.Setenv("WINGS_TOKEN_ID", "")
	wp := wings(t, "abc")
	dir := t.TempDir()
	bp := writeFile(t, dir, "mpa-bridge.yml", "wings_config: "+wp+"\ntoken: "+testToken+"\n")
	c, err := Load(bp)
	if err != nil {
		t.Fatal(err)
	}
	cr := NewCredentials(bp, c)
	cr.interval = time.Nanosecond
	if id, tok := cr.Get(); tok != testToken || id != "Xy12Ab34Cd56Ef78" {
		t.Fatal(id, tok)
	}
	touch := func(p, content string) {
		t.Helper()
		writeFile(t, filepath.Dir(p), filepath.Base(p), content)
		future := time.Now().Add(time.Minute)
		os.Chtimes(p, future, future)
		time.Sleep(time.Millisecond)
	}

	second := strings.Repeat("2", MinTokenLength)
	touch(bp, "wings_config: "+wp+"\ntoken: "+second+"\n")
	if _, tok := cr.Get(); tok != second {
		t.Errorf("token not reloaded: %q", tok)
	}
	// An invalid edit keeps the previous token.
	touch(bp, "wings_config: "+wp+"\ntoken: short\n")
	if _, tok := cr.Get(); tok != second {
		t.Errorf("invalid token taken: %q", tok)
	}
	touch(wp, strings.Replace(fmt.Sprintf(wingsYAML, "abc"), "Xy12Ab34Cd56Ef78", "NewId", 1))
	touch(bp, "wings_config: "+wp+"\ntoken: "+second+"\n")
	if id, _ := cr.Get(); id != "NewId" {
		t.Errorf("token id not reloaded: %q", id)
	}
}
