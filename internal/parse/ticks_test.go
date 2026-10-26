package parse

import (
	"strings"
	"testing"
)

func TestClassifyTickStack(t *testing.T) {
	const run = "java/lang/Thread.run;net/minecraft/server/MinecraftServer.run"
	cases := []struct {
		stack, kind, mapping string
	}{
		{run + ";net/minecraft/server/MinecraftServer.waitUntilNextTick;java/util/concurrent/locks/LockSupport.parkNanos", TickIdle, MappingMojmap},
		// Waiting inside the tick is tick work: the first decisive frame from the root counts.
		{run + ";net/minecraft/server/MinecraftServer.tickServer;com/example/Mod.onTick;net/minecraft/server/MinecraftServer.waitForTasks", TickWork, MappingMojmap},
		{run + ";net/minecraft/server/MinecraftServer.method_16208", TickIdle, MappingIntermediary},
		{run + ";net/minecraft/server/MinecraftServer.method_3748;net/minecraft/class_3218.method_18765", TickWork, MappingIntermediary},
		{run + ";net/minecraft/server/MinecraftServer.m_130012_", TickIdle, MappingSRG},
		{run + ";net/minecraft/server/MinecraftServer.m_5705_", TickWork, MappingSRG},
		{run + ";net/minecraft/server/MinecraftServer.func_213202_o", TickIdle, MappingMCP},
		{run + ";net/minecraft/server/MinecraftServer.func_71217_p", TickWork, MappingMCP},
		// Up to 1.13: Thread.sleep directly in run.
		{run + ";java/lang/Thread.sleep", TickIdle, MappingMCP},
		{run + ";net/minecraft/server/dedicated/DedicatedServer.func_71190_q", "", ""},
		// waitForTasks of BlockableEventLoop is not MinecraftServer's.
		{run + ";net/minecraft/util/thread/BlockableEventLoop.waitForTasks", "", ""},
		// Obfuscated vanilla before 26.1.
		{run + ";net/minecraft/server/MinecraftServer.w;net/minecraft/server/MinecraftServer.a", "", ""},
		// Paused in runServer without a tick or idle method, e.g. by Tick Stasis (Fabric, real stack).
		{"java/lang/Thread.run;net/minecraft/server/MinecraftServer$$Lambda.0x00007727f95ad8d8.run;net/minecraft/server/MinecraftServer.method_29739;" +
			"net/minecraft/server/MinecraftServer.method_29741;net/minecraft/server/MinecraftServer.redirect$dpk000$tick-stasis$waitUntilResumed;" +
			"java/util/concurrent/locks/LockSupport.park;jdk/internal/misc/Unsafe.park;Unsafe_Park;Parker::park;pthread_cond_wait", TickIdle, MappingIntermediary},
		{run + ";net/minecraft/server/MinecraftServer.runServer;com/example/Pause.hold;java/lang/Thread.sleep", TickIdle, MappingMojmap},
		{run + ";net/minecraft/server/MinecraftServer.m_130011_;java/lang/Object.wait", TickIdle, MappingSRG},
		{run + ";net/minecraft/server/MinecraftServer.func_240802_v_;java/util/concurrent/locks/LockSupport.parkNanos", TickIdle, MappingMCP},
		// In runServer without blocking: undecided. Blocking inside the tick stays tick work, and so does
		// blocking in Minecraft's own code, e.g. loading the worlds at start (Fabric, real stack, shortened).
		{run + ";net/minecraft/server/MinecraftServer.runServer;com/example/Mod.work", "", ""},
		{run + ";net/minecraft/server/MinecraftServer.method_29741;net/minecraft/class_3176.method_3823;net/minecraft/server/MinecraftServer.method_3735;" +
			"net/minecraft/class_3215.getChunkBlocking;net/minecraft/class_1255.method_20813;java/util/concurrent/locks/LockSupport.parkNanos", "", ""},
		{run + ";net/minecraft/server/MinecraftServer.runServer;net/minecraft/server/MinecraftServer.loadLevel;java/lang/Object.wait", "", ""},
		{run + ";net/minecraft/server/MinecraftServer.runServer;net/minecraft/server/MinecraftServer.tickServer;java/util/concurrent/locks/LockSupport.park", TickWork, MappingMojmap},
		{"[unknown_Java]", "", ""},
		{"Parker::park(bool, long);__pthread_cond_timedwait", "", ""},
	}
	for _, c := range cases {
		kind, mapping := ClassifyTickStack(strings.Split(c.stack, ";"))
		if kind != c.kind || mapping != c.mapping {
			t.Errorf("%s: %q %q, want %q %q", c.stack, kind, mapping, c.kind, c.mapping)
		}
	}
	for _, m := range []string{MappingMojmap, MappingIntermediary, MappingSRG, MappingMCP} {
		if TickServerMethod(m) == "" {
			t.Errorf("no tickServer for %s", m)
		}
	}
	if TickServerMethod("obfuscated") != "" {
		t.Error("unknown mapping has a tickServer")
	}
}
