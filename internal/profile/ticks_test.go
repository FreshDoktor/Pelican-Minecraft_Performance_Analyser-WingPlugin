package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

// fixtureCollapsed turns the Server thread of a profile fixture back into
// collapsed output.
func fixtureCollapsed(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Result struct {
			Threads []struct {
				Name   string
				Stacks []struct {
					Stack string
					Count int64
				}
			}
		}
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, th := range p.Result.Threads {
		for _, s := range th.Stacks {
			fmt.Fprintf(&sb, "[%s tid=60];%s %d\n", th.Name, s.Stack, s.Count)
		}
	}
	return sb.String()
}

func TestEvalSamplesFixtures(t *testing.T) {
	cases := []struct {
		file       string
		mapping    string
		samples    int64
		load       float64
		overloaded bool
	}{
		// Fabric without players: 520 of 599 samples idle.
		{"profile-fabric-idle.json", "intermediary", 599, 79.0 / 599, false},
		// Immersive Snow waits for chunks inside the tick: 28 of 1200 idle.
		{"profile-fabric-immersive-snow-serial.json", "intermediary", 1200, 1172.0 / 1200, true},
		{"profile-fabric-immersive-snow-g1.json", "intermediary", 1200, 1148.0 / 1200, true},
		// Forge 1.12 modpack, Thread.sleep in MinecraftServer.run: 17 of 1196 idle.
		{"profile-forge-1.12-modpack.json", "mcp", 1196, 1179.0 / 1196, true},
	}
	for _, c := range cases {
		w, mapping, classOK := evalSamples(fixtureCollapsed(t, c.file), 60)
		if mapping != c.mapping || !classOK || w.Samples != c.samples || w.Load == nil ||
			math.Abs(*w.Load-c.load) > 1e-9 || w.Overloaded != c.overloaded || w.State != TickStateOK {
			t.Errorf("%s: mapping %q classOK %v, window %+v (load %v)", c.file, mapping, classOK, w, deref(w.Load))
		}
		if !c.overloaded && (w.TPS == nil || *w.TPS != 20 || w.TickMs == nil || math.Abs(*w.TickMs-c.load*50) > 1e-9) {
			t.Errorf("%s: tps %v, tick %v", c.file, deref(w.TPS), deref(w.TickMs))
		}
		if c.overloaded && (w.TPS != nil || w.TickMs != nil) {
			t.Errorf("%s: overloaded with tps %v, tick %v", c.file, deref(w.TPS), deref(w.TickMs))
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

const (
	tRoot = "java/lang/Thread.run;java/lang/Thread.runWith;net/minecraft/server/MinecraftServer.runServer"
	tIdle = tRoot + ";net/minecraft/server/MinecraftServer.waitUntilNextTick;net/minecraft/server/MinecraftServer.waitForTasks;java/util/concurrent/locks/LockSupport.parkNanos"
	tTick = tRoot + ";net/minecraft/server/MinecraftServer.tickServer;net/minecraft/server/MinecraftServer.tickChildren;net/minecraft/server/level/ServerLevel.tick"
)

func samples(idle, tick int) string {
	var sb strings.Builder
	if idle > 0 {
		fmt.Fprintf(&sb, "[Server thread tid=60];%s %d\n", tIdle, idle)
	}
	if tick > 0 {
		fmt.Fprintf(&sb, "[Server thread tid=60];%s %d\n", tTick, tick)
	}
	return sb.String()
}

func calls(n int) string {
	return fmt.Sprintf("[Server thread tid=60];%s;net/minecraft/server/MinecraftServer.tickServer %d\n", tRoot, n)
}

func TestEvalSamples(t *testing.T) {
	w, mapping, _ := evalSamples(samples(250, 0), 60)
	if w.State != TickStateIdle || mapping != "mojmap" || w.TPS != nil {
		t.Errorf("all idle: %+v", w)
	}
	// Stacks of other threads (no filter) do not count.
	w, _, _ = evalSamples(samples(200, 50)+"[Worker tid=61];"+tTick+" 999\n", 60)
	if w.Samples != 250 || *w.TickMs != 10 {
		t.Errorf("other thread: %+v", w)
	}
	// Obfuscated vanilla: MinecraftServer frames, but no known method.
	w, mapping, _ = evalSamples("[Server thread tid=60];java/lang/Thread.run;net/minecraft/server/MinecraftServer.w;net/minecraft/server/MinecraftServer.a 250\n", 60)
	if w.State != TickStateUnsupported || mapping != "" || w.Load != nil {
		t.Errorf("obfuscated: %+v", w)
	}
	if w, _, _ = evalSamples("", 60); w.Samples != 0 {
		t.Errorf("empty: %+v", w)
	}
}

// tickLoader fakes async-profiler for the tick monitor: commands with a
// file= get the output of out(options) written there.
type tickLoader struct {
	mu   sync.Mutex
	opts []string
	out  func(options string) string
	err  func(options string) error
}

func (l *tickLoader) Load(_ context.Context, t attach.Target, _, options string) error {
	l.mu.Lock()
	l.opts = append(l.opts, options)
	out, errf := l.out, l.err
	l.mu.Unlock()
	if errf != nil {
		if err := errf(options); err != nil {
			return err
		}
	}
	for _, o := range strings.Split(options, ",") {
		if f, ok := strings.CutPrefix(o, "file=/tmp/"); ok && out != nil {
			os.WriteFile(filepath.Join(t.Tmp.Root, f), []byte(out(options)), 0o644)
		}
	}
	return nil
}

func (l *tickLoader) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.opts...)
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newTicks(t *testing.T) (*Ticks, *tickLoader, *testClock, Target) {
	t.Helper()
	l := &tickLoader{}
	c := &testClock{t: time.UnixMilli(1792930000000)}
	m := &Manager{MaxConcurrent: 2, Lib: []byte("library v1"), Loader: l, ProcRoot: t.TempDir(), Now: c.Now,
		FreeBytes: func(ctmp.Dir) (int64, bool) { return 1 << 30, true }}
	k := &Ticks{Max: 2, Profiles: m, Now: c.Now, NewID: func() string { return "00000000000000aa" },
		ThreadID: func(int, string) (int, bool, error) { return 60, true, nil },
		Every:    func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }}
	m.Yield = k.Yield
	tg := Target{ServerUUID: "s1", Attach: attach.Target{HostPID: 4242, NSPID: 26, Tmp: ctmp.Dir{Root: t.TempDir()}},
		Hsperf: ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"}, JVMStartedAt: 1792866595712}
	return k, l, c, tg
}

func (k *Ticks) monitorOf(uuid string) *monitor {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.mons[uuid]
}

func lastWindow(t *testing.T, k *Ticks, tg Target) TickWindowResult {
	t.Helper()
	s, err := k.Get(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) == 0 {
		t.Fatal("no window")
	}
	return s.Windows[len(s.Windows)-1]
}

func TestTicksModes(t *testing.T) {
	k, l, c, tg := newTicks(t)
	var mu sync.Mutex
	window, total := samples(200, 50), 0
	var run, wait int64 // ns of the Server thread
	schedErr := error(nil)
	l.out = func(o string) string {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(o, "dump") {
			return calls(total)
		}
		return window
	}
	lookups := 0
	k.HostTID = func(pid, tid int) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		lookups++
		if pid != 4242 || tid != 60 {
			return 0, container.ErrNoThread
		}
		return 4134, nil
	}
	k.SchedStat = func(pid, tid int) (int64, int64, error) {
		mu.Lock()
		defer mu.Unlock()
		if tid != 4134 {
			return 0, 0, errors.New("wrong thread")
		}
		return run, wait, schedErr
	}
	// set adds calls of tickServer and times of the Server thread (ms).
	set := func(n int, runMs, waitMs float64) {
		mu.Lock()
		total += n
		run += int64(runMs * 1e6)
		wait += int64(waitMs * 1e6)
		mu.Unlock()
	}

	s, err := k.Get(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != TickStateStarting || s.Reason != TickReasonFirstWindow || s.Method != TickMethodSampling {
		t.Fatalf("first: %+v", s)
	}
	if b, _ := os.ReadFile(filepath.Join(tg.Attach.Tmp.Root, LibName)); string(b) != "library v1" {
		t.Errorf("library %q", b)
	}
	m := k.monitorOf("s1")
	step := func() TickWindowResult {
		t.Helper()
		c.Add(TickWindow)
		if !m.step() {
			t.Fatal("monitor ended")
		}
		return lastWindow(t, k, tg)
	}

	// The first window samples and finds the mapping, then the monitor counts.
	w := step()
	if w.Method != TickMethodSampling || w.State != TickStateOK || *w.TPS != 20 || *w.TickMs != 10 || w.Samples != 250 || w.EndedAt.Sub(w.StartedAt) != TickWindow {
		t.Errorf("sampling: %+v", w)
	}
	sampling := "start,event=wall,interval=20ms,filter=60,threads,cstack=no,log=/tmp/mpa-ticks-00000000000000aa.log"
	counting := "start,event=net.minecraft.server.MinecraftServer.tickServer,interval=1,threads,log=/tmp/mpa-ticks-00000000000000aa.log"
	if got := l.calls(); len(got) != 3 || got[0] != sampling || !strings.HasPrefix(got[1], "stop,collapsed,file=/tmp/mpa-ticks-00000000000000aa.collapsed,") || got[2] != counting {
		t.Errorf("commands %q", got)
	}
	if s, _ := k.Get(context.Background(), tg); s.Method != TickMethodCounting || s.Mapping != "mojmap" {
		t.Errorf("method %q", s.Method)
	}

	// Keeps up: 100 ticks, 440 ms busy (400 ran, 40 waited for a CPU).
	set(100, 400, 40)
	w = step()
	if w.Method != TickMethodCounting || w.State != TickStateOK || w.Ticks != 100 || *w.TPS != 20 ||
		math.Abs(*w.TickMs-4.4) > 1e-9 || math.Abs(*w.Load-0.088) > 1e-9 || w.Overloaded || w.Samples != 0 {
		t.Errorf("counting: %+v (tick %v, load %v)", w, deref(w.TickMs), deref(w.Load))
	}
	// Falls behind: 71 ticks, the thread never rests. A tick takes its share of the window.
	set(71, 4600, 400)
	w = step()
	if w.Ticks != 71 || *w.TPS != 14.2 || math.Abs(*w.TickMs-5000.0/71) > 1e-9 || *w.Load != 1 || !w.Overloaded {
		t.Errorf("overloaded: %+v", w)
	}
	// Catches up, and stays counting: no command but dumps.
	n := len(l.calls())
	set(110, 2000, 0)
	w = step()
	if w.Ticks != 110 || *w.TPS != 22 || w.Overloaded || math.Abs(*w.TickMs-2000.0/110) > 1e-9 {
		t.Errorf("catching up: %+v", w)
	}
	for _, o := range l.calls()[n:] {
		if !strings.HasPrefix(o, "dump,") {
			t.Errorf("command while counting: %q", o)
		}
	}
	// Idle (nobody online, or pause-when-empty-seconds): ticks without work.
	set(100, 10, 0)
	if w = step(); w.State != TickStateIdle || *w.TPS != 20 || math.Abs(*w.TickMs-0.1) > 1e-9 || w.Overloaded {
		t.Errorf("idle: %+v", w)
	}
	if s, _ := k.Get(context.Background(), tg); s.State != TickStateIdle {
		t.Errorf("state %s", s.State)
	}
	set(0, 1, 0)
	if w = step(); w.State != TickStateIdle || w.TPS != nil || w.TickMs != nil || w.Overloaded {
		t.Errorf("idle without ticks: %+v", w)
	}
	// Without schedstat: TPS only, the thread is looked up again.
	mu.Lock()
	schedErr = errors.New("gone")
	mu.Unlock()
	set(100, 400, 0)
	if w = step(); w.State != TickStateOK || *w.TPS != 20 || w.TickMs != nil || w.Load != nil {
		t.Errorf("without schedstat: %+v", w)
	}
	mu.Lock()
	schedErr, before := nil, lookups
	mu.Unlock()
	set(100, 400, 0)
	step()
	set(100, 300, 0)
	if w = step(); w.TickMs == nil || math.Abs(*w.TickMs-3) > 1e-9 || lookups != before+1 {
		t.Errorf("schedstat back: %+v, %d lookups", w, lookups-before)
	}

	// Ends after a minute without Get.
	c.Add(TickIdleTimeout + TickWindow)
	if m.step() {
		t.Error("idle monitor did not end")
	}
	m.shutdown()
	if k.monitorOf("s1") != nil {
		t.Error("monitor still listed")
	}
	if got := l.calls(); !strings.HasPrefix(got[len(got)-1], "stop,") {
		t.Errorf("not stopped: %q", got[len(got)-1])
	}
	entries, _ := os.ReadDir(tg.Attach.Tmp.Root)
	for _, e := range entries {
		if e.Name() != LibName {
			t.Errorf("left in /tmp: %s", e.Name())
		}
	}
	if len(k.monitorOf("s1").windowsCopy()) != 0 {
		t.Error("history kept")
	}
}

func TestEvalCountingSafepoints(t *testing.T) {
	b := func(run, sp float64) busySample { return busySample{ok: true, run: run, safepoint: sp} }
	// 1 s of safepoints in a window where the thread worked half of the rest: half of them hurt a tick.
	w := evalCounting(100, TickWindow, b(0, 0), b(2000, 1000))
	if math.Abs(*w.Load-0.5) > 1e-9 || math.Abs(*w.TickMs-25) > 1e-9 {
		t.Errorf("safepoints: load %v, tick %v", deref(w.Load), deref(w.TickMs))
	}
	// Safepoints unknown: only the thread's own times.
	w = evalCounting(100, TickWindow, b(0, -1), b(2000, -1))
	if math.Abs(*w.Load-0.4) > 1e-9 {
		t.Errorf("no safepoints: load %v", deref(w.Load))
	}
	// A new thread (times went back): no load.
	if w = evalCounting(100, TickWindow, b(3000, 0), b(10, 0)); w.Load != nil || w.TickMs != nil || *w.TPS != 20 {
		t.Errorf("times back: %+v", w)
	}
	if w = evalCounting(100, 0, b(0, 0), b(10, 0)); w.State != "" {
		t.Errorf("empty window: %+v", w)
	}
}

func (m *monitor) windowsCopy() []TickWindowResult {
	if m == nil {
		return nil
	}
	m.k.mu.Lock()
	defer m.k.mu.Unlock()
	return append([]TickWindowResult{}, m.windows...)
}

func TestTicksYield(t *testing.T) {
	k, l, c, tg := newTicks(t)
	l.out = func(string) string { return samples(225, 25) }
	if _, err := k.Get(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	m := k.monitorOf("s1")
	c.Add(TickWindow)
	m.step()

	release := k.Yield(context.Background(), 4242)
	s, _ := k.Get(context.Background(), tg)
	if s.State != TickStateSuspended || s.Reason != TickReasonProfileRunning || s.Method != "" || m.active {
		t.Errorf("suspended: %+v, active %v", s, m.active)
	}
	if got := l.calls(); !strings.HasPrefix(got[len(got)-1], "stop,") {
		t.Errorf("not stopped: %q", got[len(got)-1])
	}
	n := len(l.calls())
	c.Add(TickWindow)
	m.step()
	if w := lastWindow(t, k, tg); w.State != TickStateSuspended || w.Method != "" {
		t.Errorf("suspended window: %+v", w)
	}
	if len(l.calls()) != n {
		t.Errorf("commands while suspended: %q", l.calls()[n:])
	}
	release()
	release() // once
	c.Add(TickWindow)
	m.step()
	// The mapping is known: counting right away.
	if s, _ := k.Get(context.Background(), tg); s.State != TickStateStarting || s.Method != TickMethodCounting {
		t.Errorf("resumed: %+v", s)
	}
	c.Add(TickWindow)
	m.step()
	if w := lastWindow(t, k, tg); w.State != TickStateOK || w.Method != TickMethodCounting || w.Ticks != 250 {
		t.Errorf("after resume: %+v", w)
	}

	// A new monitor while a profile runs starts suspended, without commands.
	release = k.Yield(context.Background(), 5000)
	tg2 := tg
	tg2.ServerUUID, tg2.Attach.HostPID = "s2", 5000
	n = len(l.calls())
	if s, err := k.Get(context.Background(), tg2); err != nil || s.State != TickStateSuspended {
		t.Errorf("new while suspended: %+v, %v", s, err)
	}
	if len(l.calls()) != n {
		t.Errorf("commands: %q", l.calls()[n:])
	}
	release()
	// Max 2 monitors.
	tg3 := tg
	tg3.ServerUUID = "s3"
	if _, err := k.Get(context.Background(), tg3); !errors.Is(err, ErrTooManyMonitors) {
		t.Errorf("third monitor: %v", err)
	}
}

func TestTicksProfileYields(t *testing.T) {
	k, l, _, tg := newTicks(t)
	l.out = func(string) string { return samples(225, 25) }
	m := k.Profiles
	m.ReadHsperf = nil
	m.init()
	timers := make(chan time.Time)
	m.After = func(time.Duration) <-chan time.Time { return timers }
	m.Poll = func() (<-chan time.Time, func()) { return nil, func() {} }
	if _, err := k.Get(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	run, err := m.Start(context.Background(), tg, Request{Mode: ModeWall, Seconds: 5, IntervalMs: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := l.calls()
	if !strings.HasPrefix(got[len(got)-2], "stop,collapsed,file=/tmp/mpa-ticks-") || !strings.HasPrefix(got[len(got)-1], "start,event=wall,interval=50ms") {
		t.Errorf("profile start: %q", got)
	}
	if s, _ := k.Get(context.Background(), tg); s.State != TickStateSuspended {
		t.Errorf("state %s", s.State)
	}
	timers <- time.Time{}
	for i := 0; i < 200; i++ {
		if r, _ := m.Get("s1", run.ID); r.State == StateDone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	k.mu.Lock()
	y := k.yields[4242]
	k.mu.Unlock()
	if y != 0 {
		t.Errorf("not released: %d", y)
	}
}

func TestTicksNoServerThread(t *testing.T) {
	k, l, c, tg := newTicks(t)
	l.out = func(string) string { return samples(225, 25) }
	named := true
	k.ThreadID = func(int, string) (int, bool, error) { return 0, named, container.ErrNoThread }
	dumps := 0
	k.Jcmd = func(context.Context, attach.Target, string) (string, error) {
		dumps++
		b, err := os.ReadFile("../../testdata/mpa-threads.txt")
		return string(b), err
	}
	s, err := k.Get(context.Background(), tg)
	if err != nil || s.State != TickStateUnsupported || s.Reason != TickReasonNoServerThread || len(l.calls()) != 0 || dumps != 0 {
		t.Fatalf("proxy: %+v, %v, %q, %d dumps", s, err, l.calls(), dumps)
	}
	// Java 8: no native thread names, the thread dump tells the id (nid=60).
	named = false
	m := k.monitorOf("s1")
	c.Add(TickWindow)
	m.step()
	if s, _ := k.Get(context.Background(), tg); s.State != TickStateStarting || m.tid != 60 || dumps != 1 {
		t.Errorf("java 8: %+v, tid %d, %d dumps", s, m.tid, dumps)
	}
}

func TestTicksErrors(t *testing.T) {
	k, l, c, tg := newTicks(t)
	l.err = func(o string) error {
		if strings.HasPrefix(o, "start") {
			return &attach.CommandError{Code: 100, Message: "Could not start"}
		}
		return nil
	}
	if _, err := k.Get(context.Background(), tg); err == nil || k.monitorOf("s1") != nil {
		t.Fatalf("start failure: %v", err)
	}
	k.Profiles.FreeBytes = func(ctmp.Dir) (int64, bool) { return 1 << 20, true }
	var te *TmpSpaceError
	if _, err := k.Get(context.Background(), tg); !errors.As(err, &te) {
		t.Fatalf("tmp space: %v", err)
	}
	k.Profiles.FreeBytes = func(ctmp.Dir) (int64, bool) { return 1 << 30, true }

	// A failing window: state error, the next window starts again.
	l.err = nil
	l.out = func(string) string { return samples(225, 25) }
	if _, err := k.Get(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	m := k.monitorOf("s1")
	l.err = func(o string) error {
		if strings.HasPrefix(o, "stop") {
			return attach.ErrTimeout
		}
		return nil
	}
	c.Add(TickWindow)
	if !m.step() {
		t.Fatal("ended")
	}
	s, _ := k.Get(context.Background(), tg)
	if s.State != TickStateError || s.Err == nil || s.Err.Code != CodeAttachTimeout {
		t.Errorf("error: %+v", s)
	}
	l.err = nil
	c.Add(TickWindow)
	m.step()
	if s, _ := k.Get(context.Background(), tg); s.State != TickStateStarting || s.Err != nil || s.Method != TickMethodSampling {
		t.Errorf("restarted: %+v", s)
	}
	// The JVM is gone: the monitor ends.
	l.err = func(string) error { return attach.ErrTimeout }
	m.target.Hsperf = ctmp.File{Dir: ctmp.Dir{Root: t.TempDir()}, Name: "gone"}
	c.Add(TickWindow)
	if m.step() {
		t.Error("monitor of a gone JVM did not end")
	}
}

func TestTicksNewJVM(t *testing.T) {
	k, l, _, tg := newTicks(t)
	l.out = func(string) string { return samples(225, 25) }
	if _, err := k.Get(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	old := k.monitorOf("s1")
	tg.JVMStartedAt++
	if _, err := k.Get(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if m := k.monitorOf("s1"); m == old || !old.closed {
		t.Error("restarted JVM kept the old monitor")
	}
	select {
	case <-old.quit:
	default:
		t.Error("old monitor not told to quit")
	}
}
