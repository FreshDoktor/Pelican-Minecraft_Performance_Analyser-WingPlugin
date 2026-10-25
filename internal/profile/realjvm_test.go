package profile

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/assets"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
)

// TestRealJVMTicks runs the tick monitor against a running JVM on Linux, with
// a tick loop like MinecraftServer's whose work per tick (ms) is read from
// the file MPA_REAL_JVM_LOAD:
//
//	MPA_REAL_JVM_INIT=<pid> MPA_REAL_JVM_LOAD=<file> go test ./internal/profile -run RealJVMTicks -v
//
// It goes through 10, 30 and 70 ms per tick (the last is overloaded, about
// 14 TPS), runs a profile in between and checks the windows.
func TestRealJVMTicks(t *testing.T) {
	pid, _ := strconv.Atoi(os.Getenv("MPA_REAL_JVM_INIT"))
	load := os.Getenv("MPA_REAL_JVM_LOAD")
	if pid == 0 || load == "" {
		t.Skip("MPA_REAL_JVM_INIT or MPA_REAL_JVM_LOAD not set")
	}
	proc := container.Proc{Root: "/proc"}
	j, err := proc.FindJava(pid)
	if err != nil {
		t.Fatal(err)
	}
	d, err := hsperf.ReadFile(j.Hsperf)
	if err != nil {
		t.Fatal(err)
	}
	started, _ := d.Long("sun.rt.createVmBeginTime")
	client := attach.New(32 << 20)
	at := attach.Target{HostPID: j.HostPID, NSPID: j.NSPID, TmpDir: j.TmpDir}
	ctx := context.Background()
	if err := client.Start(ctx, at); err != nil {
		t.Fatal(err)
	}
	m := &Manager{MaxConcurrent: 1, Lib: assets.AsyncProfiler, Loader: client, CPUTicks: proc.CPUTicks}
	k := &Ticks{Max: 1, Profiles: m, ThreadID: proc.ThreadID, Jcmd: client.Jcmd}
	m.Yield = k.Yield
	tg := Target{ServerUUID: "test", Attach: at, Hsperf: j.Hsperf, JVMStartedAt: started}

	setLoad := func(ms int) {
		if err := os.WriteFile(load, []byte(strconv.Itoa(ms)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	last := func() TickSnapshot {
		s, err := k.Get(ctx, tg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	wait := func(windows int) TickSnapshot {
		n := len(last().Windows)
		deadline := time.Now().Add(time.Duration(windows+2) * TickWindow)
		for time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			if s := last(); len(s.Windows) >= n+windows {
				return s
			}
		}
		t.Fatalf("no %d new windows", windows)
		return TickSnapshot{}
	}
	logWindows := func(s TickSnapshot, from int) {
		for _, w := range s.Windows[from:] {
			f := func(p *float64) string {
				if p == nil {
					return "-"
				}
				return strconv.FormatFloat(*p, 'f', 2, 64)
			}
			t.Logf("%-8s %-9s %5.2fs samples=%d ticks=%d load=%s tps=%s tick_ms=%s overloaded=%v",
				w.State, w.Method, w.EndedAt.Sub(w.StartedAt).Seconds(), w.Samples, w.Ticks, f(w.Load), f(w.TPS), f(w.TickMs), w.Overloaded)
		}
	}

	setLoad(10)
	time.Sleep(time.Second)
	s := last()
	t.Logf("start: state %s/%s, method %s", s.State, s.Reason, s.Method)
	s = wait(2)
	logWindows(s, 0)
	w := s.Windows[len(s.Windows)-1]
	if s.Mapping != "mojmap" || w.TickMs == nil || *w.TickMs < 8 || *w.TickMs > 12 {
		t.Errorf("10 ms load: mapping %q, window %+v", s.Mapping, w)
	}

	setLoad(30)
	n := len(s.Windows)
	s = wait(2)
	logWindows(s, n)
	if w := s.Windows[len(s.Windows)-1]; w.TickMs == nil || *w.TickMs < 27 || *w.TickMs > 33 {
		t.Errorf("30 ms load: window %+v", w)
	}

	// A profile takes the JVM's profiler, the monitor waits and resumes.
	run, err := m.Start(ctx, tg, Request{Mode: ModeWall, Seconds: 6, IntervalMs: 50})
	if err != nil {
		t.Fatal("profile:", err)
	}
	if s := last(); s.State != TickStateSuspended {
		t.Errorf("during the profile: state %s", s.State)
	}
	time.Sleep(7 * time.Second)
	if r, _ := m.Get("test", run.ID); r.State != StateDone || r.Result.Total == 0 {
		t.Errorf("profile: state %s, result %+v", r.State, r.Result)
	}

	setLoad(70)
	n = len(last().Windows)
	s = wait(5)
	logWindows(s, n)
	w = s.Windows[len(s.Windows)-1]
	if w.Method != TickMethodCounting || w.TPS == nil || *w.TPS < 13 || *w.TPS > 15.5 {
		t.Errorf("70 ms load: window %+v", w)
	}

	setLoad(10)
	n = len(s.Windows)
	s = wait(3)
	logWindows(s, n)
	if w := s.Windows[len(s.Windows)-1]; w.Method != TickMethodSampling || w.TickMs == nil || *w.TickMs > 13 {
		t.Errorf("back to 10 ms: window %+v", w)
	}

	// Without Get the monitor stops after TickIdleTimeout; here directly.
	k.mu.Lock()
	mon := k.mons["test"]
	k.endLocked(mon)
	k.mu.Unlock()
	time.Sleep(time.Second)
	entries, _ := os.ReadDir(j.TmpDir)
	for _, e := range entries {
		if len(e.Name()) > 4 && e.Name()[:4] == "mpa-" && e.Name() != LibName {
			t.Errorf("left in /tmp: %s", e.Name())
		}
	}
}
