package parse

import (
	"regexp"
	"strings"
)

// ParseVMFlags parses the output of VM.flags
// ("-XX:CICompilerCount=2 -XX:+UseSerialGC ..."): name → value, booleans as "true"/"false".
func ParseVMFlags(text string) map[string]string {
	flags := map[string]string{}
	for _, f := range strings.Fields(text) {
		f, ok := strings.CutPrefix(f, "-XX:")
		if !ok || f == "" {
			continue
		}
		switch f[0] {
		case '+':
			flags[f[1:]] = "true"
		case '-':
			flags[f[1:]] = "false"
		default:
			if name, val, ok := strings.Cut(f, "="); ok && name != "" {
				flags[name] = val
			}
		}
	}
	return flags
}

// HeapInfo is the heap summary from GC.heap_info.
type HeapInfo struct {
	Used      int64
	Committed *int64
	Max       *int64
}

var (
	// Serial, Parallel: "DefNew     total 96704K, used 46125K [...]", "PSYoungGen total ..., used ...".
	reGenTotal = regexp.MustCompile(`^\S+\s+total (\d+[KMG]?), used (\d+[KMG]?)`)
	// G1 (JDK 22+): "garbage-first heap   total reserved 262144K, committed 14336K, used 3170K [...]"
	reG1New = regexp.MustCompile(`^garbage-first heap\s+total reserved (\d+[KMG]?), committed (\d+[KMG]?), used (\d+[KMG]?)`)
	// G1 (older): "garbage-first heap   total 262144K, used 23360K [...]"
	reG1Old = regexp.MustCompile(`^garbage-first heap\s+total (\d+[KMG]?), used (\d+[KMG]?)`)
	// ZGC: " ZHeap           used 6M, capacity 256M, max capacity 256M"
	reZ = regexp.MustCompile(`^\s*ZHeap\s+used (\d+[KMG]?), capacity (\d+[KMG]?), max capacity (\d+[KMG]?)`)
	// Shenandoah: " 256M max, 256M soft max, 256M committed, 3777K used"
	reShen = regexp.MustCompile(`^\s*(\d+[KMG]?) max, (?:\d+[KMG]? soft max, )?(\d+[KMG]?) committed, (\d+[KMG]?) used`)
)

// ParseHeapInfo extracts used/committed/max bytes from GC.heap_info for
// Serial, Parallel, G1, ZGC and Shenandoah. ok is false for unknown output.
func ParseHeapInfo(text string) (HeapInfo, bool) {
	size := func(s string) int64 { n, _ := ParseSize(s); return n }
	var h HeapInfo
	var committed int64
	gens := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r ")
		if m := reG1New.FindStringSubmatch(line); m != nil {
			c, mx := size(m[2]), size(m[1])
			return HeapInfo{Used: size(m[3]), Committed: &c, Max: &mx}, true
		}
		if m := reG1Old.FindStringSubmatch(line); m != nil {
			c := size(m[1])
			return HeapInfo{Used: size(m[2]), Committed: &c}, true
		}
		if m := reZ.FindStringSubmatch(line); m != nil {
			c, mx := size(m[2]), size(m[3])
			return HeapInfo{Used: size(m[1]), Committed: &c, Max: &mx}, true
		}
		if m := reShen.FindStringSubmatch(line); m != nil {
			c, mx := size(m[2]), size(m[1])
			return HeapInfo{Used: size(m[3]), Committed: &c, Max: &mx}, true
		}
		// Top-level generation lines only (spaces are indented); never metaspace.
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "Metaspace") {
			continue
		}
		if m := reGenTotal.FindStringSubmatch(line); m != nil {
			committed += size(m[1])
			h.Used += size(m[2])
			gens++
		}
	}
	if gens == 0 {
		return HeapInfo{}, false
	}
	h.Committed = &committed
	return h, true
}

// MetaspaceInfo is the summary line "Metaspace used 1379K, committed 1536K, reserved 1114112K".
type MetaspaceInfo struct {
	Used, Committed, Reserved int64
}

var reMetaspace = regexp.MustCompile(`(?m)^\s*Metaspace\s+used (\d+[KMG]?), committed (\d+[KMG]?), reserved (\d+[KMG]?)`)

// ParseMetaspace finds the metaspace summary in VM.metaspace output (JDK 25)
// or in GC.heap_info output (older JDKs print it there).
func ParseMetaspace(text string) (MetaspaceInfo, bool) {
	m := reMetaspace.FindStringSubmatch(text)
	if m == nil {
		return MetaspaceInfo{}, false
	}
	size := func(s string) int64 { n, _ := ParseSize(s); return n }
	return MetaspaceInfo{Used: size(m[1]), Committed: size(m[2]), Reserved: size(m[3])}, true
}
