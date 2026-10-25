package api

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
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
		clock: c,
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
