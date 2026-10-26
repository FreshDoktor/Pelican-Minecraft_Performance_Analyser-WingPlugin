package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

// Stacks of the Server thread in the tick examples, from the thread dump
// fixture (mojmap); the tick work below tickChildren is illustrative.
const (
	exTickRoot = "java/lang/Thread.run;java/lang/Thread.runWith;net/minecraft/server/MinecraftServer$$Lambda.0x0000000010f1aa00.run;" +
		"net/minecraft/server/MinecraftServer.lambda$spin$0;net/minecraft/server/MinecraftServer.runServer"
	exTickIdle = exTickRoot + ";net/minecraft/server/MinecraftServer.waitUntilNextTick;net/minecraft/util/thread/BlockableEventLoop.managedBlock;" +
		"net/minecraft/server/MinecraftServer.waitForTasks;java/util/concurrent/locks/LockSupport.parkNanos;jdk/internal/misc/Unsafe.park"
	exTickWork = exTickRoot + ";net/minecraft/server/MinecraftServer.tickServer;net/minecraft/server/MinecraftServer.tickChildren;" +
		"net/minecraft/server/level/ServerLevel.tick;net/minecraft/world/level/entity/EntityTickList.forEach"
)

// exTicks sets what the fake async-profiler returns to the tick monitor:
// wall samples of the Server thread (idle, work) for "stop", and the calls
// of tickServer counted so far for "dump".
func exTicks(f *fakeAttach, idle, work, calls int) {
	var sb strings.Builder
	if idle > 0 {
		fmt.Fprintf(&sb, "[Server thread tid=60];%s %d\n", exTickIdle, idle)
	}
	if work > 0 {
		fmt.Fprintf(&sb, "[Server thread tid=60];%s %d\n", exTickWork, work)
	}
	samples := sb.String()
	dump := fmt.Sprintf("[Server thread tid=60];%s;net/minecraft/server/MinecraftServer.tickServer %d\n", exTickRoot, calls)
	f.mu.Lock()
	f.tickOut = func(o string) string {
		if strings.HasPrefix(o, "dump") {
			return dump
		}
		return samples
	}
	f.mu.Unlock()
}

// exBusy lets the Server thread run and wait for a CPU (schedstat), in ms.
func exBusy(f *fakeAttach, runMs, waitMs float64) {
	f.mu.Lock()
	f.schedRun += int64(runMs * 1e6)
	f.schedWait += int64(waitMs * 1e6)
	f.mu.Unlock()
}

// tickWindow lets the tick monitor end one window (the ticker fires a window
// after the last restart, which took stopTook) and waits until the history
// has n windows.
func tickWindow(t *testing.T, s *Server, path string, n int) TicksResponse {
	t.Helper()
	f, c := fakes(s)
	c.Advance(profile.TickWindow - f.stopTook)
	fire(t, f.timers.tick)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var r TicksResponse
		if err := json.Unmarshal(getAt(t, s, path, 200), &r); err != nil {
			t.Fatal(err)
		}
		if len(r.History) >= n {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tick monitor: no window %d", n)
	return TicksResponse{}
}

// waitCounting waits until the monitor counts. After the first window it
// records the window before it starts counting and reads the times of the
// Server thread, so busy time the test adds earlier would be lost.
func waitCounting(t *testing.T, s *Server, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var r TicksResponse
		if err := json.Unmarshal(getAt(t, s, path, 200), &r); err != nil {
			t.Fatal(err)
		}
		if r.Method != nil && *r.Method == profile.TickMethodCounting {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("tick monitor: does not count")
}

// tickExamples runs the tick monitor through the real handlers with fake
// attach (wall samples and call counts), fake clock and fake ticker.
func tickExamples(t *testing.T) map[string]any {
	ex := map[string]any{}
	path := "/api/v1/servers/" + exUUID + "/ticks"

	// Vanilla, a few players: 4-5 ms per tick. The first window samples and
	// finds the mapping, from then on the monitor counts tickServer.
	s := testServer(t)
	f, c := fakes(s)
	c.Set(time.UnixMilli(exTickStart))
	exTicks(f, 229, 21, 0)
	ex["ticks-starting"] = getAt(t, s, path, 200)
	tickWindow(t, s, path, 1)
	waitCounting(t, s, path)
	exTicks(f, 0, 0, 100)
	exBusy(f, 430, 16)
	tickWindow(t, s, path, 2)
	exTicks(f, 0, 0, 200)
	exBusy(f, 470, 12)
	tickWindow(t, s, path, 3)
	ex["ticks"] = getAt(t, s, path, 200)

	// Idle (nobody online, or pause-when-empty-seconds): the Server thread only waits; tickServer is
	// still called, but does nothing.
	s = testServer(t)
	f, c = fakes(s)
	c.Set(time.UnixMilli(exTickStart))
	exTicks(f, 250, 0, 0)
	getAt(t, s, path, 200)
	tickWindow(t, s, path, 1)
	waitCounting(t, s, path)
	exTicks(f, 0, 0, 100)
	exBusy(f, 4, 1)
	tickWindow(t, s, path, 2)
	ex["ticks-idle"] = getAt(t, s, path, 200)

	// Overloaded: no idle time left in the first window, then about 70 ms per tick.
	s = testServer(t)
	f, c = fakes(s)
	c.Set(time.UnixMilli(exTickStart))
	exTicks(f, 0, 250, 0)
	getAt(t, s, path, 200)
	tickWindow(t, s, path, 1)
	waitCounting(t, s, path)
	exTicks(f, 0, 0, 71)
	exBusy(f, 4710, 262)
	tickWindow(t, s, path, 2)
	exTicks(f, 0, 0, 143)
	exBusy(f, 4795, 186)
	tickWindow(t, s, path, 3)
	ex["ticks-overloaded"] = getAt(t, s, path, 200)

	// A lag analysis takes the profiler: the monitor waits.
	s = testServer(t)
	f, c = fakes(s)
	c.Set(time.UnixMilli(exTickStart))
	exTicks(f, 229, 21, 0)
	getAt(t, s, path, 200)
	tickWindow(t, s, path, 1)
	post(t, s, "/api/v1/servers/"+exUUID+"/profiles", `{"seconds": 30}`, 202)
	tickWindow(t, s, path, 2)
	ex["ticks-suspended"] = getAt(t, s, path, 200)

	// A proxy: no Server thread.
	s = testServer(t)
	s.Ticks.ThreadID = func(int, string) (int, bool, error) { return 0, true, container.ErrNoThread }
	_, c = fakes(s)
	c.Set(time.UnixMilli(exTickStart))
	ex["ticks-unsupported"] = getAt(t, s, path, 200)

	// A JVM started 12 s ago without a running listener: no SIGQUIT yet.
	s = testServer(t)
	s.SocketExists = func(string) bool { return false }
	_, c = fakes(s)
	c.Set(time.UnixMilli(exJVMStart + 12_000))
	ex["ticks-jvm-starting"] = getAt(t, s, path, 200)
	return ex
}

func decodeTicks(t *testing.T, b []byte) TicksResponse {
	t.Helper()
	var r TicksResponse
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTicksExamples(t *testing.T) {
	ex := tickExamples(t)
	check := func(name string, ok bool) {
		t.Helper()
		if !ok {
			t.Errorf("%s: %s", name, ex[name])
		}
	}
	r := decodeTicks(t, ex["ticks-starting"].(json.RawMessage))
	check("ticks-starting", r.State == profile.TickStateStarting && *r.Reason == profile.TickReasonFirstWindow &&
		*r.Method == "sampling" && r.Current == nil && len(r.History) == 0 && r.Error == nil)
	r = decodeTicks(t, ex["ticks"].(json.RawMessage))
	h := r.History
	check("ticks", r.State == "ok" && r.Reason == nil && *r.Mapping == "mojmap" && *r.Method == "counting" && len(h) == 3 &&
		*h[0].Method == "sampling" && *h[0].TPS == 20 && *h[0].TickTimeMs == 4.2 && *h[0].Samples == 250 && h[0].Ticks == nil &&
		*h[1].Method == "counting" && *h[1].Ticks == 100 && *h[1].TPS == 20.02 && *h[1].TickTimeMs == 4.46 && *h[1].LoadPercent == 8.93 &&
		*h[2].Ticks == 100 && *h[2].TPS == 20 && *h[2].TickTimeMs == 4.82 && *h[2].LoadPercent == 9.64 && h[2].Samples == nil &&
		h[2].DurationMs == 5000 && !h[2].Overloaded)
	r = decodeTicks(t, ex["ticks-idle"].(json.RawMessage))
	h = r.History
	check("ticks-idle", r.State == "idle" && len(h) == 2 && h[0].TPS == nil && h[0].TickTimeMs == nil && *h[0].LoadPercent == 0 &&
		*h[1].Method == "counting" && *h[1].TPS == 20.02 && *h[1].TickTimeMs == 0.05 && *h[1].LoadPercent == 0.1 && !h[1].Overloaded)
	r = decodeTicks(t, ex["ticks-overloaded"].(json.RawMessage))
	h = r.History
	check("ticks-overloaded", len(h) == 3 && *r.Method == "counting" &&
		*h[0].Method == "sampling" && h[0].Overloaded && h[0].TPS == nil && *h[0].LoadPercent == 100 &&
		*h[1].Ticks == 71 && *h[1].TPS == 14.21 && h[1].Overloaded && *h[1].TickTimeMs == 70.354 && *h[1].LoadPercent == 99.54 &&
		*h[2].Ticks == 72 && *h[2].TPS == 14.4 && *h[2].TickTimeMs == 69.444 && *h[2].LoadPercent == 99.62)
	r = decodeTicks(t, ex["ticks-suspended"].(json.RawMessage))
	check("ticks-suspended", r.State == "suspended" && *r.Reason == "profile_running" && r.Method == nil &&
		len(r.History) == 2 && r.History[1].State == "suspended" && r.History[1].Method == nil)
	r = decodeTicks(t, ex["ticks-unsupported"].(json.RawMessage))
	check("ticks-unsupported", r.State == "unsupported" && *r.Reason == "no_server_thread" && r.Method == nil && r.MonitorStartedAt != nil)
	r = decodeTicks(t, ex["ticks-jvm-starting"].(json.RawMessage))
	check("ticks-jvm-starting", r.State == "starting" && *r.Reason == "jvm_starting" && r.MonitorStartedAt == nil)
}

func TestTicksErrors(t *testing.T) {
	path := func(uuid string) string { return "/api/v1/servers/" + uuid + "/ticks" }
	s := testServer(t)
	cases := []struct {
		uuid, code string
		status     int
	}{
		{uuidStopped, CodeServerNotRunning, 409},
		{uuidPaused, CodeServerNotRunning, 409},
		{uuidNoJava, CodeNoJavaProcess, 409},
		{uuidNoPerf, CodeHsperfUnavailable, 409},
		{uuidMissing, CodeServerNotFound, 404},
	}
	for _, c := range cases {
		w := do(t, s, http.MethodGet, path(c.uuid), testToken)
		var e ErrorBody
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != c.status || e.Error != c.code {
			t.Errorf("%s: %d %s", c.uuid, w.Code, w.Body)
		}
	}
	if w := do(t, s, http.MethodPost, path(uuidRunning), testToken); w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Errorf("POST: %d %s", w.Code, w.Body)
	}

	// -Xrs and no listener: never a signal.
	young := testServer(t)
	young.SocketExists = func(string) bool { return false }
	withVMArgs(t, young, "-Xmx1G -Xrs")
	if w := do(t, young, http.MethodGet, path(uuidRunning), testToken); w.Code != 409 || !strings.Contains(w.Body.String(), `"reason":"xrs"`) {
		t.Errorf("xrs: %d %s", w.Code, w.Body)
	}
	if f, _ := fakes(young); f.starts != 0 || len(f.loadCalls()) != 0 {
		t.Errorf("xrs: %d listener starts, loads %q", f.starts, f.loadCalls())
	}

	// Limit of monitors on the node.
	s = testServer(t)
	s.Ticks.Max = 1
	s.Limits.MaxTickMonitors = 1
	f, _ := fakes(s)
	exTicks(f, 229, 21, 0)
	if w := do(t, s, http.MethodGet, path(uuidRunning), testToken); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	other := exContainer()
	other.UUID, other.PID = "00000000-0000-4000-8000-000000000005", 5000
	s.Docker.(fakeDocker)[other.UUID] = other
	j := s.Proc.(fakeProc)[exContainer().PID]
	s.Proc.(fakeProc)[5000] = j
	w := do(t, s, http.MethodGet, path(other.UUID), testToken)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"error":"too_many_tick_monitors","message":"Too many tick monitors are running on this node.","details":{"limit":1}`) {
		t.Errorf("limit: %d %s", w.Code, w.Body)
	}

	// Without the feature.
	s = testServer(t)
	s.Ticks = nil
	if w := do(t, s, http.MethodGet, path(uuidRunning), testToken); w.Code != 501 {
		t.Errorf("not implemented: %d %s", w.Code, w.Body)
	}
}
