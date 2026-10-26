package hsperf

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

// builder writes hsperfdata v2 buffers like HotSpot's PerfMemory.
type builder struct {
	order   binary.ByteOrder
	entries [][]byte
}

func newBuilder(order binary.ByteOrder) *builder { return &builder{order: order} }

func (b *builder) entry(name string, typ byte, units Units, vecLen int, data []byte) *builder {
	nameOff := 20
	dataOff := nameOff + len(name) + 1
	if pad := dataOff % 8; typ == 'J' && pad != 0 {
		dataOff += 8 - pad
	}
	total := dataOff + len(data)
	if pad := total % 8; pad != 0 {
		total += 8 - pad
	}
	e := make([]byte, total)
	b.order.PutUint32(e[0:], uint32(total))
	b.order.PutUint32(e[4:], uint32(nameOff))
	b.order.PutUint32(e[8:], uint32(vecLen))
	e[12], e[13], e[14], e[15] = typ, 0, byte(units), 1
	b.order.PutUint32(e[16:], uint32(dataOff))
	copy(e[nameOff:], name)
	copy(e[dataOff:], data)
	b.entries = append(b.entries, e)
	return b
}

func (b *builder) long(name string, units Units, v int64) *builder {
	d := make([]byte, 8)
	b.order.PutUint64(d, uint64(v))
	return b.entry(name, 'J', units, 0, d)
}

func (b *builder) str(name, v string) *builder {
	// HotSpot reserves more than the string and pads with zeros.
	d := make([]byte, len(v)+8)
	copy(d, v)
	return b.entry(name, 'B', UnitsString, len(d), d)
}

func (b *builder) bytes() []byte {
	out := make([]byte, 32)
	binary.BigEndian.PutUint32(out[0:], Magic)
	if b.order == binary.LittleEndian {
		out[4] = 1
	}
	out[5], out[6], out[7] = 2, 0, 1
	for _, e := range b.entries {
		out = append(out, e...)
	}
	b.order.PutUint32(out[8:], uint32(len(out)))
	b.order.PutUint32(out[24:], 32)
	b.order.PutUint32(out[28:], uint32(len(b.entries)))
	// Unused rest of the mapping.
	return append(out, make([]byte, 512)...)
}

func collector(b *builder, i, name string, inv, ticks int64) {
	p := "sun.gc.collector." + i + "."
	b.str(p+"name", name).long(p+"invocations", UnitsEvents, inv).long(p+"time", UnitsTicks, ticks).
		long(p+"lastEntryTime", UnitsTicks, 0).long(p+"lastExitTime", UnitsTicks, 0)
}

func gen(b *builder, g, name string, capacity, max int64) {
	p := "sun.gc.generation." + g + "."
	b.str(p+"name", name).long(p+"capacity", UnitsBytes, capacity).long(p+"maxCapacity", UnitsBytes, max)
}

func space(b *builder, g, s, name string, used, capacity, max int64) {
	p := "sun.gc.generation." + g + ".space." + s + "."
	b.str(p+"name", name).long(p+"used", UnitsBytes, used).long(p+"capacity", UnitsBytes, capacity).long(p+"maxCapacity", UnitsBytes, max)
}

const mb = 1 << 20

// Layouts as seen with local Java 25 JVMs (-Xmx256m) after a GC.
func serialLayout(order binary.ByteOrder) []byte {
	b := newBuilder(order)
	b.long("sun.os.hrt.frequency", UnitsHertz, 10_000_000)
	b.str("sun.gc.policy.name", "Copy:MSC")
	collector(b, "0", "Serial young collection pauses", 3, 120_000)
	collector(b, "1", "Serial full collection pauses", 1, 50_000)
	gen(b, "0", "new", 85*mb, 85*mb)
	space(b, "0", "0", "eden", 0, 68*mb, 68*mb)
	space(b, "0", "1", "s0", 1*mb, 8*mb+512*1024, 8*mb+512*1024)
	space(b, "0", "2", "s1", 0, 8*mb+512*1024, 8*mb+512*1024)
	gen(b, "1", "old", 171*mb, 171*mb)
	space(b, "1", "0", "old", 3*mb, 171*mb, 171*mb)
	return b.bytes()
}

func parallelLayout() []byte {
	b := newBuilder(binary.LittleEndian)
	b.str("sun.gc.policy.name", "ParScav:MSC")
	collector(b, "0", "Parallel young collection pauses", 0, 0)
	collector(b, "1", "Parallel full collection pauses", 1, 30_000)
	gen(b, "0", "new", 85*mb, 85*mb)
	space(b, "0", "0", "eden", 0, 64*mb, 84*mb)
	space(b, "0", "1", "s0", 0, 10*mb+512*1024, 28*mb)
	space(b, "0", "2", "s1", 0, 10*mb+512*1024, 28*mb)
	gen(b, "1", "old", 171*mb, 171*mb)
	space(b, "1", "0", "old", 3*mb, 171*mb, 171*mb)
	return b.bytes()
}

func g1Layout() []byte {
	b := newBuilder(binary.LittleEndian)
	b.str("sun.gc.policy.name", "GarbageFirst")
	collector(b, "0", "G1 young collection pauses", 5, 100_000)
	collector(b, "1", "G1 full collection pauses", 2, 71_200)
	collector(b, "2", "G1 concurrent cycle pauses", 1, 10_000)
	gen(b, "0", "young", 5*mb, 256*mb)
	space(b, "0", "0", "eden", 0, 5*mb, 256*mb)
	space(b, "0", "1", "s0", 0, 0, 0)
	space(b, "0", "2", "s1", 0, 0, 256*mb)
	gen(b, "1", "old", 9*mb, 256*mb)
	space(b, "1", "0", "space", 3*mb, 9*mb, 256*mb)
	return b.bytes()
}

func zgcLayout() []byte {
	b := newBuilder(binary.LittleEndian)
	// No sun.gc.policy.name with ZGC; collector indices 0 and 2.
	collector(b, "0", "ZGC minor collection pauses", 4, 1_000)
	collector(b, "2", "ZGC major collection pauses", 16, 2_532)
	gen(b, "0", "young", 250*mb, 256*mb)
	space(b, "0", "0", "space", 0, 250*mb, 256*mb)
	gen(b, "1", "old", 6*mb, 256*mb)
	space(b, "1", "0", "space", 6*mb, 6*mb, 256*mb)
	return b.bytes()
}

func TestLayouts(t *testing.T) {
	cases := []struct {
		name                 string
		buf                  []byte
		gc                   string
		indices              []int
		used, committed, max int64
	}{
		{"serial-le", serialLayout(binary.LittleEndian), GCSerial, []int{0, 1}, 4 * mb, 256 * mb, 256 * mb},
		{"serial-be", serialLayout(binary.BigEndian), GCSerial, []int{0, 1}, 4 * mb, 256 * mb, 256 * mb},
		{"parallel", parallelLayout(), GCParallel, []int{0, 1}, 3 * mb, 256 * mb, 256 * mb},
		{"g1", g1Layout(), GCG1, []int{0, 1, 2}, 3 * mb, 14 * mb, 256 * mb},
		{"zgc", zgcLayout(), GCZ, []int{0, 2}, 6 * mb, 256 * mb, 256 * mb},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Parse(c.buf)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.GCType(); got != c.gc {
				t.Errorf("GCType = %q, want %q", got, c.gc)
			}
			var idx []int
			for _, col := range d.Collectors() {
				idx = append(idx, col.Index)
			}
			if !equalInts(idx, c.indices) {
				t.Errorf("collector indices = %v, want %v", idx, c.indices)
			}
			u, cm, mx := d.HeapTotals(d.GCType())
			if u != c.used || cm != c.committed || mx != c.max {
				t.Errorf("HeapTotals = %d/%d/%d, want %d/%d/%d", u, cm, mx, c.used, c.committed, c.max)
			}
		})
	}
}

func TestTicksWindowsFrequency(t *testing.T) {
	d, err := Parse(serialLayout(binary.LittleEndian))
	if err != nil {
		t.Fatal(err)
	}
	// 120000 ticks at 10^7 Hz = 12 ms.
	if got := d.TicksToMs(d.Collectors()[0].Time); got != 12 {
		t.Errorf("TicksToMs = %v, want 12", got)
	}
}

func TestParseErrors(t *testing.T) {
	good := serialLayout(binary.LittleEndian)
	mod := func(f func(b []byte) []byte) []byte {
		b := append([]byte(nil), good...)
		return f(b)
	}
	cases := map[string]struct {
		buf  []byte
		want error
	}{
		"short":          {good[:10], ErrTruncated},
		"magic":          {mod(func(b []byte) []byte { b[0] = 0; return b }), ErrBadMagic},
		"version":        {mod(func(b []byte) []byte { b[5] = 1; return b }), ErrBadVersion},
		"not accessible": {mod(func(b []byte) []byte { b[7] = 0; return b }), ErrNotAccessible},
		"entry overflow": {mod(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[32:], 1<<30); return b }), ErrTruncated},
		"cut entries":    {mod(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[8:], 60); return b[:60] }), ErrTruncated},
	}
	for name, c := range cases {
		if _, err := Parse(c.buf); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestClassification(t *testing.T) {
	kinds := map[string]string{
		"Serial young collection pauses":     KindYoung,
		"Serial full collection pauses":      KindFull,
		"G1 concurrent cycle pauses":         KindConcurrent,
		"ZGC minor collection pauses":        KindYoung,
		"ZGC major collection pauses":        KindOld,
		"Shenandoah partial":                 KindConcurrent,
		"Shenandoah full":                    KindFull,
		"Copy":                               KindYoung,
		"MSC":                                KindFull,
		"PSScavenge":                         KindYoung,
		"PSParallelCompact":                  KindFull,
		"G1 incremental collections":         KindYoung,
		"G1 stop-the-world full collections": KindFull,
		"something":                          KindUnknown,
	}
	for name, want := range kinds {
		if got := CollectorKind(name); got != want {
			t.Errorf("CollectorKind(%q) = %q, want %q", name, got, want)
		}
	}
	if p := TimeIsPause("ZGC major collection pauses"); p == nil || !*p {
		t.Error("ZGC pauses must be pauses")
	}
	if p := TimeIsPause("Z concurrent cycle"); p == nil || *p {
		t.Error("cycles are not pauses")
	}
	if TimeIsPause("Shenandoah partial") != nil {
		t.Error("Shenandoah is unknown")
	}
	versions := map[string]int{"25.0.4.1": 25, "1.8.0_392": 8, "21": 21, "17-ea": 17}
	for v, want := range versions {
		if got, ok := FeatureVersion(v); !ok || got != want {
			t.Errorf("FeatureVersion(%q) = %d, want %d", v, got, want)
		}
	}
}

// TestFixture checks the file from the test node against the values in PLAN.md.
func TestFixture(t *testing.T) {
	d, err := ReadFile(ctmp.File{Dir: ctmp.Dir{Root: "../../testdata"}, Name: "mpa-hsperf.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Len() != 182 || d.Major != 2 || d.Minor != 0 {
		t.Errorf("entries %d, version %d.%d", d.Len(), d.Major, d.Minor)
	}
	str := func(n, want string) {
		if got, _ := d.String(n); got != want {
			t.Errorf("%s = %q, want %q", n, got, want)
		}
	}
	long := func(n string, want int64) {
		if got, _ := d.Long(n); got != want {
			t.Errorf("%s = %d, want %d", n, got, want)
		}
	}
	str("java.rt.vmArgs", "-Xms128M -Xmx1907M")
	str("sun.rt.javaCommand", "server.jar")
	str("java.property.java.version", "25.0.4.1")
	str("java.property.java.vm.vendor", "Eclipse Adoptium")
	str("sun.gc.policy.name", "Copy:MSC")
	long("sun.os.hrt.frequency", 1_000_000_000)
	long("java.cls.loadedClasses", 26618)
	long("sun.rt.safepoints", 123)

	if d.GCType() != GCSerial {
		t.Errorf("GCType = %s", d.GCType())
	}
	cs := d.Collectors()
	if len(cs) != 2 || cs[0].Invocations != 116 || cs[1].Invocations != 4 {
		t.Fatalf("collectors = %+v", cs)
	}
	if ms := d.TicksToMs(cs[0].Time); ms < 2700 || ms > 2701 {
		t.Errorf("young GC time = %v ms, want 2.70 s", ms)
	}
	if ms := d.TicksToMs(cs[1].Time); ms < 526 || ms > 527 {
		t.Errorf("full GC time = %v ms, want 0.53 s", ms)
	}
	if ms := d.TicksToMs(mustLong(d, "sun.rt.safepointTime")); ms < 3232 || ms > 3233 {
		t.Errorf("safepoint time = %v ms, want 3.23 s", ms)
	}

	gs := d.Generations()
	if len(gs) != 2 || gs[0].Max != 636*mb || gs[1].Max != 1272*mb {
		t.Fatalf("generations = %+v", gs)
	}
	// Old gen: 512 MB (decimal) committed, 99.98 % used.
	if gs[1].Capacity != 512356352 || float64(gs[1].Used())/float64(gs[1].Capacity) < 0.9997 {
		t.Errorf("old gen used %d of %d", gs[1].Used(), gs[1].Capacity)
	}
	// Serial: max is the sum of the generations (636 + 1272 MiB, Xmx 1907M + alignment).
	if _, _, mx := d.HeapTotals(GCSerial); mx != 1908*mb {
		t.Errorf("heap max = %d", mx)
	}
	if m := mustLong(d, "sun.gc.metaspace.used"); m/mb != 109 { // 110 MB decimal
		t.Errorf("metaspace used = %d", m)
	}
	if sup, known := d.AttachSupported(); !sup || !known {
		t.Error("attach must be supported")
	}
}

func mustLong(d *Data, n string) int64 {
	v, _ := d.Long(n)
	return v
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
