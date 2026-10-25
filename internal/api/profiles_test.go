package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

func postStatus(t *testing.T, s *Server, uuid, body string) (int, ErrorBody, Profile) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+uuid+"/profiles", strings.NewReader(body))
	r.RemoteAddr = "10.0.0.2:1"
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var e ErrorBody
	var p Profile
	json.Unmarshal(w.Body.Bytes(), &e)
	json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code == 202 && w.Header().Get("Location") != "/api/v1/servers/"+uuid+"/profiles/"+p.ID {
		t.Errorf("Location %q", w.Header().Get("Location"))
	}
	return w.Code, e, p
}

func tmpFiles(t *testing.T, s *Server) []string {
	t.Helper()
	java, _ := s.Proc.(fakeProc)[exContainer().PID]
	es, _ := os.ReadDir(java.TmpDir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestProfileValidation(t *testing.T) {
	s := testServer(t)
	for _, body := range []string{
		``, `[]`, `{"seconds": 0}`, `{"seconds": 121}`, `{"seconds": "10"}`, `{"seconds": 10, "mode": "heap"}`,
		`{"seconds": 10, "mode": "alloc", "interval_ms": 5}`, `{"seconds": 10, "interval_ms": 0}`,
		`{"seconds": 10, "interval_ms": 1001}`, `{"seconds": 10, "file": "/etc/passwd"}`, `{"seconds": 10} {}`,
		`{"seconds": 10, "mode": "wall", "x": "` + strings.Repeat("a", 5000) + `"}`,
	} {
		code, e, _ := postStatus(t, s, uuidRunning, body)
		if code != 400 || e.Error != CodeInvalidRequest || e.Details["field"] == nil {
			t.Errorf("%.60s: %d %+v", body, code, e)
		}
	}
	if f, _ := fakes(s); len(f.loadCalls()) != 0 {
		t.Errorf("loads %v", f.loadCalls())
	}
	if code, e, _ := postStatus(t, s, uuidStopped, `{"seconds": 10}`); code != 409 || e.Error != CodeServerNotRunning {
		t.Errorf("stopped: %d %+v", code, e)
	}
}

func TestProfileLifecycle(t *testing.T) {
	s := testServer(t)
	f, c := fakes(s)
	code, _, p := postStatus(t, s, uuidRunning, `{"seconds": 30, "mode": "alloc"}`)
	if code != 202 || p.State != profile.StateRunning || p.AllocIntervalBytes == nil || p.IntervalMs != nil {
		t.Fatalf("%d %+v", code, p)
	}
	loads := f.loadCalls()
	if len(loads) != 1 || loads[0] != "start,alloc=524288,threads,log=/tmp/mpa-"+exProfileID+".log" || f.libs[0] != "/tmp/mpa-libasyncProfiler.so" {
		t.Errorf("loads %v libs %v", loads, f.libs)
	}
	if got := tmpFiles(t, s); strings.Join(got, ",") != profile.LibName {
		t.Errorf("tmp %v", got)
	}

	// One profile per server.
	if code, e, _ := postStatus(t, s, uuidRunning, `{"seconds": 10}`); code != 409 || e.Error != CodeProfileRunning || e.Details["profile_id"] != exProfileID {
		t.Errorf("second: %d %+v", code, e)
	}
	// Other servers' UUIDs do not see it.
	if w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidNoJava+"/profiles/"+exProfileID, testToken); w.Code != 404 {
		t.Errorf("foreign uuid: %d", w.Code)
	}

	c.Advance(30 * time.Second)
	fire(t, f.timers.end)
	r := waitState(t, s, uuidRunning, exProfileID, profile.StateDone)
	if r.Result.Unit != "bytes" || r.Result.Total != 2613 || len(r.Result.Warnings) != 0 {
		t.Errorf("result %+v", r.Result)
	}
	if last := f.loadCalls()[1]; last != "stop,collapsed,total,log=/tmp/mpa-"+exProfileID+".log,file=/tmp/mpa-"+exProfileID+".collapsed" {
		t.Errorf("stop %q", last)
	}
	// Only the library stays in /tmp.
	if got := tmpFiles(t, s); strings.Join(got, ",") != profile.LibName {
		t.Errorf("tmp after run %v", got)
	}
	// Kept for 15 minutes.
	c.Advance(14 * time.Minute)
	if w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/profiles/"+exProfileID, testToken); w.Code != 200 {
		t.Errorf("after 14 min: %d", w.Code)
	}
	c.Advance(2 * time.Minute)
	if w := do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/profiles/"+exProfileID, testToken); w.Code != 404 {
		t.Errorf("after 16 min: %d", w.Code)
	}
}

func TestProfileDelete(t *testing.T) {
	s := testServer(t)
	f, _ := fakes(s)
	if code, _, _ := postStatus(t, s, uuidRunning, `{"seconds": 60}`); code != 202 {
		t.Fatal(code)
	}
	path := "/api/v1/servers/" + uuidRunning + "/profiles/" + exProfileID
	w := do(t, s, http.MethodDelete, path, testToken)
	if w.Code != 204 || w.Body.Len() != 0 || w.Header().Get("User-Agent") == "" {
		t.Errorf("delete: %d %q", w.Code, w.Body)
	}
	loads := f.loadCalls()
	if len(loads) != 2 || !strings.HasPrefix(loads[1], "stop,collapsed,log=/tmp/mpa-"+exProfileID) {
		t.Errorf("loads %v", loads)
	}
	if got := tmpFiles(t, s); strings.Join(got, ",") != profile.LibName {
		t.Errorf("tmp after delete %v", got)
	}
	if w := do(t, s, http.MethodGet, path, testToken); w.Code != 404 {
		t.Errorf("get after delete: %d", w.Code)
	}
	if w := do(t, s, http.MethodDelete, path, testToken); w.Code != 404 {
		t.Errorf("second delete: %d", w.Code)
	}
	// A new profile can start right away.
	if code, _, _ := postStatus(t, s, uuidRunning, `{"seconds": 10}`); code != 202 {
		t.Errorf("restart: %d", code)
	}
}

func TestProfileLimits(t *testing.T) {
	s := testServer(t)
	s.Profiles.MaxConcurrent = 1
	s.Limits.MaxConcurrentProfiles = 1
	f, _ := fakes(s)
	// A second running server.
	const uuid2 = "00000000-0000-4000-8000-000000000010"
	info := exContainer()
	info.UUID, info.PID = uuid2, 6000
	s.Docker.(fakeDocker)[uuid2] = info
	java := s.Proc.(fakeProc)[exContainer().PID]
	java.TmpDir = t.TempDir()
	s.Proc.(fakeProc)[6000] = java
	ids := []string{"0000000000000001", "0000000000000002"}
	s.Profiles.NewID = func() string { id := ids[0]; ids = ids[1:]; return id }

	if code, _, _ := postStatus(t, s, uuidRunning, `{"seconds": 10}`); code != 202 {
		t.Fatal(code)
	}
	if code, e, _ := postStatus(t, s, uuid2, `{"seconds": 10}`); code != 429 || e.Error != CodeTooManyProfiles || e.Details["limit"] != float64(1) {
		t.Errorf("limit: %d %+v", code, e)
	}

	// Not enough space in /tmp.
	s = testServer(t)
	f, _ = fakes(s)
	f.freeBytes = 1 << 20
	code, e, _ := postStatus(t, s, uuidRunning, `{"seconds": 10}`)
	if code != 507 || e.Error != CodeInsufficientTmpSpace || e.Details["free_bytes"] != float64(1<<20) {
		t.Errorf("tmp space: %d %+v", code, e)
	}
	if len(f.loadCalls()) != 0 || len(tmpFiles(t, s)) != 0 {
		t.Errorf("nothing may happen: %v %v", f.loadCalls(), tmpFiles(t, s))
	}

	// The agent refuses to start: attach_failed, nothing stays behind.
	s = testServer(t)
	f, _ = fakes(s)
	f.loadErr = &attach.CommandError{Code: 200, Message: "return code: 200"}
	code, e, _ = postStatus(t, s, uuidRunning, `{"seconds": 10}`)
	if code != 502 || e.Error != CodeAttachFailed || e.Details["code"] != float64(200) {
		t.Errorf("agent error: %d %+v", code, e)
	}
	var list ProfileList
	json.Unmarshal(do(t, s, http.MethodGet, "/api/v1/servers/"+uuidRunning+"/profiles", testToken).Body.Bytes(), &list)
	if len(list.Profiles) != 0 {
		t.Errorf("failed start must not be listed: %+v", list)
	}

	// JVM too young: no signal, no load.
	s = testServer(t)
	s.SocketExists = func(string) bool { return false }
	f, c := fakes(s)
	c.Set(time.UnixMilli(exJVMStart + 5000))
	if code, e, _ := postStatus(t, s, uuidRunning, `{"seconds": 10}`); code != 409 || e.Error != CodeAttachUnavailable || f.starts != 0 || len(f.loadCalls()) != 0 {
		t.Errorf("young: %d %+v", code, e)
	}
}

func TestCleanupAll(t *testing.T) {
	s := testServer(t)
	java := s.Proc.(fakeProc)[exContainer().PID]
	for _, n := range []string{"mpa-0123456789abcdef.collapsed", "mpa-0123456789abcdef.log", profile.LibName, "other.txt"} {
		os.WriteFile(filepath.Join(java.TmpDir, n), []byte("x"), 0o644)
	}
	os.Mkdir(filepath.Join(java.TmpDir, "mpa-dir"), 0o755)
	s.CleanupAll(t.Context(), listFunc(func() []string { return []string{uuidRunning, uuidStopped, uuidMissing} }))
	got := tmpFiles(t, s)
	if strings.Join(got, ",") != "mpa-dir,"+profile.LibName+",other.txt" {
		t.Errorf("after cleanup %v", got)
	}
	// The library is not mapped (no maps file), so no stop is sent.
	if f, _ := fakes(s); len(f.loadCalls()) != 0 {
		t.Errorf("loads %v", f.loadCalls())
	}
}

type listFunc func() []string

func (l listFunc) List(context.Context) ([]string, error) { return l(), nil }
