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
