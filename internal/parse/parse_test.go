package parse

import (
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func TestVMArgs(t *testing.T) {
	a := ParseVMArgs("-Xms128M -Xmx1907M")
	if *a.Xms != 128<<20 || *a.Xmx != 1907<<20 || a.GC != nil || a.Xrs || a.MaxRAMPercentage != nil {
		t.Errorf("%+v", a)
	}
	a = ParseVMArgs("-Xmx1G -XX:+UseG1GC -XX:MaxRAMPercentage=75.0 -Xss512k -XX:MaxMetaspaceSize=512m -Xrs -XX:+DisableAttachMechanism -XX:MaxHeapSize=2g -Dfoo=-Xmx9g")
	if *a.Xmx != 2<<30 || *a.GC != "g1" || *a.MaxRAMPercentage != 75 || *a.Xss != 512<<10 ||
		*a.MaxMetaspaceSize != 512<<20 || !a.Xrs || !a.DisableAttach {
		t.Errorf("%+v", a)
	}
	if a := ParseVMArgs("-XX:+UseZGC -XX:-ReduceSignalUsage -XX:ThreadStackSize=1024"); *a.GC != "zgc" || a.Xrs || *a.Xss != 1<<20 {
		t.Errorf("%+v", a)
	}
	for in, want := range map[string]int64{"524288": 524288, "4k": 4096, "2T": 2 << 40} {
		if got, ok := ParseSize(in); !ok || got != want {
			t.Errorf("ParseSize(%q) = %d", in, got)
		}
	}
	for _, bad := range []string{"", "M", "-1g", "1.5g"} {
		if _, ok := ParseSize(bad); ok {
			t.Errorf("ParseSize(%q) must fail", bad)
		}
	}
}

func TestThreadDumpFixture(t *testing.T) {
	d := ParseThreadDump(strings.TrimPrefix(readFixture(t, "mpa-threads.txt"), "26:\n"))
	if d.Timestamp != "2026-10-25 13:09:43" || d.VM != "OpenJDK 64-Bit Server VM (25.0.4.1+1-LTS mixed mode, sharing)" {
		t.Errorf("header %q %q", d.Timestamp, d.VM)
	}
	if len(d.Threads) != 28 || d.JNIGlobal == nil || *d.JNIGlobal != 41 || *d.JNIWeak != 49 || len(d.Deadlocks) != 0 {
		t.Fatalf("threads %d", len(d.Threads))
	}
	var st *Thread
	vm := 0
	for i := range d.Threads {
		if d.Threads[i].Name == "Server thread" {
			st = &d.Threads[i]
		}
		if d.Threads[i].VMThread {
			vm++
		}
	}
	if vm != 2 {
		t.Errorf("vm threads %d", vm)
	}
	if st == nil {
		t.Fatal("no Server thread")
	}
	// Header says runnable, the state line counts.
	if *st.JavaID != 30 || *st.OSID != 60 || st.Daemon || *st.CPUMs != 656.31 || *st.ElapsedMs != 19430 ||
		st.State != "TIMED_WAITING" || st.StateDetail != "parking" || st.OSState != "runnable" {
		t.Errorf("%+v", st)
	}
	if len(st.Stack) != 10 || st.Stack[0].Method != "jdk.internal.misc.Unsafe.park" || !st.Stack[0].Native ||
		st.Stack[0].Module != "java.base@25.0.4.1" || len(st.Stack[0].Locks) != 1 {
		t.Fatalf("stack %+v", st.Stack)
	}
	l := st.Stack[0].Locks[0]
	if l.Type != "parking" || l.Address != "0x00000000b82a7ff0" || l.Class != "java.lang.String" {
		t.Errorf("lock %+v", l)
	}
	f := st.Stack[4]
	if f.Method != "net.minecraft.server.MinecraftServer.waitUntilNextTick" || f.File != "MinecraftServer.java" || *f.Line != 859 || f.Module != "" {
		t.Errorf("frame %+v", f)
	}
	if f := st.Stack[7]; f.File != "" || f.Line != nil || f.Native {
		t.Errorf("Unknown Source frame %+v", f)
	}
}

// Fabric server with mods (Immersive Snow), frames with intermediary names and
// a mixin handler. The dump has no "<pid>:" line in front.
func TestThreadDumpFabricFixture(t *testing.T) {
	d := ParseThreadDump(readFixture(t, "threads-fabric-mods.txt"))
	if d.Timestamp != "2026-10-25 19:32:53" || len(d.Threads) != 48 || *d.JNIGlobal != 45 || *d.JNIWeak != 45 || len(d.Deadlocks) != 0 {
		t.Fatalf("header %q, threads %d", d.Timestamp, len(d.Threads))
	}
	var st *Thread
	for i := range d.Threads {
		if d.Threads[i].Name == "Server thread" {
			st = &d.Threads[i]
		}
	}
	if st == nil {
		t.Fatal("no Server thread")
	}
	if *st.JavaID != 44 || *st.CPUMs != 40972.79 || st.State != "TIMED_WAITING" || st.StateDetail != "parking" || len(st.Stack) != 32 {
		t.Fatalf("%+v", st)
	}
	// Waits for a chunk inside the tick of Immersive Snow.
	if f := st.Stack[4]; f.Method != "net.minecraft.class_3215.getChunkBlocking" {
		t.Errorf("frame 4 %+v", f)
	}
	if f := st.Stack[18]; f.Method != "straywave.minecraft.immersivesnow.ImmersiveSnowEvents.onWorldTick" || f.File != "ImmersiveSnowEvents.java" || *f.Line != 51 {
		t.Errorf("frame 18 %+v", f)
	}
	if f := st.Stack[22]; f.Method != "net.minecraft.class_3218.handler$bha000$fabric-lifecycle-events-v1$startWorldTick" {
		t.Errorf("mixin frame %+v", f)
	}
}

func TestThreadDumpDeadlock(t *testing.T) {
	dump := `2026-10-25 13:00:00
Full thread dump OpenJDK 64-Bit Server VM (25.0.4.1+1-LTS mixed mode, sharing):

"A" #31 [70] prio=5 os_prio=0 cpu=1.00ms elapsed=5.00s tid=0x1 nid=70 waiting for monitor entry  [0x2]
   java.lang.Thread.State: BLOCKED (on object monitor)
	at Demo.a(Demo.java:10)
	- waiting to lock <0x00000000c0000010> (a java.lang.Object)
	- locked <0x00000000c0000020> (a java.lang.Object)

   Locked ownable synchronizers:
	- <0x00000000c0000030> (a java.util.concurrent.locks.ReentrantLock$NonfairSync)

"B" #32 [71] prio=5 os_prio=0 cpu=1.00ms elapsed=5.00s tid=0x3 nid=71 waiting for monitor entry  [0x4]
   java.lang.Thread.State: BLOCKED (on object monitor)
	at Demo.b(Demo.java:20)
	- waiting to lock <0x00000000c0000020> (a java.lang.Object)
	- eliminated <owner is scalar replaced> (a java.lang.Object)

JNI global refs: 1, weak refs: 0


Found one Java-level deadlock:
=============================
"A":
  waiting to lock monitor 0x00007f0000000001 (object 0x00000000c0000010, a java.lang.Object),
  which is held by "B"

"B":
  waiting to lock monitor 0x00007f0000000002 (object 0x00000000c0000020, a java.lang.Object),
  which is held by "A"

Java stack information for the threads listed above:
===================================================
"A":
	at Demo.a(Demo.java:10)

Found 1 deadlock.
`
	d := ParseThreadDump(dump)
	if len(d.Threads) != 2 || d.Threads[0].State != "BLOCKED" {
		t.Fatalf("%+v", d.Threads)
	}
	if ls := d.Threads[0].Stack[0].Locks; len(ls) != 2 || ls[0].Type != "waiting_to_lock" || ls[1].Type != "locked" {
		t.Errorf("locks %+v", ls)
	}
	if s := d.Threads[0].Synchronizers; len(s) != 1 || s[0].Type != "owned" || s[0].Class != "java.util.concurrent.locks.ReentrantLock$NonfairSync" {
		t.Errorf("sync %+v", s)
	}
	if l := d.Threads[1].Stack[0].Locks[1]; l.Type != "eliminated" || l.Address != "" {
		t.Errorf("eliminated %+v", l)
	}
	if len(d.Deadlocks) != 1 || len(d.Deadlocks[0].Threads) != 2 {
		t.Fatalf("deadlocks %+v", d.Deadlocks)
	}
	a := d.Deadlocks[0].Threads[0]
	if a.Name != "A" || a.HeldBy != "B" || !strings.HasPrefix(a.WaitingFor, "waiting to lock monitor 0x00007f0000000001") {
		t.Errorf("%+v", a)
	}
}

func TestHistogramFixture(t *testing.T) {
	h := ParseHistogram(readFixture(t, "mpa-histogram-top300.txt"))
	if len(h.Classes) != 302 || h.TotalInstances != 3594114 || h.TotalBytes != 150843200 {
		t.Fatalf("%d classes, total %d/%d", len(h.Classes), h.TotalInstances, h.TotalBytes)
	}
	c := h.Classes[0]
	if c.Rank != 1 || c.Name != "[B" || c.ElementType != "byte" || c.ArrayDepth != 1 || c.Module != "java.base@25.0.4.1" || c.Bytes != 32965352 {
		t.Errorf("%+v", c)
	}
	if c := h.Classes[4]; c.Name != "net.minecraft.world.level.block.state.BlockState" || c.Module != "" || c.ArrayDepth != 0 {
		t.Errorf("%+v", c)
	}
	if last := h.Classes[len(h.Classes)-1]; last.Rank != 16670 || last.Module != "jdk.localedata@25.0.4.1" {
		t.Errorf("%+v", last)
	}
	for in, want := range map[string]string{
		"[Ljava.lang.Object;": "java.lang.Object",
		"[[Lnet.minecraft.world.level.block.state.StateHolder;": "net.minecraft.world.level.block.state.StateHolder",
		"[[I":              "int",
		"java.lang.String": "java.lang.String",
	} {
		if got, _ := ElementType(in); got != want {
			t.Errorf("ElementType(%q) = %q", in, got)
		}
	}
}

func TestCollapsed(t *testing.T) {
	text := "[Server thread tid=60];a;b;c 198\n[Server thread tid=60];a;b;d 3\n[Watchdog tid=64];x 201\n[Server thread tid=60];a;b;c 2\nno-thread;frame 5\ngarbage\n"
	ts := ParseCollapsed(text)
	if len(ts) != 3 {
		t.Fatalf("%+v", ts)
	}
	st := ts[0]
	if st.Name != "Server thread" || *st.TID != 60 || st.Count != 203 || len(st.Stacks) != 2 || st.Stacks[0].Stack != "a;b;c" || st.Stacks[0].Count != 200 {
		t.Errorf("%+v", st)
	}
	if ts[2].Name != "" || ts[2].TID != nil || ts[2].Stacks[0].Stack != "no-thread;frame" {
		t.Errorf("%+v", ts[2])
	}
}
