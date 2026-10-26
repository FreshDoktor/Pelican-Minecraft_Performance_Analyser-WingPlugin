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

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/config"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

const (
	testToken   = "4f1c0d5e8a9b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5" // the bridge token, not the Wings token
	testTokenID = "Xy12Ab34Cd56Ef78"
	uuidRunning = exUUID
	uuidStopped = "00000000-0000-4000-8000-000000000001"
	uuidNoJava  = "00000000-0000-4000-8000-000000000002"
	uuidNoPerf  = "00000000-0000-4000-8000-000000000003"
	uuidPaused  = "00000000-0000-4000-8000-000000000004"
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
	running := exContainer()
	stopped := exContainer()
	stopped.UUID, stopped.State, stopped.Running, stopped.PID = uuidStopped, "exited", false, 0
	noJava := exContainer()
	noJava.UUID, noJava.PID = uuidNoJava, 3000
	paused := exContainer()
	paused.UUID, paused.State, paused.Running = uuidPaused, "paused", false
	noPerf := exContainer()
	noPerf.UUID, noPerf.PID = uuidNoPerf, 4000

	java := exJava()
	java.Hsperf = ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"}
	java.Tmp = ctmp.Dir{Root: t.TempDir()} // the container's /tmp
	javaNoPerf := exJava()
	javaNoPerf.User, javaNoPerf.Hsperf = "", ctmp.File{}

	c := &clock{t: time.UnixMilli(exBridgeStart)}
	s := &Server{
		Version:      "1.0.0",
		Creds:        config.StaticCredentials(testTokenID, testToken),
		Docker:       fakeDocker{uuidRunning: running, uuidStopped: stopped, uuidNoJava: noJava, uuidNoPerf: noPerf, uuidPaused: paused},
		Proc:         fakeProc{running.PID: java, noPerf.PID: javaNoPerf},
		Attach:       newFakeAttach(t, c),
		Features:     exFeatures(),
		TmpfsBytes:   exTmpfs,
		Limits:       NodeLimits{MaxConcurrentProfiles: 2, MaxProfileSeconds: 120, MaxTickMonitors: 10},
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:          c.Now,
		HostCPUs:     exHostCPUs,
		SocketExists: func(f ctmp.File) bool { return f.Dir.Root != "" && f.Name == ".java_pid26" },
	}
	f := s.Attach.(*fakeAttach)
	collapsed, err := os.ReadFile("testdata/profile-wall.collapsed")
	if err != nil {
		t.Fatal(err)
	}
	f.collapsed = string(collapsed)
	f.timers = newFakeTimers()
	ticks := int64(1000)
	s.Profiles = &profile.Manager{
		MaxConcurrent: 2,
		Lib:           []byte("fake libasyncProfiler.so"),
		Loader:        f,
		ProcRoot:      t.TempDir(),
		Check:         s.CheckServer,
		Now:           c.Now,
		ReadHsperf:    func(p ctmp.File) (*hsperf.Data, error) { return f.readHsperf(s, p) },
		CPUTicks:      func(int) (int64, error) { v := ticks; ticks += f.cpuStep; return v, nil },
		FreeBytes:     func(ctmp.Dir) (int64, bool) { return f.freeBytes, true },
		After:         f.timers.After,
		Poll:          f.timers.Poll,
		NewID:         func() string { return exProfileID },
		Log:           s.Log,
	}
	s.Ticks = &profile.Ticks{
		Max:      10,
		Profiles: s.Profiles,
		ThreadID: func(int, string) (int, bool, error) { return 60, true, nil },
		HostTID:  func(int, int) (int, error) { return 4134, nil },
		SchedStat: func(int, int) (int64, int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.schedRun, f.schedWait, nil
		},
		Jcmd:  f.Jcmd,
		Now:   c.Now,
		Every: f.timers.Every,
		NewID: func() string { return exTickMonitorID },
		Log:   s.Log,
	}
	s.Profiles.Yield = s.Ticks.Yield
	s.Init()
	c.Set(time.UnixMilli(exNow))
	return s
}

// fakes returns the fake attacher and clock of a test server.
func fakes(s *Server) (*fakeAttach, *clock) {
	f := s.Attach.(*fakeAttach)
	return f, f.clock
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
		{"/api/v1/servers/" + uuidRunning + "/vm", "vm", 200},
		{"/api/v1/servers/" + uuidRunning + "/metrics?live=1", "metrics-live", 200},
		{"/api/v1/servers/" + uuidRunning + "/threads", "threads", 200},
		{"/api/v1/servers/" + uuidRunning + "/histogram?limit=100", "histogram", 200},
		{"/api/v1/servers/" + uuidRunning + "/metrics", "metrics", 200},
		{"/api/v1/servers/" + uuidStopped + "/vm", "error-server-not-running", 409},
		{"/api/v1/servers/" + uuidNoJava + "/metrics", "error-no-java-process", 409},
		{"/api/v1/servers/" + uuidNoPerf + "/metrics", "error-hsperf-unavailable", 409},
		{"/api/v1/servers/" + uuidMissing + "/status", "error-server-not-found", 404},
		{"/api/v1/servers/" + uuidDocker + "/vm", "error-docker-unavailable", 503},
		{"/api/v1/servers/" + uuidRunning + "/profiles", "profiles-empty", 200},
		{"/api/v1/servers/" + uuidRunning + "/profiles/" + exProfileID, "error-profile-not-found", 404},
		{"/api/v1/nope", "error-not-found", 404},
		{"/api/v1/servers/" + uuidRunning + "/nope", "error-not-found", 404},
	}
	for _, c := range cases {
		_, clk := fakes(s)
		clk.Set(time.UnixMilli(exNow))
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

func TestMetricsLive(t *testing.T) {
	s := testServer(t)
	path := "/api/v1/servers/" + uuidRunning + "/metrics?live=1"
	var m MetricsResponse
	w := do(t, s, http.MethodGet, path, testToken)
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || w.Code != 200 {
		t.Fatal(w.Code, err)
	}
	lh := m.LiveHeap
	if lh == nil || lh.UsedBytes != (46125+500231)*1024 || *lh.CommittedBytes != (96704+500348)*1024 || lh.MaxBytes != nil ||
		lh.Metaspace == nil || lh.Metaspace.UsedBytes != 112295*1024 || m.LiveHeapError != nil {
		t.Errorf("live heap: %+v %v", lh, m.LiveHeapError)
	}
	f, _ := fakes(s)
	if strings.Join(f.cmds, ",") != "GC.heap_info,VM.metaspace" {
		t.Errorf("commands %v", f.cmds)
	}

	f.out["GC.heap_info"] = "something new"
	m = MetricsResponse{}
	json.Unmarshal(do(t, s, http.MethodGet, path, testToken).Body.Bytes(), &m)
	if m.LiveHeap != nil || m.LiveHeapError == nil || *m.LiveHeapError != CodeParseFailed {
		t.Errorf("unparsable: %v %v", m.LiveHeap, m.LiveHeapError)
	}

	s.Attach = nil
	m = MetricsResponse{}
	json.Unmarshal(do(t, s, http.MethodGet, path, testToken).Body.Bytes(), &m)
	if m.LiveHeap != nil || m.LiveHeapError == nil || *m.LiveHeapError != CodeNotImplemented {
		t.Errorf("without attacher: %v %v", m.LiveHeap, m.LiveHeapError)
	}
	if w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/metrics?live=maybe", testToken); w.Code != 400 {
		t.Errorf("bad live: %d", w.Code)
	}
}

// withVMArgs returns a server whose JVM reports the given java.rt.vmArgs.
func withVMArgs(t *testing.T, s *Server, vmArgs string) {
	t.Helper()
	orig := s.ReadHsperf
	s.ReadHsperf = func(p ctmp.File) (*hsperf.Data, error) {
		d, err := orig(p)
		if err == nil {
			d.SetString("java.rt.vmArgs", vmArgs)
		}
		return d, err
	}
}

// TestAttachSafety checks when the listener may be started (SIGQUIT).
func TestAttachSafety(t *testing.T) {
	path := "/api/v1/servers/" + uuidRunning + "/threads"
	begin := int64(1792866595712) // createVmBeginTime of the fixture

	// Listener running: no start, the command runs.
	s := testServer(t)
	if w := do(t, s, http.MethodGet, path, testToken); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if f, _ := fakes(s); f.starts != 0 || f.targets[0].HostPID != 2817346 || f.targets[0].NSPID != 26 {
		t.Errorf("starts %d, targets %+v", f.starts, f.targets)
	}

	// Listener not running, JVM old enough: start once, then the command.
	s = testServer(t)
	s.SocketExists = func(ctmp.File) bool { return false }
	if w := do(t, s, http.MethodGet, path, testToken); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if f, _ := fakes(s); f.starts != 1 || len(f.cmds) != 1 {
		t.Errorf("starts %d, cmds %v", f.starts, f.cmds)
	}

	// JVM younger than 30 s: no signal.
	s = testServer(t)
	s.SocketExists = func(ctmp.File) bool { return false }
	f, c := fakes(s)
	c.Set(time.UnixMilli(begin + 29_000))
	w := do(t, s, http.MethodGet, path, testToken)
	var e ErrorBody
	json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 409 || e.Error != CodeAttachUnavailable || e.Details["reason"] != ReasonJVMStarting || f.starts != 0 || len(f.cmds) != 0 {
		t.Errorf("young JVM: %d %s, starts %d", w.Code, w.Body, f.starts)
	}

	// -Xrs and DisableAttachMechanism never get a signal.
	for reason, vmArgs := range map[string]string{ReasonXrs: "-Xmx1G -Xrs", ReasonDisabled: "-Xmx1G -XX:+DisableAttachMechanism"} {
		s = testServer(t)
		s.SocketExists = func(ctmp.File) bool { return false }
		withVMArgs(t, s, vmArgs)
		f, _ := fakes(s)
		w := do(t, s, http.MethodGet, path, testToken)
		e = ErrorBody{}
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != 409 || e.Details["reason"] != reason || f.starts != 0 || len(f.cmds) != 0 {
			t.Errorf("%s: %d %s, starts %d", reason, w.Code, w.Body, f.starts)
		}
		// With a running listener -Xrs is harmless (no signal needed); disabled stays disabled.
		s.SocketExists = func(ctmp.File) bool { return true }
		w = do(t, s, http.MethodGet, path, testToken)
		if want := map[string]int{ReasonXrs: 200, ReasonDisabled: 409}[reason]; w.Code != want {
			t.Errorf("%s with listener: %d, want %d", reason, w.Code, want)
		}
	}

	// No hsperfdata: the server is not ready, nothing is attempted.
	s = testServer(t)
	f, _ = fakes(s)
	if w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidNoPerf+"/threads", testToken); w.Code != 409 || f.starts != 0 || len(f.cmds) != 0 {
		t.Errorf("no hsperfdata: %d", w.Code)
	}
}

func TestAttachErrors(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{attach.ErrTimeout, 504, CodeAttachTimeout},
		{attach.ErrTooLarge, 502, CodeOutputTooLarge},
		{&attach.CommandError{Code: -1, Message: "java.lang.IllegalArgumentException: x"}, 502, CodeAttachFailed},
		{attach.ErrProtocol, 502, CodeAttachFailed},
	}
	for _, c := range cases {
		s := testServer(t)
		f, _ := fakes(s)
		for _, cmd := range []string{"Thread.print -l", "GC.class_histogram -all", "VM.flags"} {
			f.errs[cmd] = c.err
		}
		for _, p := range []string{"/threads", "/histogram"} {
			w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+p, testToken)
			var e ErrorBody
			json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != c.status || e.Error != c.code {
				t.Errorf("%v %s: %d %s", c.err, p, w.Code, w.Body)
			}
		}
		var vm VMResponse
		json.Unmarshal(do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/vm", testToken).Body.Bytes(), &vm)
		if vm.Flags != nil || vm.FlagsError == nil || *vm.FlagsError != c.code {
			t.Errorf("%v vm: %v %v", c.err, vm.Flags, vm.FlagsError)
		}
	}
	s := testServer(t)
	f, _ := fakes(s)
	f.out["GC.class_histogram -all"] = "nothing useful"
	if w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/histogram", testToken); w.Code != 502 {
		t.Errorf("unparsable histogram: %d", w.Code)
	}
	for _, q := range []string{"0", "100001", "x"} {
		if w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/histogram?limit="+q, testToken); w.Code != 400 {
			t.Errorf("limit=%s: %d", q, w.Code)
		}
	}
}

// TestPaused: a paused container counts as not running everywhere.
func TestPaused(t *testing.T) {
	s := testServer(t)
	for _, p := range []string{"/vm", "/metrics", "/metrics?live=1", "/threads", "/histogram"} {
		w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidPaused+p, testToken)
		var e ErrorBody
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != 409 || e.Error != CodeServerNotRunning || e.Details["container_state"] != "paused" {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	var st StatusResponse
	json.Unmarshal(do(t, s, http.MethodGet, "/api/v1/servers/"+uuidPaused+"/status", testToken).Body.Bytes(), &st)
	if st.State != CodeServerNotRunning || st.Server.Running || st.Server.ContainerState != "paused" || st.Java != nil {
		t.Errorf("status: %+v", st)
	}
}

func TestAuth(t *testing.T) {
	s := testServer(t)
	ua := "MPA Bridge/v1.0.0 (id:" + testTokenID + ")"
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
	// Without a token nothing is accepted, not even an empty bearer.
	s.Creds = config.StaticCredentials(testTokenID, "")
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "10.0.0.2:1"
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("empty token: %d", w.Code)
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
	if w := do(t, s, http.MethodDelete, "/api/v1/servers/"+uuidRunning+"/profiles/"+exProfileID, testToken); w.Code != 404 {
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
