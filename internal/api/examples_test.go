package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

// go test ./internal/api -run Examples -update rewrites docs/examples.
var update = flag.Bool("update", false, "rewrite docs/examples")

const examplesDir = "../../docs/examples"

// Fixed inputs of the examples. Values from the fixtures where they exist;
// UUID, PIDs on the host and timestamps are made up.
const (
	exUUID         = "1a7ce997-259b-452e-8b4e-cecc464142ca"
	exNow          = int64(1792929810000) // 2026-10-25 12:03:30 UTC, ~17.6 h after the JVM start in mpa-hsperf.bin
	exBridgeStart  = int64(1792926210000)
	exHostCPUs     = 24
	exTmpfs        = int64(100) << 20
	exProfileID    = "c0f3e1a2b4d59687"
	exProfileStart = int64(1792930250000)

	exTickMonitorID = "5b1e0c2d9a874f36"
	exTickStart     = int64(1792930100000)
)

func exContainer() container.Info {
	return container.Info{
		UUID: exUUID, State: "running", Running: true,
		StartedAt: time.UnixMilli(1792866593104), PID: 2817301,
		// Wings: server memory 1907 MB, overhead multiplier 1.15 (< 2048 MB), CPU limit 100 %.
		Memory: 2299579597, MemoryReservation: 1999634432,
		CPUQuota: 100000, CPUPeriod: 100000,
	}
}

func exJava() container.JavaProcess {
	return container.JavaProcess{HostPID: 2817346, NSPID: 26, User: "container", Hsperf: ctmp.File{Name: "x"}}
}

func loadFixture(t *testing.T) *hsperf.Data {
	t.Helper()
	d, err := hsperf.ReadFile(ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func exFeatures() []string {
	return []string{FeatureStatus, FeatureVM, FeatureMetrics, FeatureThreads, FeatureHistogram, FeatureVMFlags, FeatureLiveHeap, FeatureProfiles, FeatureTicks}
}

// Feature lists of the milestones, for node-status-m1/-m2/-m3.
var (
	featuresM1 = []string{FeatureStatus, FeatureVM, FeatureMetrics}
	featuresM2 = []string{FeatureStatus, FeatureVM, FeatureMetrics, FeatureThreads, FeatureHistogram, FeatureVMFlags, FeatureLiveHeap}
	featuresM3 = []string{FeatureStatus, FeatureVM, FeatureMetrics, FeatureThreads, FeatureHistogram, FeatureVMFlags, FeatureLiveHeap, FeatureProfiles}
)

// get runs a request against the real handlers and returns the body. Bodies
// of other test containers are rewritten to exUUID.
func get(t *testing.T, s *Server, path string, status int) json.RawMessage {
	t.Helper()
	if f, ok := s.Attach.(*fakeAttach); ok && f.clock.Now().UnixMilli() > exNow {
		f.clock.Set(time.UnixMilli(exNow))
	}
	w := do(t, s, http.MethodGet, path, testToken)
	if w.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", path, w.Code, status, w.Body)
	}
	b := w.Body.Bytes()
	for _, u := range []string{uuidStopped, uuidNoJava, uuidNoPerf, uuidPaused} {
		b = bytes.ReplaceAll(b, []byte(u), []byte(exUUID))
	}
	return json.RawMessage(bytes.TrimSpace(b))
}

// examples builds every example document. Everything the bridge serves comes
// from the HTTP handlers with fake Docker, /proc and attach (fixture outputs);
// the profile examples (milestone 3) are built from the types directly.
func examples(t *testing.T) map[string]any {
	ex := map[string]any{}
	srv := func(features []string) *Server {
		s := testServer(t)
		s.Features = features
		return s
	}
	s := srv(exFeatures())
	base := "/api/v1/servers/"

	ex["node-status"] = get(t, s, "/api/v1/status", 200)
	ex["node-status-m1"] = get(t, srv(featuresM1), "/api/v1/status", 200)
	ex["node-status-m2"] = get(t, srv(featuresM2), "/api/v1/status", 200)
	ex["node-status-m3"] = get(t, srv(featuresM3), "/api/v1/status", 200)
	ex["status"] = get(t, s, base+exUUID+"/status", 200)
	ex["status-not-running"] = get(t, s, base+uuidStopped+"/status", 200)
	ex["status-no-java"] = get(t, s, base+uuidNoJava+"/status", 200)
	ex["status-hsperf-unavailable"] = get(t, s, base+uuidNoPerf+"/status", 200)
	ex["vm"] = get(t, s, base+exUUID+"/vm", 200)
	ex["metrics"] = get(t, s, base+exUUID+"/metrics", 200)
	ex["metrics-live"] = get(t, s, base+exUUID+"/metrics?live=1", 200)
	ex["threads"] = get(t, s, base+exUUID+"/threads", 200)
	ex["histogram"] = get(t, s, base+exUUID+"/histogram?limit=100", 200)

	// A JVM started 12 s ago without a running listener: no SIGQUIT yet.
	young := testServer(t)
	young.SocketExists = func(ctmp.File) bool { return false }
	_, c := fakes(young)
	c.Set(time.UnixMilli(1792866595712 + 12_000))
	ex["vm-without-flags"] = get(t, young, base+exUUID+"/vm", 200)

	for k, v := range profileExamples(t) {
		ex[k] = v
	}
	for k, v := range tickExamples(t) {
		ex[k] = v
	}
	for _, e := range Errors {
		ex["error-"+strings.ReplaceAll(e.Code, "_", "-")] = newError(e.Code, "", exErrorDetails[e.Code]).body
	}
	return ex
}

var exErrorDetails = map[string]map[string]any{
	CodeInvalidRequest:       {"field": "seconds", "reason": "must be between 1 and 120"},
	CodeServerNotRunning:     {"container_state": "exited"},
	CodeAttachUnavailable:    {"reason": ReasonXrs},
	CodeProfileRunning:       {"profile_id": exProfileID},
	CodeTooManyProfiles:      {"limit": 2},
	CodeTooManyTickMonitors:  {"limit": 10},
	CodeInsufficientTmpSpace: {"free_bytes": 1048576, "required_bytes": 8388608},
}

// post sends a JSON body to the real handlers.
func post(t *testing.T, s *Server, path, body string, status int) json.RawMessage {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.RemoteAddr = "10.0.0.2:51234"
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("POST %s: status %d, want %d: %s", path, w.Code, status, w.Body)
	}
	return json.RawMessage(bytes.TrimSpace(w.Body.Bytes()))
}

// Profile timeline of the examples (fake clock).
const (
	exPauseOffsetMs = 4183.211 // the young collection during the profile
	exJVMStart      = int64(1792866595712)
)

// gcDuringProfile adds one young collection (20.052 ms) and 3 safepoints to the fixture.
func gcDuringProfile(d *hsperf.Data) {
	startUptime := float64(exProfileStart - exJVMStart)
	entry := int64((startUptime + exPauseOffsetMs) * 1e6)
	inv, _ := d.Long("sun.gc.collector.0.invocations")
	tm, _ := d.Long("sun.gc.collector.0.time")
	d.SetLong("sun.gc.collector.0.invocations", inv+1)
	d.SetLong("sun.gc.collector.0.time", tm+20_052_215)
	d.SetLong("sun.gc.collector.0.lastEntryTime", entry)
	d.SetLong("sun.gc.collector.0.lastExitTime", entry+20_052_215)
	sp, _ := d.Long("sun.rt.safepoints")
	st, _ := d.Long("sun.rt.safepointTime")
	d.SetLong("sun.rt.safepoints", sp+3)
	d.SetLong("sun.rt.safepointTime", st+21_408_000)
}

// profileExamples runs profiles through the real handlers and the profile
// manager, with fake attach (collapsed output from testdata), fake clock and
// fake hsperfdata changes.
func profileExamples(t *testing.T) map[string]any {
	ex := map[string]any{}
	base := "/api/v1/servers/" + exUUID + "/profiles"

	// wall, 10 s, with one collection.
	s := testServer(t)
	f, c := fakes(s)
	c.Set(time.UnixMilli(exProfileStart))
	ex["profiles-empty"] = getAt(t, s, base, 200)
	ex["profile-created"] = post(t, s, base, `{"seconds": 10}`, 202)
	c.Set(time.UnixMilli(exProfileStart + 4210))
	ex["profile-running"] = getAt(t, s, base+"/"+exProfileID, 200)
	ex["profiles"] = getAt(t, s, base, 200)
	f.mu.Lock()
	f.hsperfMod = gcDuringProfile
	f.mu.Unlock()
	fire(t, f.timers.poll)
	c.Set(time.UnixMilli(exProfileStart + 10_000))
	fire(t, f.timers.end)
	waitState(t, s, exUUID, exProfileID, profile.StateDone)
	ex["profile-done"] = getAt(t, s, base+"/"+exProfileID, 200)

	// cpu on a paused (idle) server: nothing sampled.
	s = testServer(t)
	f, c = fakes(s)
	f.collapsed, f.cpuStep, f.stopTook = "", 1, 1932*time.Microsecond
	c.Set(time.UnixMilli(exProfileStart))
	post(t, s, base, `{"seconds": 10, "mode": "cpu"}`, 202)
	c.Set(time.UnixMilli(exProfileStart + 10_000))
	fire(t, f.timers.end)
	waitState(t, s, exUUID, exProfileID, profile.StateDone)
	ex["profile-done-no-samples"] = getAt(t, s, base+"/"+exProfileID, 200)

	// The server stops during the profile.
	s = testServer(t)
	f, c = fakes(s)
	c.Set(time.UnixMilli(exProfileStart))
	post(t, s, base, `{"seconds": 10}`, 202)
	stopped := exContainer()
	stopped.State, stopped.Running, stopped.PID = "exited", false, 0
	s.Docker.(fakeDocker)[exUUID] = stopped
	f.mu.Lock()
	f.hsperfErr = os.ErrNotExist
	f.mu.Unlock()
	c.Set(time.UnixMilli(exProfileStart + 6532))
	fire(t, f.timers.poll)
	waitState(t, s, exUUID, exProfileID, profile.StateFailed)
	ex["profile-failed"] = getAt(t, s, base+"/"+exProfileID, 200)
	return ex
}

// getAt is get without resetting the clock.
func getAt(t *testing.T, s *Server, path string, status int) json.RawMessage {
	t.Helper()
	w := do(t, s, http.MethodGet, path, testToken)
	if w.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", path, w.Code, status, w.Body)
	}
	return json.RawMessage(bytes.TrimSpace(w.Body.Bytes()))
}

func marshalExample(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// TestExamples keeps docs/examples in sync with the response types and the
// fixtures. Run with -update after an intended change of the contract.
func TestExamples(t *testing.T) {
	ex := examples(t)
	if *update {
		old, _ := filepath.Glob(filepath.Join(examplesDir, "*.json"))
		for _, f := range old {
			os.Remove(f)
		}
		if err := os.MkdirAll(examplesDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, v := range ex {
		path := filepath.Join(examplesDir, name+".json")
		got := marshalExample(v)
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (run with -update)", name, err)
			continue
		}
		if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
			t.Errorf("%s.json is out of date (run with -update)", name)
		}
	}
	files, _ := filepath.Glob(filepath.Join(examplesDir, "*.json"))
	if len(files) != len(ex) && !*update {
		t.Errorf("docs/examples has %d files, expected %d", len(files), len(ex))
	}
}

// equalJSON compares two JSON documents semantically.
func equalJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}
