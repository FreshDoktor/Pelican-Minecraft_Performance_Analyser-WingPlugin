package api

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/parse"
)

// Attach reasons.
const (
	ReasonDisabled    = "disabled"
	ReasonXrs         = "xrs"
	ReasonJVMStarting = "jvm_starting"
	ReasonUnknown     = "unknown"
)

// minSignalUptimeMs: below this JVM age no SIGQUIT is sent (signal handlers
// may not be installed yet).
const minSignalUptimeMs = 30_000

func ptr[T any](v T) *T { return &v }

func strPtr(d *hsperf.Data, name string) *string {
	if v, ok := d.String(name); ok {
		return &v
	}
	return nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func positive(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

// jvmTimes returns started_at (Unix ms) and uptime (ms) from sun.rt.createVmBeginTime.
func jvmTimes(d *hsperf.Data, nowMs int64) (*int64, *int64) {
	begin, ok := d.Long("sun.rt.createVmBeginTime")
	if !ok || begin <= 0 {
		return nil, nil
	}
	up := max(nowMs-begin, 0)
	return &begin, &up
}

func buildAttach(d *hsperf.Data, listener bool, uptimeMs *int64) AttachInfo {
	a := AttachInfo{ListenerRunning: listener}
	var args parse.VMArgs
	if d != nil {
		if sup, known := d.AttachSupported(); known {
			a.Supported = &sup
		}
		vmArgs, _ := d.String("java.rt.vmArgs")
		args = parse.ParseVMArgs(vmArgs)
	}
	switch {
	case a.Supported != nil && !*a.Supported, args.DisableAttach:
		a.Reason = ptr(ReasonDisabled)
	case listener:
		a.Available = true
	case d == nil:
		a.Reason = ptr(ReasonUnknown)
	case args.Xrs:
		a.Reason = ptr(ReasonXrs)
	case uptimeMs == nil || *uptimeMs < minSignalUptimeMs:
		a.Reason = ptr(ReasonJVMStarting)
	default:
		a.Available = true
	}
	return a
}

func buildJavaProcess(j container.JavaProcess, d *hsperf.Data, nowMs int64) *JavaProcess {
	p := &JavaProcess{HostPID: j.HostPID, ContainerPID: j.NSPID, User: nonEmpty(j.User), HsperfAvailable: d != nil}
	if d != nil {
		p.Version = strPtr(d, "java.property.java.version")
		p.VMName = strPtr(d, "java.property.java.vm.name")
		p.VMVendor = strPtr(d, "java.property.java.vm.vendor")
		p.StartedAt, p.UptimeMs = jvmTimes(d, nowMs)
	}
	return p
}

func collectorIDs(d *hsperf.Data) []CollectorID {
	out := []CollectorID{}
	for _, c := range d.Collectors() {
		out = append(out, CollectorID{Index: c.Index, Name: c.Name, Kind: hsperf.CollectorKind(c.Name), TimeIsPause: hsperf.TimeIsPause(c.Name)})
	}
	return out
}

// containerLimits converts the Docker limits.
func containerLimits(info container.Info, hostCPUs int, tmpfsBytes int64) ContainerLimits {
	l := ContainerLimits{
		MemoryLimitBytes:       positive(info.Memory),
		MemoryReservationBytes: positive(info.MemoryReservation),
		CPUQuotaUs:             positive(info.CPUQuota),
		CPUPeriodUs:            positive(info.CPUPeriod),
		CpusetCpus:             nonEmpty(info.CpusetCpus),
		HostCPUs:               hostCPUs,
		TmpfsSizeBytes:         tmpfsBytes,
	}
	switch {
	case info.NanoCPUs > 0:
		l.CPULimit = ptr(round3(float64(info.NanoCPUs) / 1e9))
	case info.CPUQuota > 0 && info.CPUPeriod > 0:
		l.CPULimit = ptr(round3(float64(info.CPUQuota) / float64(info.CPUPeriod)))
	}
	if n, ok := cpusetCount(info.CpusetCpus); ok {
		l.CpusetCount = &n
	}
	eff := float64(hostCPUs)
	if l.CPULimit != nil && *l.CPULimit < eff {
		eff = *l.CPULimit
	}
	if l.CpusetCount != nil && float64(*l.CpusetCount) < eff {
		eff = float64(*l.CpusetCount)
	}
	l.EffectiveCPUs = round3(eff)
	return l
}

// cpusetCount counts the CPUs of a cpuset list like "0-3,8,10-11".
func cpusetCount(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return 0, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return 0, false
			}
		}
		n += b - a + 1
	}
	return n, true
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// BuildVM builds GET /vm from hsperfdata and the container.
func BuildVM(d *hsperf.Data, info container.Info, listener bool, hostCPUs int, tmpfsBytes, nowMs int64) VMResponse {
	started, uptime := jvmTimes(d, nowMs)
	vmArgs, _ := d.String("java.rt.vmArgs")
	args := parse.ParseVMArgs(vmArgs)
	gcType := d.GCType()
	_, _, heapMax := d.HeapTotals(gcType)

	r := VMResponse{
		SampledAt: nowMs,
		Java: VMJava{
			Version:         strPtr(d, "java.property.java.version"),
			VMName:          strPtr(d, "java.property.java.vm.name"),
			VMVendor:        strPtr(d, "java.property.java.vm.vendor"),
			VMVersion:       strPtr(d, "java.property.java.vm.version"),
			VMInfo:          strPtr(d, "java.property.java.vm.info"),
			Home:            strPtr(d, "java.property.java.home"),
			InternalVersion: strPtr(d, "sun.rt.internalVersion"),
		},
		Launch: VMLaunch{
			VMArgs:      strPtr(d, "java.rt.vmArgs"),
			VMFlags:     strPtr(d, "java.rt.vmFlags"),
			JavaCommand: strPtr(d, "sun.rt.javaCommand"),
			ClassPath:   strPtr(d, "java.property.java.class.path"),
			StartedAt:   started,
			UptimeMs:    uptime,
		},
		MemorySettings: MemorySettings{
			XmsBytes:             args.Xms,
			XmxBytes:             args.Xmx,
			XssBytes:             args.Xss,
			MaxMetaspaceBytes:    args.MaxMetaspaceSize,
			MaxRAMPercentage:     args.MaxRAMPercentage,
			InitialRAMPercentage: args.InitialRAMPercentage,
			ExplicitGC:           args.GC,
			Xrs:                  args.Xrs,
			DisableAttach:        args.DisableAttach,
		},
		GC: VMGC{
			Type:       gcType,
			SelectedBy: "ergonomics",
			Policy:     strPtr(d, "sun.gc.policy.name"),
			Collectors: collectorIDs(d),
		},
		HeapMaxBytes: heapMax,
		Metaspace:    memoryArea(d, "sun.gc.metaspace."),
		Attach:       buildAttach(d, listener, uptime),
		Container:    containerLimits(info, hostCPUs, tmpfsBytes),
		FlagsError:   ptr(CodeNotImplemented), // replaced by the handler once VM.flags ran
	}
	if args.GC != nil {
		r.GC.SelectedBy = "flag"
	}
	if r.Java.Version != nil {
		if fv, ok := hsperf.FeatureVersion(*r.Java.Version); ok {
			r.Java.FeatureVersion = &fv
		}
	}
	return r
}

// BuildMetrics builds GET /metrics from hsperfdata.
func BuildMetrics(d *hsperf.Data, nowMs int64) MetricsResponse {
	started, uptime := jvmTimes(d, nowMs)
	gcType := d.GCType()
	used, committed, maxBytes := d.HeapTotals(gcType)
	long := func(n string) int64 { v, _ := d.Long(n); return v }

	r := MetricsResponse{
		SampledAt:   nowMs,
		StartedAt:   started,
		UptimeMs:    uptime,
		Heap:        HeapTotals{UsedBytes: used, CommittedBytes: committed, MaxBytes: maxBytes, Source: SourceAfterGC},
		Generations: []Generation{},
		Threads: ThreadCounts{
			Live: long("java.threads.live"), Daemon: long("java.threads.daemon"),
			Peak: long("java.threads.livePeak"), Started: long("java.threads.started"),
		},
		Classes: ClassCounts{
			Loaded: long("java.cls.loadedClasses"), Unloaded: long("java.cls.unloadedClasses"),
			SharedLoaded: long("java.cls.sharedLoadedClasses"), SharedUnloaded: long("java.cls.sharedUnloadedClasses"),
		},
		Safepoints: Safepoints{
			Count:      long("sun.rt.safepoints"),
			TimeMs:     d.TicksToMs(long("sun.rt.safepointTime")),
			SyncTimeMs: d.TicksToMs(long("sun.rt.safepointSyncTime")),
		},
		JIT: JIT{
			Compiles: long("sun.ci.totalCompiles"),
			TimeMs:   d.TicksToMs(long("java.ci.totalTime")),
			Threads:  long("sun.ci.threads"),
		},
	}
	r.Classes.Current = r.Classes.Loaded + r.Classes.SharedLoaded - r.Classes.Unloaded - r.Classes.SharedUnloaded

	for _, g := range d.Generations() {
		og := Generation{Index: g.Index, Name: g.Name, Kind: hsperf.GenerationKind(g.Name),
			UsedBytes: g.Used(), CommittedBytes: g.Capacity, MaxBytes: g.Max, Source: SourceAfterGC, Spaces: []Space{}}
		for _, s := range g.Spaces {
			og.Spaces = append(og.Spaces, Space{Index: s.Index, Name: s.Name, UsedBytes: s.Used, CommittedBytes: s.Capacity, MaxBytes: s.Max, Source: SourceAfterGC})
		}
		r.Generations = append(r.Generations, og)
	}
	r.Metaspace = memoryArea(d, "sun.gc.metaspace.")
	r.ClassSpace = memoryArea(d, "sun.gc.compressedclassspace.")

	r.GC = MetricsGC{
		Type:         gcType,
		LastCause:    strPtr(d, "sun.gc.lastCause"),
		CurrentCause: strPtr(d, "sun.gc.cause"),
		Collectors:   []CollectorMetrics{},
	}
	var totalTicks int64
	for _, c := range d.Collectors() {
		cm := CollectorMetrics{
			Index: c.Index, Name: c.Name, Kind: hsperf.CollectorKind(c.Name), TimeIsPause: hsperf.TimeIsPause(c.Name),
			Count: c.Invocations, TimeMs: d.TicksToMs(c.Time),
		}
		if c.Invocations > 0 && c.LastEntry > 0 {
			cm.LastStartedUptimeMs = ptr(d.TicksToMs(c.LastEntry))
			if started != nil {
				cm.LastStartedAt = ptr(*started + int64(math.Round(*cm.LastStartedUptimeMs)))
			}
			if c.LastExit >= c.LastEntry {
				cm.LastDurationMs = ptr(d.TicksToMs(c.LastExit - c.LastEntry))
			}
		}
		r.GC.Count += c.Invocations
		totalTicks += c.Time
		r.GC.Collectors = append(r.GC.Collectors, cm)
	}
	r.GC.TimeMs = d.TicksToMs(totalTicks)
	return r
}

func memoryArea(d *hsperf.Data, prefix string) *MemoryArea {
	u, ok := d.Long(prefix + "used")
	if !ok {
		return nil
	}
	c, _ := d.Long(prefix + "capacity")
	m, _ := d.Long(prefix + "maxCapacity")
	return &MemoryArea{UsedBytes: u, CommittedBytes: c, MaxBytes: m, Source: SourceAfterGC}
}

// Java thread states in by_state.
var threadStates = []string{"NEW", "RUNNABLE", "BLOCKED", "WAITING", "TIMED_WAITING", "TERMINATED"}

// BuildThreads builds GET /threads from a parsed dump.
func BuildThreads(dump parse.ThreadDump, raw string, nowMs int64, durationMs float64) ThreadsResponse {
	r := ThreadsResponse{
		SampledAt:  nowMs,
		DurationMs: round3(durationMs),
		DumpedAt:   nonEmpty(dump.Timestamp),
		VM:         nonEmpty(dump.VM),
		Threads:    []ThreadInfo{},
		Deadlocks:  []DeadlockInfo{},
		Raw:        raw,
		Summary:    ThreadSummary{ByState: map[string]int{}},
	}
	for _, s := range threadStates {
		r.Summary.ByState[s] = 0
	}
	for _, t := range dump.Threads {
		ti := ThreadInfo{
			Name: t.Name, JavaID: t.JavaID, OSID: t.OSID, VMThread: t.VMThread, Daemon: t.Daemon,
			Priority: t.Priority, OSPriority: t.OSPriority, CPUMs: t.CPUMs, ElapsedMs: t.ElapsedMs,
			State: nonEmpty(t.State), StateDetail: nonEmpty(t.StateDetail), OSState: nonEmpty(t.OSState),
			Stack: []FrameInfo{}, LockedSynchronizers: lockInfos(t.Synchronizers), Info: append([]string{}, t.Info...),
		}
		for _, f := range t.Stack {
			ti.Stack = append(ti.Stack, FrameInfo{Method: f.Method, Module: nonEmpty(f.Module), File: nonEmpty(f.File),
				Line: f.Line, Native: f.Native, Locks: lockInfos(f.Locks)})
		}
		r.Summary.Total++
		if t.VMThread {
			r.Summary.VM++
		} else {
			r.Summary.Java++
		}
		if t.Daemon {
			r.Summary.Daemon++
		}
		if t.State != "" {
			r.Summary.ByState[t.State]++
		}
		r.Threads = append(r.Threads, ti)
	}
	for _, dl := range dump.Deadlocks {
		di := DeadlockInfo{Threads: []DeadlockThread{}}
		for _, t := range dl.Threads {
			di.Threads = append(di.Threads, DeadlockThread{Name: t.Name, WaitingFor: nonEmpty(t.WaitingFor), HeldBy: nonEmpty(t.HeldBy)})
		}
		if len(di.Threads) > 0 {
			r.Deadlocks = append(r.Deadlocks, di)
		}
	}
	if dump.JNIGlobal != nil && dump.JNIWeak != nil {
		r.JNIRefs = &JNIRefs{Global: *dump.JNIGlobal, Weak: *dump.JNIWeak}
	}
	return r
}

func lockInfos(ls []parse.Lock) []LockInfo {
	out := []LockInfo{}
	for _, l := range ls {
		out = append(out, LockInfo{Type: l.Type, Address: nonEmpty(l.Address), Class: nonEmpty(l.Class)})
	}
	return out
}

// Histogram limits.
const (
	DefaultHistogramLimit = 2000
	MaxHistogramLimit     = 100000
)

// BuildHistogram builds GET /histogram, keeping the limit largest classes.
// Totals come from the "Total" line and the highest rank, so they cover
// every class even when the input was shortened.
func BuildHistogram(h parse.Histogram, limit int, nowMs int64, durationMs float64) HistogramResponse {
	classes := append([]parse.HistogramClass(nil), h.Classes...)
	sort.SliceStable(classes, func(i, j int) bool { return classes[i].Bytes > classes[j].Bytes })
	r := HistogramResponse{
		SampledAt: nowMs, DurationMs: round3(durationMs), All: true, Limit: limit,
		Total:   HistogramTotal{Classes: len(classes), Instances: h.TotalInstances, Bytes: h.TotalBytes},
		Classes: []HistogramEntry{},
	}
	var inst, byt int64
	for _, c := range classes {
		if int(c.Rank) > r.Total.Classes {
			r.Total.Classes = int(c.Rank)
		}
		if len(r.Classes) >= limit {
			continue
		}
		inst += c.Instances
		byt += c.Bytes
		r.Classes = append(r.Classes, HistogramEntry{Rank: c.Rank, Name: c.Name, ElementType: c.ElementType,
			ArrayDepth: c.ArrayDepth, Module: nonEmpty(c.Module), Instances: c.Instances, Bytes: c.Bytes})
	}
	if h.TotalBytes == 0 && h.TotalInstances == 0 {
		for _, c := range classes {
			r.Total.Instances += c.Instances
			r.Total.Bytes += c.Bytes
		}
	}
	r.Omitted = HistogramTotal{Classes: r.Total.Classes - len(r.Classes), Instances: r.Total.Instances - inst, Bytes: r.Total.Bytes - byt}
	return r
}

// BuildProfileThreads converts parsed collapsed stacks.
func BuildProfileThreads(ts []parse.ProfileThread) ([]ProfileThread, int64) {
	out := []ProfileThread{}
	var total int64
	for _, t := range ts {
		pt := ProfileThread{Name: t.Name, TID: t.TID, Count: t.Count, Stacks: []ProfileStack{}}
		for _, s := range t.Stacks {
			pt.Stacks = append(pt.Stacks, ProfileStack{Stack: s.Stack, Count: s.Count})
		}
		total += t.Count
		out = append(out, pt)
	}
	return out, total
}

func serverInfo(info container.Info) ServerInfo {
	si := ServerInfo{UUID: info.UUID, ContainerState: info.State, Running: info.Running}
	if !info.StartedAt.IsZero() {
		si.StartedAt = ptr(info.StartedAt.UnixMilli())
	}
	return si
}

// BuildLiveHeap parses GC.heap_info.
func BuildLiveHeap(raw string, nowMs int64) (*LiveHeap, bool) {
	h, ok := parse.ParseHeapInfo(raw)
	if !ok {
		return nil, false
	}
	lh := &LiveHeap{SampledAt: nowMs, Source: SourceLive, UsedBytes: h.Used, CommittedBytes: h.Committed, MaxBytes: h.Max, Raw: raw}
	lh.Metaspace = liveMetaspace(raw)
	return lh, true
}

func liveMetaspace(text string) *LiveMetaspace {
	m, ok := parse.ParseMetaspace(text)
	if !ok {
		return nil
	}
	return &LiveMetaspace{UsedBytes: m.Used, CommittedBytes: m.Committed, ReservedBytes: m.Reserved, Source: SourceLive}
}
