package api

import (
	"bytes"
	"encoding/json"
	"flag"
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

// examples builds every example document.
func examples(t *testing.T) map[string]any {
	d := loadFixture(t)
	ex := map[string]any{}

	ex["node-status"] = NodeStatusResponse{
		BridgeVersion: "0.1.0", APIVersion: APIVersion, Features: exFeatures(),
		StartedAt: exBridgeStart, UptimeMs: exNow - exBridgeStart, SampledAt: exNow, TLS: false,
		Limits: NodeLimits{MaxConcurrentProfiles: 2, MaxProfileSeconds: 120},
	}

	bridge := BridgeInfo{Version: "0.1.0", APIVersion: APIVersion, Features: exFeatures()}
	java := buildJavaProcess(exJava(), d, exNow)
	attach := buildAttach(d, true, java.UptimeMs)
	ex["status"] = StatusResponse{State: StateReady, Bridge: bridge, Server: serverInfo(exContainer()), Java: java, Attach: &attach, SampledAt: exNow}

	stopped := exContainer()
	stopped.State, stopped.Running, stopped.PID = "exited", false, 0
	ex["status-not-running"] = StatusResponse{State: CodeServerNotRunning, Bridge: bridge, Server: serverInfo(stopped), SampledAt: exNow}
	ex["status-no-java"] = StatusResponse{State: CodeNoJavaProcess, Bridge: bridge, Server: serverInfo(exContainer()), SampledAt: exNow}
	nohs := exJava()
	nohs.User, nohs.Hsperf = "", ""
	jn := buildJavaProcess(nohs, nil, exNow)
	an := buildAttach(nil, false, nil)
	ex["status-hsperf-unavailable"] = StatusResponse{State: CodeHsperfUnavailable, Bridge: bridge, Server: serverInfo(exContainer()), Java: jn, Attach: &an, SampledAt: exNow}

	vm := BuildVM(d, exContainer(), true, exHostCPUs, exTmpfs, exNow)
	ex["vm-without-flags"] = vm
	vm.Flags = exFlags()
	vm.FlagsError = nil
	ex["vm"] = vm

	m := BuildMetrics(d, exNow)
	ex["metrics"] = m
	m.LiveHeap = exLiveHeap()
	ex["metrics-live"] = m

	raw, err := os.ReadFile("../../testdata/mpa-threads.txt")
	if err != nil {
		t.Fatal(err)
	}
	// jcmd prints "<pid>:" first; the attach protocol does not.
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimPrefix(text, "26:\n")
	ex["threads"] = BuildThreads(parse.ParseThreadDump(text), text, 1792930183412, 18.204)

	hist, err := os.ReadFile("../../testdata/mpa-histogram-top300.txt")
	if err != nil {
		t.Fatal(err)
	}
	ex["histogram"] = BuildHistogram(parse.ParseHistogram(string(hist)), 100, 1792930190871, 187.316)

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

// exFlags is an illustrative VM.flags result for the fixture JVM (milestone 2).
func exFlags() map[string]string {
	return map[string]string{
		"CICompilerCount":         "2",
		"InitialHeapSize":         "134217728",
		"MaxHeapSize":             "1999634432",
		"MaxNewSize":              "666894336",
		"MinHeapDeltaBytes":       "196608",
		"MinHeapSize":             "134217728",
		"NewSize":                 "44695552",
		"NonNMethodCodeHeapSize":  "5832780",
		"NonProfiledCodeHeapSize": "122912730",
		"OldSize":                 "89522176",
		"ProfiledCodeHeapSize":    "122912730",
		"ReservedCodeCacheSize":   "251658240",
		"SegmentedCodeCache":      "true",
		"SoftMaxHeapSize":         "1999634432",
		"UseCompressedOops":       "true",
		"UseSerialGC":             "true",
	}
}

// exLiveHeap is an illustrative GC.heap_info result (milestone 2).
func exLiveHeap() *LiveHeap {
	raw := "DefNew     total 96704K, used 46125K [0x0000000088c00000, 0x000000008f2f0000, 0x00000000b0a00000)\n" +
		" eden space 86016K,  41% used [0x0000000088c00000, 0x000000008b4d4f10, 0x000000008e000000)\n" +
		" from space 10688K,  46% used [0x000000008e000000, 0x000000008e4ce070, 0x000000008ea70000)\n" +
		" to   space 10688K,   0% used [0x000000008ea70000, 0x000000008ea70000, 0x000000008f4e0000)\n" +
		"Tenured    total 500348K, used 500231K [0x00000000b0a00000, 0x00000000cf2df000, 0x0000000100000000)\n" +
		" the  space 500348K,  99% used [0x00000000b0a00000, 0x00000000cf2c1bd0, 0x00000000cf2df000)\n"
	return &LiveHeap{SampledAt: exNow, Source: SourceLive, UsedBytes: (46125 + 500231) * 1024,
		CommittedBytes: ptr(int64(96704+500348) * 1024), MaxBytes: nil, Raw: raw}
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
