package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/parse"
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
	return container.JavaProcess{HostPID: 2817346, NSPID: 26, User: "container", Hsperf: "x"}
}

func loadFixture(t *testing.T) *hsperf.Data {
	t.Helper()
	d, err := hsperf.ReadFile("../../testdata/mpa-hsperf.bin")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func exFeatures() []string {
	return []string{FeatureStatus, FeatureVM, FeatureMetrics, FeatureThreads, FeatureHistogram, FeatureVMFlags, FeatureLiveHeap, FeatureProfiles}
}

// Feature lists of the milestones, for node-status-m1/-m2.
var (
	featuresM1 = []string{FeatureStatus, FeatureVM, FeatureMetrics}
	featuresM2 = []string{FeatureStatus, FeatureVM, FeatureMetrics, FeatureThreads, FeatureHistogram, FeatureVMFlags, FeatureLiveHeap}
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
	young.SocketExists = func(string) bool { return false }
	_, c := fakes(young)
	c.Set(time.UnixMilli(1792866595712 + 12_000))
	ex["vm-without-flags"] = get(t, young, base+exUUID+"/vm", 200)

	for k, v := range profileExamples(t) {
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
	CodeInsufficientTmpSpace: {"free_bytes": 1048576, "required_bytes": 8388608},
}

func profileExamples(t *testing.T) map[string]any {
	raw, err := os.ReadFile("testdata/profile-wall.collapsed")
	if err != nil {
		t.Fatal(err)
	}
	threads, total := BuildProfileThreads(parse.ParseCollapsed(string(raw)))
	interval := 50
	base := Profile{
		ID: exProfileID, ServerUUID: exUUID, Mode: "wall", Seconds: 10, IntervalMs: &interval,
		State: ProfileRunning, StartedAt: exProfileStart, EndsAt: exProfileStart + 10_000,
	}
	ex := map[string]any{}
	created := base
	ex["profile-created"] = created

	running := base
	running.ElapsedMs, running.Progress = 4210, 0.421
	ex["profile-running"] = running
	ex["profiles"] = ProfileList{Profiles: []Profile{running}}

	young := true
	full := true
	done := base
	done.State = ProfileDone
	done.FinishedAt = ptr(exProfileStart + 10_318)
	done.ElapsedMs, done.Progress = 10_318, 1
	done.ExpiresAt = ptr(*done.FinishedAt + 15*60*1000)
	done.Result = &ProfileResult{
		DurationMs: 10004.871, Unit: UnitSamples, IntervalMs: &interval, Total: total, Warnings: []string{},
		Threads: threads,
		GC: ProfileGC{
			Count: 1, TimeMs: 20.052,
			Collectors: []ProfileCollector{
				{Index: 0, Name: "Serial young collection pauses", Kind: hsperf.KindYoung, TimeIsPause: &young, Count: 1, TimeMs: 20.052},
				{Index: 1, Name: "Serial full collection pauses", Kind: hsperf.KindFull, TimeIsPause: &full, Count: 0, TimeMs: 0},
			},
			Pauses:         []GCPause{{CollectorIndex: 0, Kind: hsperf.KindYoung, StartOffsetMs: 4183.211, DurationMs: 20.052}},
			PausesComplete: true,
		},
		Safepoints: ProfileSafepoints{Count: 3, TimeMs: 21.408},
		CPU:        ProfileCPU{ProcessMs: 41.2, ProcessPercent: 0.412},
	}
	ex["profile-done"] = done

	cpuInterval := 10
	idle := base
	idle.Mode, idle.IntervalMs = "cpu", &cpuInterval
	idle.State = ProfileDone
	idle.FinishedAt = ptr(exProfileStart + 10_204)
	idle.ElapsedMs, idle.Progress = 10_204, 1
	idle.ExpiresAt = ptr(*idle.FinishedAt + 15*60*1000)
	idle.Result = &ProfileResult{
		DurationMs: 10001.932, Unit: UnitSamples, IntervalMs: &cpuInterval, Total: 0,
		Warnings: []string{WarningNoSamples}, Threads: []ProfileThread{},
		GC: ProfileGC{Collectors: []ProfileCollector{
			{Index: 0, Name: "Serial young collection pauses", Kind: hsperf.KindYoung, TimeIsPause: &young},
			{Index: 1, Name: "Serial full collection pauses", Kind: hsperf.KindFull, TimeIsPause: &full},
		}, Pauses: []GCPause{}, PausesComplete: true},
		CPU: ProfileCPU{ProcessMs: 7, ProcessPercent: 0.07},
	}
	ex["profile-done-no-samples"] = idle

	failed := base
	failed.State = ProfileFailed
	failed.FinishedAt = ptr(exProfileStart + 6_532)
	failed.ElapsedMs, failed.Progress = 6_532, 0.653
	failed.ExpiresAt = ptr(*failed.FinishedAt + 15*60*1000)
	e := newError(CodeServerNotRunning, "The server stopped during the profile.", map[string]any{"container_state": "exited"}).body
	failed.Error = &e
	ex["profile-failed"] = failed
	return ex
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
