package parse

import "strings"

// Mappings the Server thread's methods can be named in (docs/API.md, "Tick and idle methods").
const (
	MappingMojmap       = "mojmap"
	MappingIntermediary = "intermediary"
	MappingSRG          = "srg"
	MappingMCP          = "mcp"
)

// Kinds of a Server thread stack.
const (
	TickWork = "tick"
	TickIdle = "idle"
)

// TickMethods are the tick (tickServer, tickChildren) and idle
// (waitUntilNextTick, waitForTasks) methods of MinecraftServer per mapping,
// the same table as ProfileAnalyzer::MAPPINGS in the panel.
var TickMethods = []struct {
	Mapping string
	Tick    [2]string
	Idle    [2]string
	Run     string // runServer, the loop that calls the tick
}{
	{MappingMojmap, [2]string{"tickServer", "tickChildren"}, [2]string{"waitUntilNextTick", "waitForTasks"}, "runServer"},
	{MappingIntermediary, [2]string{"method_3748", "method_3813"}, [2]string{"method_16208", "method_20813"}, "method_29741"},
	{MappingSRG, [2]string{"m_5705_", "m_5703_"}, [2]string{"m_130012_", "m_5667_"}, "m_130011_"},
	{MappingMCP, [2]string{"func_71217_p", "func_71190_q"}, [2]string{"func_213202_o", "func_223705_bi"}, "func_240802_v_"},
}

// blockingFrames start the frames of a thread that blocks (park, sleep, wait).
var blockingFrames = []string{
	"java/util/concurrent/locks/LockSupport.park",
	"jdk/internal/misc/Unsafe.park",
	"sun/misc/Unsafe.park",
	"java/lang/Thread.sleep",
	"java/lang/Object.wait",
}

// ServerClass is the class of the tick loop in every mapping (JVM form).
const ServerClass = "net/minecraft/server/MinecraftServer"

// TickServerMethod returns the name of tickServer in a mapping, "" if unknown.
func TickServerMethod(mapping string) string {
	for _, m := range TickMethods {
		if m.Mapping == mapping {
			return m.Tick[0]
		}
	}
	return ""
}

// ClassifyTickStack looks at a collapsed stack (root first) from the root:
// the first frame of a class ending in /MinecraftServer that is a tick or idle
// method decides. Up to 1.13 MinecraftServer.run directly followed by
// java/lang/Thread.sleep is idle (mcp). Without such a frame, a stack in
// runServer that blocks with only mod, JDK or mixin frames ($ in the method,
// e.g. redirect$…) in between is idle too: the server is paused between two
// ticks by a mod (Tick Stasis parks the Server thread while nobody is
// online). Minecraft's own code in between is work, e.g. loading the worlds
// at start. Returns "", "" if no frame decides.
func ClassifyTickStack(frames []string) (kind, mapping string) {
	run, ranOut := "", false
	for i, f := range frames {
		class, method, ok := splitFrame(f)
		if !ok || !strings.HasSuffix(class, "/MinecraftServer") {
			switch {
			case run == "":
			case blocking(f):
				return TickIdle, run
			case strings.HasPrefix(f, "net/minecraft/"):
				run = ""
			}
			continue
		}
		if method == "run" && i+1 < len(frames) && frames[i+1] == "java/lang/Thread.sleep" {
			return TickIdle, MappingMCP
		}
		isRun := false
		for _, m := range TickMethods {
			if method == m.Tick[0] || method == m.Tick[1] {
				return TickWork, m.Mapping
			}
			if method == m.Idle[0] || method == m.Idle[1] {
				return TickIdle, m.Mapping
			}
			if method == m.Run && !ranOut {
				run, ranOut, isRun = m.Mapping, true, true
			}
		}
		if !isRun && !strings.Contains(method, "$") {
			run = ""
		}
	}
	return "", ""
}

func blocking(frame string) bool {
	for _, b := range blockingFrames {
		if strings.HasPrefix(frame, b) {
			return true
		}
	}
	return false
}

// splitFrame splits "pkg/Class.method" at the last dot of the method part.
func splitFrame(f string) (class, method string, ok bool) {
	i := strings.LastIndexByte(f, '.')
	if i <= 0 || i == len(f)-1 || strings.ContainsAny(f, " (") {
		return "", "", false
	}
	return f[:i], f[i+1:], true
}
