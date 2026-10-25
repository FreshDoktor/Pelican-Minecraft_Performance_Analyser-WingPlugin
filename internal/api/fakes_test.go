package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeAttach answers jcmd commands with fixture output and advances the clock
// by a fixed duration per command.
type fakeAttach struct {
	clock   *clock
	out     map[string]string
	errs    map[string]error
	took    map[string]time.Duration
	mu      sync.Mutex
	starts  int
	targets []attach.Target
	cmds    []string

	// async-profiler
	loads     []string
	libs      []string
	loadErr   error
	collapsed string
	tickOut   func(options string) string // output of the tick monitor's commands
	stopTook  time.Duration
	timers    *fakeTimers
	cpuStep   int64 // CPU ticks per read
	freeBytes int64 // free space in /tmp
	hsperfErr error
	hsperfMod func(*hsperf.Data)
}

// readHsperf is the profile manager's hsperfdata source: the fixture with
// optional modifications (e.g. a collection during the profile).
func (f *fakeAttach) readHsperf(_ *Server, p string) (*hsperf.Data, error) {
	f.mu.Lock()
	err, mod := f.hsperfErr, f.hsperfMod
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	d, err := hsperf.ReadFile(p)
	if err == nil && mod != nil {
		mod(d)
	}
	return d, err
}

func (f *fakeAttach) Start(_ context.Context, t attach.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.targets = append(f.targets, t)
	return nil
}

func (f *fakeAttach) Jcmd(_ context.Context, t attach.Target, cmd string) (string, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.targets = append(f.targets, t)
	f.mu.Unlock()
	f.clock.Advance(f.took[cmd])
	if err := f.errs[cmd]; err != nil {
		return "", err
	}
	out, ok := f.out[cmd]
	if !ok {
		return "", &attach.CommandError{Code: 1, Message: "java.lang.IllegalArgumentException: Unknown diagnostic command"}
	}
	return out, nil
}

// exVMFlags is an illustrative VM.flags output for the fixture JVM.
const exVMFlags = "-XX:CICompilerCount=2 -XX:InitialHeapSize=134217728 -XX:MaxHeapSize=1999634432 -XX:MaxNewSize=666894336 " +
	"-XX:MinHeapDeltaBytes=196608 -XX:MinHeapSize=134217728 -XX:NewSize=44695552 -XX:NonNMethodCodeHeapSize=5832780 " +
	"-XX:NonProfiledCodeHeapSize=122912730 -XX:OldSize=89522176 -XX:ProfiledCodeHeapSize=122912730 " +
	"-XX:ReservedCodeCacheSize=251658240 -XX:+SegmentedCodeCache -XX:SoftMaxHeapSize=1999634432 " +
	"-XX:+UseCompressedOops -XX:+UseSerialGC \n"

// exHeapInfo is an illustrative GC.heap_info output for the fixture JVM (Serial).
const exHeapInfo = "DefNew     total 96704K, used 46125K [0x0000000088c00000, 0x000000008f2f0000, 0x00000000b0a00000)\n" +
	" eden space 86016K,  41% used [0x0000000088c00000, 0x000000008b4d4f10, 0x000000008e000000)\n" +
	" from space 10688K,  46% used [0x000000008e000000, 0x000000008e4ce070, 0x000000008ea70000)\n" +
	" to   space 10688K,   0% used [0x000000008ea70000, 0x000000008ea70000, 0x000000008f4e0000)\n" +
	"Tenured    total 500348K, used 500231K [0x00000000b0a00000, 0x00000000cf2df000, 0x0000000100000000)\n" +
	" the  space 500348K,  99% used [0x00000000b0a00000, 0x00000000cf2c1bd0, 0x00000000cf2df000)\n"

// exVMMetaspace is an illustrative beginning of VM.metaspace (JDK 25), with
// the values of the fixture's hsperfdata.
const exVMMetaspace = "Metaspace        used 112295K, committed 113344K, reserved 1179648K\n" +
	" class space     used 18759K, committed 19200K, reserved 1048576K\n\n" +
	"Total Usage - 1129 loaders, 28064 classes (1453 shared):\n"

// fixtureText reads a node fixture; jcmd's "<pid>:" line is removed because
// the attach protocol does not send it.
func fixtureText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(strings.ReplaceAll(string(b), "\r\n", "\n"), "26:\n")
}

func newFakeAttach(t *testing.T, c *clock) *fakeAttach {
	return &fakeAttach{
		clock:     c,
		cpuStep:   4,
		freeBytes: 1 << 30,
		stopTook:  4871 * time.Microsecond,
		out: map[string]string{
			"Thread.print -l":         fixtureText(t, "mpa-threads.txt"),
			"GC.class_histogram -all": fixtureText(t, "mpa-histogram-top300.txt"),
			"VM.flags":                exVMFlags,
			"GC.heap_info":            exHeapInfo,
			"VM.metaspace":            exVMMetaspace,
		},
		errs: map[string]error{},
		took: map[string]time.Duration{
			"Thread.print -l":         18204 * time.Microsecond,
			"GC.class_histogram -all": 187316 * time.Microsecond,
			"VM.flags":                2113 * time.Microsecond,
			"GC.heap_info":            1874 * time.Microsecond,
			"VM.metaspace":            912 * time.Microsecond,
		},
	}
}

// Load fakes async-profiler: "start…" succeeds (or fails with loadErr),
// "stop…" writes collapsed into the file= of the options and advances the
// clock by stopTook.
func (f *fakeAttach) Load(_ context.Context, t attach.Target, lib, options string) error {
	f.mu.Lock()
	f.loads = append(f.loads, options)
	f.libs = append(f.libs, lib)
	err := f.loadErr
	collapsed := f.collapsed
	if f.tickOut != nil && strings.Contains(options, "/tmp/mpa-ticks-") {
		collapsed = f.tickOut(options)
	}
	f.mu.Unlock()
	if strings.HasPrefix(options, "start") {
		return err
	}
	f.clock.Advance(f.stopTook)
	for _, o := range strings.Split(options, ",") {
		if file, ok := strings.CutPrefix(o, "file=/tmp/"); ok {
			os.WriteFile(filepath.Join(t.TmpDir, file), []byte(collapsed), 0o644)
			os.WriteFile(filepath.Join(t.TmpDir, strings.TrimSuffix(file, ".collapsed")+".log"), nil, 0o644)
		}
	}
	return nil
}

func (f *fakeAttach) loadCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.loads...)
}

// fakeTimers drives the profile loop: end fires the end of every profile,
// poll one hsperfdata poll, tick one window of the tick monitor.
type fakeTimers struct {
	end  chan time.Time
	poll chan time.Time
	tick chan time.Time
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{end: make(chan time.Time), poll: make(chan time.Time), tick: make(chan time.Time)}
}

func (ft *fakeTimers) Every(time.Duration) (<-chan time.Time, func()) { return ft.tick, func() {} }

func (ft *fakeTimers) After(time.Duration) <-chan time.Time { return ft.end }

func (ft *fakeTimers) Poll() (<-chan time.Time, func()) { return ft.poll, func() {} }

// fire sends on c or fails after a second (no profile waiting).
func fire(t *testing.T, c chan time.Time) {
	t.Helper()
	select {
	case c <- time.Time{}:
	case <-time.After(time.Second):
		t.Fatal("no running profile loop")
	}
}

// waitState polls a profile until it reaches state.
func waitState(t *testing.T, s *Server, uuid, id, state string) profile.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := s.Profiles.Get(uuid, id); ok && r.State == state {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, _ := s.Profiles.Get(uuid, id)
	t.Fatalf("profile %s: state %q, want %q", id, r.State, state)
	return r
}
