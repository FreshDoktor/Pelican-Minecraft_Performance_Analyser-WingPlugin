package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/config"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
)

const (
	testToken   = "wings-node-token"
	testTokenID = "Xy12Ab34Cd56Ef78"
	uuidRunning = exUUID
	uuidStopped = "00000000-0000-4000-8000-000000000001"
	uuidNoJava  = "00000000-0000-4000-8000-000000000002"
	uuidNoPerf  = "00000000-0000-4000-8000-000000000003"
	uuidMissing = "00000000-0000-4000-8000-0000000000ff"
	uuidDocker  = "00000000-0000-4000-8000-0000000000ee"
)

type fakeDocker map[string]container.Info

func (f fakeDocker) Inspect(_ context.Context, uuid string) (container.Info, error) {
	if uuid == uuidDocker {
		return container.Info{}, container.ErrDockerUnavailable
	}
	info, ok := f[uuid]
	if !ok {
		return container.Info{}, container.ErrNotFound
	}
	return info, nil
}

// fakeProc maps the container's init PID to its JVM.
type fakeProc map[int]container.JavaProcess

func (f fakeProc) FindJava(pid int) (container.JavaProcess, error) {
	j, ok := f[pid]
	if !ok {
		return container.JavaProcess{}, container.ErrNoJava
	}
	return j, nil
}

func testServer(t *testing.T) *Server {
	t.Helper()
	fixture, _ := filepath.Abs("../../testdata/mpa-hsperf.bin")
	running := exContainer()
	stopped := exContainer()
	stopped.UUID, stopped.State, stopped.Running, stopped.PID = uuidStopped, "exited", false, 0
	noJava := exContainer()
	noJava.UUID, noJava.PID = uuidNoJava, 3000
	noPerf := exContainer()
	noPerf.UUID, noPerf.PID = uuidNoPerf, 4000

	java := exJava()
	java.Hsperf = fixture
	java.TmpDir = "/proc/2817346/root/tmp"
	javaNoPerf := exJava()
	javaNoPerf.User, javaNoPerf.Hsperf = "", ""

	now := time.UnixMilli(exBridgeStart)
	s := &Server{
		Version:    "0.1.0",
		Creds:      config.StaticCredentials(testTokenID, testToken),
		Docker:     fakeDocker{uuidRunning: running, uuidStopped: stopped, uuidNoJava: noJava, uuidNoPerf: noPerf},
		Proc:       fakeProc{running.PID: java, noPerf.PID: javaNoPerf},
		TmpfsBytes: exTmpfs,
		Limits:     NodeLimits{MaxConcurrentProfiles: 2, MaxProfileSeconds: 120},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return now },
		HostCPUs:   exHostCPUs,
		SocketExists: func(p string) bool {
			return strings.HasSuffix(filepath.ToSlash(p), "/.java_pid26")
		},
	}
	s.Init()
	now = time.UnixMilli(exNow)
	return s
}

func do(t *testing.T, s *Server, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "10.0.0.2:51234"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func readExample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(examplesDir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withFeatures replaces the feature list of an example by this build's and
// normalizes the server UUID.
func withFeatures(t *testing.T, b []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if br, ok := m["bridge"].(map[string]any); ok {
		br["features"] = Implemented
	}
	// The test containers have their own UUIDs; the examples all use exUUID.
	if sv, ok := m["server"].(map[string]any); ok {
		sv["uuid"] = exUUID
	}
	if _, ok := m["features"]; ok {
		m["features"] = Implemented
	}
	out, _ := json.Marshal(m)
	return out
}

// TestGolden checks the milestone-1 endpoints against docs/examples.
func TestGolden(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		path, example string
		status        int
	}{
		{"/api/v1/status", "node-status", 200},
		{"/api/v1/servers/" + uuidRunning + "/status", "status", 200},
		{"/api/v1/servers/" + strings.ToUpper(uuidRunning) + "/status", "status", 200},
		{"/api/v1/servers/" + uuidStopped + "/status", "status-not-running", 200},
		{"/api/v1/servers/" + uuidNoJava + "/status", "status-no-java", 200},
		{"/api/v1/servers/" + uuidNoPerf + "/status", "status-hsperf-unavailable", 200},
		{"/api/v1/servers/" + uuidRunning + "/vm", "vm-without-flags", 200},
		{"/api/v1/servers/" + uuidRunning + "/metrics", "metrics", 200},
		{"/api/v1/servers/" + uuidStopped + "/vm", "error-server-not-running", 409},
		{"/api/v1/servers/" + uuidNoJava + "/metrics", "error-no-java-process", 409},
		{"/api/v1/servers/" + uuidNoPerf + "/metrics", "error-hsperf-unavailable", 409},
		{"/api/v1/servers/" + uuidMissing + "/status", "error-server-not-found", 404},
		{"/api/v1/servers/" + uuidDocker + "/vm", "error-docker-unavailable", 503},
		{"/api/v1/servers/" + uuidRunning + "/threads", "error-not-implemented", 501},
		{"/api/v1/servers/" + uuidRunning + "/histogram", "error-not-implemented", 501},
		{"/api/v1/servers/" + uuidRunning + "/profiles", "error-not-implemented", 501},
		{"/api/v1/servers/" + uuidRunning + "/profiles/" + exProfileID, "error-not-implemented", 501},
		{"/api/v1/nope", "error-not-found", 404},
		{"/api/v1/servers/" + uuidRunning + "/nope", "error-not-found", 404},
	}
	for _, c := range cases {
		w := do(t, s, http.MethodGet, c.path, testToken)
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d: %s", c.path, w.Code, c.status, w.Body)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("%s: content type %q", c.path, ct)
		}
		if !equalJSON(t, withFeatures(t, readExample(t, c.example)), withFeatures(t, w.Body.Bytes())) {
			t.Errorf("%s differs from %s.json:\n%s", c.path, c.example, w.Body)
		}
	}
}

func TestMetricsLiveNotImplemented(t *testing.T) {
	s := testServer(t)
	w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/metrics?live=1", testToken)
	var m MetricsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || w.Code != 200 {
		t.Fatal(w.Code, err)
	}
	if m.LiveHeap != nil || m.LiveHeapError == nil || *m.LiveHeapError != CodeNotImplemented {
		t.Errorf("live heap: %v %v", m.LiveHeap, m.LiveHeapError)
	}
	if w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/metrics?live=maybe", testToken); w.Code != 400 {
		t.Errorf("bad live: %d", w.Code)
	}
}

func TestAuth(t *testing.T) {
	s := testServer(t)
	ua := "MPA Bridge/v0.1.0 (id:" + testTokenID + ")"
	path := "/api/v1/servers/" + uuidRunning + "/status"
	for name, c := range map[string]struct {
		header string
		status int
	}{
		"missing":   {"", 401},
		"basic":     {"Basic " + testToken, 401},
		"wrong":     {"Bearer nope", 401},
		"prefix":    {"Bearer " + testToken[:5], 401},
		"lowercase": {"bearer " + testToken, 401},
		"ok":        {"Bearer " + testToken, 200},
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "10.0.0.2:1"
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != c.status {
			t.Errorf("%s: %d, want %d", name, w.Code, c.status)
		}
		if got := w.Header().Get("User-Agent"); got != ua {
			t.Errorf("%s: User-Agent %q", name, got)
		}
		if c.status == 401 {
			if !equalJSON(t, readExample(t, "error-unauthorized"), w.Body.Bytes()) {
				t.Errorf("%s: body %s", name, w.Body)
			}
			if w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("%s: no WWW-Authenticate", name)
			}
		}
	}
	// Unknown routes need the token too.
	if w := do(t, s, http.MethodGet, "/nope", ""); w.Code != 401 {
		t.Errorf("unauthenticated 404 leaks: %d", w.Code)
	}
}

func TestAllowedIPs(t *testing.T) {
	s := testServer(t)
	p, _ := config.ParsePrefix("10.0.0.0/24")
	q, _ := config.ParsePrefix("2001:db8::1")
	s.Allowed = []netip.Prefix{p, q}
	for addr, want := range map[string]int{
		"10.0.0.2:1":          200,
		"[::ffff:10.0.0.9]:1": 200,
		"[2001:db8::1]:1":     200,
		"10.0.1.2:1":          403,
		"[2001:db8::2]:1":     403,
		"garbage":             403,
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		r.RemoteAddr = addr
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", addr, w.Code, want)
		}
		if w.Header().Get("User-Agent") == "" {
			t.Errorf("%s: User-Agent missing", addr)
		}
		if want == 403 && !equalJSON(t, readExample(t, "error-ip-not-allowed"), w.Body.Bytes()) {
			t.Errorf("%s: body %s", addr, w.Body)
		}
	}
}

func TestRoutingErrors(t *testing.T) {
	s := testServer(t)
	w := do(t, s, http.MethodPost, "/api/v1/servers/"+uuidRunning+"/metrics", testToken)
	if w.Code != 405 || w.Header().Get("Allow") != "GET" || !equalJSON(t, readExample(t, "error-method-not-allowed"), w.Body.Bytes()) {
		t.Errorf("405: %d %q %s", w.Code, w.Header().Get("Allow"), w.Body)
	}
	if w := do(t, s, http.MethodHead, "/api/v1/servers/"+uuidRunning+"/status", testToken); w.Code != 200 {
		t.Errorf("HEAD: %d", w.Code)
	}
	if w := do(t, s, http.MethodDelete, "/api/v1/servers/"+uuidRunning+"/profiles/"+exProfileID, testToken); w.Code != 501 {
		t.Errorf("DELETE profile: %d", w.Code)
	}
	if w := do(t, s, http.MethodPut, "/api/v1/servers/"+uuidRunning+"/profiles", testToken); w.Code != 405 || w.Header().Get("Allow") != "GET, POST" {
		t.Errorf("PUT profiles: %d %q", w.Code, w.Header().Get("Allow"))
	}
	for _, p := range []string{
		"/api/v1/servers/not-a-uuid/status",
		"/api/v1/servers/" + uuidRunning + "/profiles/XYZ",
		"/api/v1/servers/" + uuidRunning + "/profiles/0123456789abcdef0",
	} {
		w := do(t, s, http.MethodGet, p, testToken)
		var e ErrorBody
		if json.Unmarshal(w.Body.Bytes(), &e); w.Code != 400 || e.Error != CodeInvalidRequest || e.Details["field"] == nil {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
}

func TestErrorCatalog(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Errors {
		if seen[e.Code] || e.Status < 400 || e.Message == "" {
			t.Errorf("bad entry %+v", e)
		}
		seen[e.Code] = true
	}
	if newError("no_such_code", "", nil).status != 500 {
		t.Error("unknown codes must map to 500")
	}
}
