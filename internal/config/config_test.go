package config

import (
	"fmt"
	"os"
	"path/filepath"
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

func TestLoadWings(t *testing.T) {
	t.Setenv("WINGS_TOKEN", "")
	t.Setenv("WINGS_TOKEN_ID", "")
	w, err := LoadWings(wings(t, "abc123"))
	if err != nil {
		t.Fatal(err)
	}
	if w.TokenID != "Xy12Ab34Cd56Ef78" || w.Token != "abc123" || w.API.SSL.Enabled ||
		w.API.SSL.Cert != "/etc/letsencrypt/live/wing.example.com/fullchain.pem" || *w.Docker.TmpfsSize != 150 {
		t.Errorf("unexpected %+v", w)
	}

	t.Setenv("WINGS_TOKEN", "from-env")
	t.Setenv("WINGS_TOKEN_ID", "id-env")
	w, err = LoadWings(wings(t, "abc123"))
	if err != nil || w.Token != "from-env" || w.TokenID != "id-env" {
		t.Errorf("env must win: %+v %v", w, err)
	}

	t.Setenv("WINGS_TOKEN", "")
	if _, err := LoadWings(wings(t, `""`)); err == nil {
		t.Error("empty token must fail")
	}
	dir := t.TempDir()
	writeFile(t, dir, "tok", "filetoken\n")
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	w, err = LoadWings(wings(t, "file://${CREDENTIALS_DIRECTORY}/tok"))
	if err != nil || w.Token != "filetoken" {
		t.Errorf("file token: %+v %v", w, err)
	}
}

func TestLoadDefaultsAndBridgeFile(t *testing.T) {
	t.Setenv("WINGS_TOKEN", "")
	t.Setenv("DOCKER_HOST", "")
	wp := wings(t, "abc")
	dir := t.TempDir()

	// Missing bridge file: defaults (wings path must then be the default, so point to it).
	bp := writeFile(t, dir, "mpa-bridge.yml", "wings_config: "+wp+"\n")
	c, err := Load(bp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8090 || c.Host != "0.0.0.0" || c.MaxConcurrentProfiles != 2 || c.MaxProfileSeconds != 120 ||
		c.DockerSocket != "/var/run/docker.sock" || c.ProcRoot != "/proc" || len(c.Allowed) != 0 {
		t.Errorf("defaults: %+v", c.Bridge)
	}
	if c.TmpfsSizeBytes != 150<<20 || c.Token != "abc" {
		t.Errorf("wings: %d %q", c.TmpfsSizeBytes, c.Token)
	}

	bp = writeFile(t, dir, "mpa-bridge.yml", "wings_config: "+wp+"\nport: 9000\nallowed_ips: [10.0.0.5, 192.168.0.0/24, '::ffff:10.0.0.6']\nmax_profile_seconds: 60\n")
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
		bp = writeFile(t, dir, "mpa-bridge.yml", "wings_config: "+wp+"\n"+bad+"\n")
		if _, err := Load(bp); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.yml")); err == nil {
		t.Error("missing bridge file falls back to /etc/pelican/config.yml, which does not exist here")
	}
}

func TestCredentialsReload(t *testing.T) {
	t.Setenv("WINGS_TOKEN", "")
	p := wings(t, "first")
	c := &Config{Bridge: Bridge{WingsConfig: p}}
	w, _ := LoadWings(p)
	c.Wings = *w
	cr := NewCredentials(c)
	cr.interval = time.Nanosecond
	if _, tok := cr.Get(); tok != "first" {
		t.Fatal(tok)
	}
	if err := os.WriteFile(p, []byte(fmt.Sprintf(wingsYAML, "second")), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	os.Chtimes(p, future, future)
	time.Sleep(time.Millisecond)
	if _, tok := cr.Get(); tok != "second" {
		t.Errorf("token not reloaded: %q", tok)
	}
}
