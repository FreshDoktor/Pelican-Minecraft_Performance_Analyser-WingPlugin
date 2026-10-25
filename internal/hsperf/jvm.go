package hsperf

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// GC types as exposed by the API.
const (
	GCSerial     = "serial"
	GCParallel   = "parallel"
	GCG1         = "g1"
	GCZ          = "zgc"
	GCShenandoah = "shenandoah"
	GCUnknown    = "unknown"
)

// Collector kinds.
const (
	KindYoung      = "young"
	KindOld        = "old"
	KindFull       = "full"
	KindConcurrent = "concurrent"
	KindHeap       = "heap"
	KindUnknown    = "unknown"
)

// Collector is one sun.gc.collector.<n> block.
type Collector struct {
	Index       int
	Name        string
	Invocations int64
	Time        int64 // ticks
	LastEntry   int64 // ticks since JVM start
	LastExit    int64 // ticks since JVM start
}

// Space is one sun.gc.generation.<g>.space.<s> block.
type Space struct {
	Index               int
	Name                string
	Used, Capacity, Max int64
}

// Generation is one sun.gc.generation.<g> block.
type Generation struct {
	Index         int
	Name          string
	Capacity, Max int64
	Spaces        []Space
}

// Used is the sum of the used bytes of all spaces.
func (g Generation) Used() int64 {
	var u int64
	for _, s := range g.Spaces {
		u += s.Used
	}
	return u
}

// Frequency returns sun.os.hrt.frequency (ticks per second), defaulting to 10^9.
func (d *Data) Frequency() int64 {
	if f, ok := d.Long("sun.os.hrt.frequency"); ok && f > 0 {
		return f
	}
	return 1_000_000_000
}

// TicksToMs converts ticks to milliseconds, rounded to microseconds.
func (d *Data) TicksToMs(t int64) float64 {
	return RoundMs(float64(t) * 1000 / float64(d.Frequency()))
}

// RoundMs rounds a millisecond value to three decimals.
func RoundMs(ms float64) float64 { return math.Round(ms*1000) / 1000 }

// indices returns the sorted distinct <n> of all names "<prefix><n>.<suffix>".
func (d *Data) indices(prefix, suffix string) []int {
	var out []int
	for _, name := range d.Names(prefix) {
		rest := strings.TrimPrefix(name, prefix)
		num, tail, ok := strings.Cut(rest, ".")
		if !ok || tail != suffix {
			continue
		}
		if n, err := strconv.Atoi(num); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// Collectors returns all collectors ordered by index. Indices may have gaps (ZGC: 0 and 2).
func (d *Data) Collectors() []Collector {
	var out []Collector
	for _, i := range d.indices("sun.gc.collector.", "name") {
		p := "sun.gc.collector." + strconv.Itoa(i) + "."
		c := Collector{Index: i}
		c.Name, _ = d.String(p + "name")
		c.Invocations, _ = d.Long(p + "invocations")
		c.Time, _ = d.Long(p + "time")
		c.LastEntry, _ = d.Long(p + "lastEntryTime")
		c.LastExit, _ = d.Long(p + "lastExitTime")
		out = append(out, c)
	}
	return out
}

// Generations returns all heap generations with their spaces, ordered by index.
func (d *Data) Generations() []Generation {
	var out []Generation
	for _, i := range d.indices("sun.gc.generation.", "name") {
		p := "sun.gc.generation." + strconv.Itoa(i) + "."
		g := Generation{Index: i}
		g.Name, _ = d.String(p + "name")
		g.Capacity, _ = d.Long(p + "capacity")
		g.Max, _ = d.Long(p + "maxCapacity")
		for _, s := range d.indices(p+"space.", "name") {
			sp := p + "space." + strconv.Itoa(s) + "."
			x := Space{Index: s}
			x.Name, _ = d.String(sp + "name")
			x.Used, _ = d.Long(sp + "used")
			x.Capacity, _ = d.Long(sp + "capacity")
			x.Max, _ = d.Long(sp + "maxCapacity")
			g.Spaces = append(g.Spaces, x)
		}
		out = append(out, g)
	}
	return out
}

// GCType derives the collector family from the collector names, falling back
// to the legacy names of older JDKs and to sun.gc.policy.name.
func (d *Data) GCType() string {
	for _, c := range d.Collectors() {
		if t := gcTypeFromName(c.Name); t != GCUnknown {
			return t
		}
	}
	policy, _ := d.String("sun.gc.policy.name")
	switch {
	case policy == "Copy:MSC":
		return GCSerial
	case strings.HasPrefix(policy, "ParScav"):
		return GCParallel
	case policy == "GarbageFirst":
		return GCG1
	}
	return GCUnknown
}

func gcTypeFromName(name string) string {
	switch {
	case strings.HasPrefix(name, "Serial "), name == "Copy", name == "MSC", name == "MarkSweepCompact":
		return GCSerial
	case strings.HasPrefix(name, "Parallel "), name == "PSScavenge", name == "PSParallelCompact":
		return GCParallel
	case strings.HasPrefix(name, "G1 "):
		return GCG1
	case strings.HasPrefix(name, "ZGC "), strings.HasPrefix(name, "Z "):
		return GCZ
	case strings.HasPrefix(name, "Shenandoah"):
		return GCShenandoah
	}
	return GCUnknown
}

// CollectorKind classifies a collector by its name.
func CollectorKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "young"), strings.Contains(n, "minor"), strings.Contains(n, "incremental"),
		name == "Copy", name == "PSScavenge":
		return KindYoung
	case strings.Contains(n, "full"), name == "MSC", name == "MarkSweepCompact", name == "PSParallelCompact":
		return KindFull
	case strings.Contains(n, "major"):
		return KindOld
	case strings.Contains(n, "concurrent"), strings.Contains(n, "partial"), strings.Contains(n, "phases"):
		return KindConcurrent
	}
	return KindUnknown
}

// TimeIsPause reports whether the collector's time counter measures
// stop-the-world pauses. nil means unknown.
func TimeIsPause(name string) *bool {
	t, f := true, false
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "pause"):
		return &t
	case strings.Contains(n, "cycle"):
		return &f
	case strings.HasPrefix(name, "Shenandoah"):
		return nil
	case gcTypeFromName(name) != GCUnknown, strings.Contains(n, "stop-the-world"):
		return &t
	}
	return nil
}

// GenerationKind classifies a generation by its name.
func GenerationKind(name string) string {
	switch strings.ToLower(name) {
	case "new", "young":
		return KindYoung
	case "old", "tenured":
		return KindOld
	case "heap":
		return KindHeap
	}
	return KindUnknown
}

// HeapTotals returns used, committed and max bytes of the whole heap.
// Used and committed are summed over all generations. Max is summed for
// Serial and Parallel; G1, ZGC and Shenandoah report the whole heap as max of
// every generation, so the maximum is taken there (and for unknown collectors).
func (d *Data) HeapTotals(gcType string) (used, committed, max int64) {
	for _, g := range d.Generations() {
		used += g.Used()
		committed += g.Capacity
		switch gcType {
		case GCSerial, GCParallel:
			max += g.Max
		default:
			if g.Max > max {
				max = g.Max
			}
		}
	}
	return used, committed, max
}

// AttachSupported reports the first flag of sun.rt.jvmCapabilities.
func (d *Data) AttachSupported() (supported, known bool) {
	c, ok := d.String("sun.rt.jvmCapabilities")
	if !ok || c == "" {
		return false, false
	}
	return c[0] == '1', true
}

// FeatureVersion parses the Java feature version ("25.0.4.1" → 25, "1.8.0_392" → 8).
func FeatureVersion(v string) (int, bool) {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r < '0' || r > '9' })
	if len(parts) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, false
	}
	if n == 1 && len(parts) > 1 {
		if m, err := strconv.Atoi(parts[1]); err == nil {
			return m, true
		}
	}
	return n, true
}
