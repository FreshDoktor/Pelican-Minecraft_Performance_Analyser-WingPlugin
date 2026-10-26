package profile

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
)

type recLoader struct {
	mu   sync.Mutex
	opts []string
	errs []error // returned in order, then nil
}

func (l *recLoader) Load(_ context.Context, _ attach.Target, _, options string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.opts = append(l.opts, options)
	if len(l.errs) > 0 {
		e := l.errs[0]
		l.errs = l.errs[1:]
		return e
	}
	return nil
}

func newManager(t *testing.T) (*Manager, attach.Target) {
	t.Helper()
	m := &Manager{MaxConcurrent: 2, Lib: []byte("library v1"), Loader: &recLoader{}, ProcRoot: t.TempDir()}
	m.init()
	return m, attach.Target{HostPID: 4242, NSPID: 26, Tmp: ctmp.Dir{Root: t.TempDir()}}
}

func mapLib(t *testing.T, m *Manager, pid int) {
	t.Helper()
	dir := filepath.Join(m.ProcRoot, "4242")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "maps"), []byte("7f0000000000-7f0000100000 r-xp 00000000 00:2a 17 /tmp/mpa-libasyncProfiler.so\n"), 0o644)
}

func TestEnsureLib(t *testing.T) {
	m, tg := newManager(t)
	lib := filepath.Join(tg.Tmp.Root, LibName)
	if err := m.ensureLib(tg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(lib); string(b) != "library v1" {
		t.Fatalf("lib %q", b)
	}
	// A different file (e.g. planted by the container user) is replaced.
	os.WriteFile(lib, []byte("evil"), 0o644)
	if err := m.ensureLib(tg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(lib); string(b) != "library v1" {
		t.Errorf("stale lib not replaced: %q", b)
	}
	// A library mapped by the JVM is never touched (overwriting it crashes the JVM).
	os.WriteFile(lib, []byte("old version, loaded"), 0o644)
	mapLib(t, m, tg.HostPID)
	if err := m.ensureLib(tg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(lib); string(b) != "old version, loaded" {
		t.Errorf("mapped lib was modified: %q", b)
	}
}

func TestEnsureLibSymlink(t *testing.T) {
	m, tg := newManager(t)
	target := filepath.Join(t.TempDir(), "host-file")
	os.WriteFile(target, []byte("host"), 0o644)
	if err := os.Symlink(target, filepath.Join(tg.Tmp.Root, LibName)); err != nil {
		t.Skip("symlinks not available:", err)
	}
	if err := m.ensureLib(tg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "host" {
		t.Errorf("symlink target was written: %q", b)
	}
	if st, err := os.Lstat(filepath.Join(tg.Tmp.Root, LibName)); err != nil || !st.Mode().IsRegular() {
		t.Errorf("lib is not a regular file: %v", err)
	}
}

func TestOptions(t *testing.T) {
	const id = "0123456789abcdef"
	cases := map[string]string{
		startOptions(id, ModeWall, 50): "start,event=wall,interval=50ms,threads,log=/tmp/mpa-0123456789abcdef.log",
		startOptions(id, ModeCPU, 10):  "start,event=itimer,interval=10ms,threads,log=/tmp/mpa-0123456789abcdef.log",
		startOptions(id, ModeAlloc, 0): "start,alloc=524288,threads,log=/tmp/mpa-0123456789abcdef.log",
		stopOptions(id, ModeWall):      "stop,collapsed,log=/tmp/mpa-0123456789abcdef.log,file=/tmp/mpa-0123456789abcdef.collapsed",
		stopOptions(id, ModeAlloc):     "stop,collapsed,total,log=/tmp/mpa-0123456789abcdef.log,file=/tmp/mpa-0123456789abcdef.collapsed",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

func TestCleanup(t *testing.T) {
	m, tg := newManager(t)
	for _, n := range []string{"mpa-a.collapsed", "mpa-b.log", LibName, "keep.txt"} {
		os.WriteFile(filepath.Join(tg.Tmp.Root, n), []byte("x"), 0o644)
	}
	mapLib(t, m, tg.HostPID)
	m.Cleanup(context.Background(), tg, true)
	es, _ := os.ReadDir(tg.Tmp.Root)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "keep.txt,"+LibName {
		t.Errorf("left %v", names)
	}
	l := m.Loader.(*recLoader)
	if len(l.opts) != 1 || !strings.HasPrefix(l.opts[0], "stop,collapsed,") {
		t.Errorf("a loaded profiler must be stopped: %v", l.opts)
	}
	// Without listener no attach at all.
	m.Cleanup(context.Background(), tg, false)
	if len(l.opts) != 1 {
		t.Errorf("no attach without listener: %v", l.opts)
	}
}

func TestStartRetriesLeftoverProfiler(t *testing.T) {
	m, tg := newManager(t)
	l := &recLoader{errs: []error{&attach.CommandError{Code: 200, Message: "return code: 200"}}}
	m.Loader = l
	// The agent logs why it failed.
	m.NewID = func() string {
		os.WriteFile(filepath.Join(tg.Tmp.Root, "mpa-00000000000000aa.log"), []byte("[ERROR] Profiler already started\n"), 0o644)
		return "00000000000000aa"
	}
	m.Poll = func() (<-chan time.Time, func()) { return nil, func() {} }
	m.After = func(time.Duration) <-chan time.Time { return nil }
	r, err := m.Start(context.Background(), Target{ServerUUID: "u", Attach: tg, Hsperf: ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"}}, Request{Mode: ModeWall, Seconds: 1, IntervalMs: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.opts) != 3 || !strings.HasPrefix(l.opts[1], "stop,") || !strings.HasPrefix(l.opts[2], "start,") || r.State != StateRunning {
		t.Errorf("opts %v state %s", l.opts, r.State)
	}
	m.Delete("u", r.ID)
}

// finishedRun is a done profile of uuid that finished at fin.
func finishedRun(id, uuid string, fin time.Time) *run {
	return &run{Run: Run{ID: id, ServerUUID: uuid, State: StateDone, StartedAt: fin.Add(-10 * time.Second),
		FinishedAt: fin, ExpiresAt: fin.Add(Retention), Result: &Result{}}, done: make(chan struct{})}
}

func runIDs(m *Manager) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id := range m.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func TestStartDropsOldestFinished(t *testing.T) {
	m, tg := newManager(t)
	now := time.Unix(1_700_000_000, 0)
	m.Now = func() time.Time { return now }
	m.Poll = func() (<-chan time.Time, func()) { return nil, func() {} }
	m.After = func(time.Duration) <-chan time.Time { return nil }
	m.NewID = func() string { return "00000000000000c3" }
	for _, r := range []*run{
		finishedRun("a1", "u", now.Add(-3*time.Minute)),
		finishedRun("a2", "u", now.Add(-time.Minute)),
		finishedRun("b1", "v", now.Add(-4*time.Minute)),
		finishedRun("b2", "v", now.Add(-2*time.Minute)),
	} {
		m.runs[r.ID] = r
	}
	m.runs["a1"].State, m.runs["a1"].Result = StateFailed, nil // failed ones count the same
	hs := ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"}
	r, err := m.Start(context.Background(), Target{ServerUUID: "u", Attach: tg, Hsperf: hs}, Request{Mode: ModeWall, Seconds: 1, IntervalMs: 50})
	if err != nil {
		t.Fatal(err)
	}
	// The third run of u drops its oldest; v keeps both.
	if got := runIDs(m); got != "00000000000000c3,a2,b1,b2" {
		t.Errorf("kept %s", got)
	}
	// The running one blocks a new start and nothing is dropped.
	if _, err := m.Start(context.Background(), Target{ServerUUID: "u", Attach: tg, Hsperf: hs}, Request{Mode: ModeWall, Seconds: 1, IntervalMs: 50}); err == nil {
		t.Error("second start while running")
	}
	if got := runIDs(m); got != "00000000000000c3,a2,b1,b2" {
		t.Errorf("kept %s after a refused start", got)
	}
	m.Delete("u", r.ID)
}

func TestDropOldestKeepsActive(t *testing.T) {
	m, _ := newManager(t)
	now := time.Unix(1_700_000_000, 0)
	// The active run is the oldest (no finished_at yet), it stays anyway.
	p := &run{Run: Run{ID: "p", ServerUUID: "u", State: StateProcessing, StartedAt: now.Add(-time.Hour)}}
	m.runs["p"] = p
	for _, r := range []*run{finishedRun("d1", "u", now.Add(-2*time.Minute)), finishedRun("d2", "u", now.Add(-time.Minute))} {
		m.runs[r.ID] = r
	}
	m.mu.Lock()
	m.dropOldestLocked("u")
	m.mu.Unlock()
	if got := runIDs(m); got != "d2,p" {
		t.Errorf("kept %s", got)
	}
	// Only active runs left over the limit: nothing to drop, no endless loop.
	delete(m.runs, "d2")
	m.runs["q"] = &run{Run: Run{ID: "q", ServerUUID: "u", State: StateRunning}}
	m.runs["r"] = &run{Run: Run{ID: "r", ServerUUID: "u", State: StateRunning}}
	m.mu.Lock()
	m.dropOldestLocked("u")
	m.mu.Unlock()
	if got := runIDs(m); got != "p,q,r" {
		t.Errorf("kept %s", got)
	}
}

func TestGCTracker(t *testing.T) {
	d, err := hsperf.ReadFile(ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := hsperf.ReadFile(ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"})
	start := int64(63_000_000) // ms since JVM start
	g := newGCTracker(before, start)
	g.observe(d) // nothing new
	// One young collection 1.5 s after the start.
	d.SetLong("sun.gc.collector.0.invocations", 117)
	d.SetLong("sun.gc.collector.0.time", 2700556872+10_000_000)
	d.SetLong("sun.gc.collector.0.lastEntryTime", (start+1500)*1_000_000)
	d.SetLong("sun.gc.collector.0.lastExitTime", (start+1500)*1_000_000+10_000_000)
	g.observe(d)
	g.observe(d)
	cs, ps, complete := g.result(before, d)
	if len(ps) != 1 || ps[0].StartOffsetMs != 1500 || ps[0].DurationMs != 10 || !complete || cs[0].Count != 1 || cs[0].TimeMs != 10 || cs[1].Count != 0 {
		t.Errorf("collectors %+v pauses %+v complete %v", cs, ps, complete)
	}
	// Two collections between polls: only the last one is seen.
	d.SetLong("sun.gc.collector.0.invocations", 119)
	d.SetLong("sun.gc.collector.0.lastEntryTime", (start+3000)*1_000_000)
	d.SetLong("sun.gc.collector.0.lastExitTime", (start+3000)*1_000_000+5_000_000)
	g.observe(d)
	if _, ps, complete := g.result(before, d); len(ps) != 2 || complete {
		t.Errorf("pauses %+v complete %v", ps, complete)
	}
}

// panicLoader panics on the commands that start with prefix, else records them.
type panicLoader struct {
	recLoader
	prefix string
}

func (l *panicLoader) Load(ctx context.Context, t attach.Target, lib, options string) error {
	l.recLoader.Load(ctx, t, lib, options)
	if strings.HasPrefix(options, l.prefix) {
		panic("injected: " + options)
	}
	return nil
}

func TestRunPanicFails(t *testing.T) {
	for _, c := range []struct {
		name   string
		hsperf bool   // ReadHsperf panics while running
		loader string // the loader panics on these commands
		stops  int    // stop commands sent
	}{
		{"hsperf while running", true, "", 1},
		{"stop while processing", false, "stop,", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, tg := newManager(t)
			l := &panicLoader{prefix: c.loader}
			if c.loader == "" {
				l.prefix = "never"
			}
			m.Loader = l
			const id = "00000000000000bb"
			m.NewID = func() string { return id }
			reads := 0
			m.ReadHsperf = func(f ctmp.File) (*hsperf.Data, error) {
				if reads++; reads > 1 && c.hsperf {
					panic("injected: hsperf")
				}
				return hsperf.ReadFile(f)
			}
			tick := make(chan time.Time)
			end := make(chan time.Time)
			m.Poll = func() (<-chan time.Time, func()) { return tick, func() {} }
			m.After = func(time.Duration) <-chan time.Time { return end }
			released := make(chan struct{})
			m.Yield = func(context.Context, int) func() { return func() { close(released) } }
			hs := ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"}
			r, err := m.Start(context.Background(), Target{ServerUUID: "u", Attach: tg, Hsperf: hs}, Request{Mode: ModeWall, Seconds: 1, IntervalMs: 50})
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"mpa-" + id + ".log", "mpa-" + id + ".collapsed"} {
				os.WriteFile(filepath.Join(tg.Tmp.Root, n), []byte("x"), 0o644)
			}
			if c.hsperf {
				tick <- time.Time{}
			} else {
				end <- time.Time{}
			}
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Fatal("release not called")
			}
			m.mu.Lock()
			done := m.runs[id].done
			m.mu.Unlock()
			<-done
			got, _ := m.Get("u", r.ID)
			if got.State != StateFailed || got.Err == nil || got.Err.Code != CodeInternal || got.ExpiresAt.IsZero() {
				t.Errorf("run %+v, err %+v", got, got.Err)
			}
			stops := 0
			for _, o := range l.opts {
				if strings.HasPrefix(o, "stop,") {
					stops++
				}
			}
			if stops != c.stops {
				t.Errorf("%d stops: %q", stops, l.opts)
			}
			es, _ := os.ReadDir(tg.Tmp.Root)
			for _, e := range es {
				if e.Name() != LibName {
					t.Errorf("left in /tmp: %s", e.Name())
				}
			}
			// The server can be profiled again.
			m.NewID = func() string { return "00000000000000bc" }
			c.hsperf, l.prefix, m.Yield = false, "never", nil
			if _, err := m.Start(context.Background(), Target{ServerUUID: "u", Attach: tg, Hsperf: hs}, Request{Mode: ModeWall, Seconds: 1, IntervalMs: 50}); err != nil {
				t.Errorf("start after the panic: %v", err)
			}
			m.Delete("u", "00000000000000bc")
		})
	}
}

func TestStartPanicDiscards(t *testing.T) {
	m, tg := newManager(t)
	m.Loader = &panicLoader{prefix: "start,"}
	released := false
	m.Yield = func(context.Context, int) func() { return func() { released = true } }
	hs := ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must reach net/http")
			}
		}()
		m.Start(context.Background(), Target{ServerUUID: "u", Attach: tg, Hsperf: hs}, Request{Mode: ModeWall, Seconds: 1, IntervalMs: 50})
	}()
	if got := runIDs(m); got != "" || !released {
		t.Errorf("runs %q, released %v", got, released)
	}
}
