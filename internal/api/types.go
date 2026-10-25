package api

// Response types of API v1. They are the contract in docs/API.md: every field
// is always present, nullable ones are pointers. Slices are never nil.

// APIVersion is the major version of the HTTP API (path prefix /api/v1).
const APIVersion = 1

// Features a bridge build may support.
const (
	FeatureStatus    = "status"
	FeatureVM        = "vm"
	FeatureMetrics   = "metrics"
	FeatureThreads   = "threads"
	FeatureHistogram = "histogram"
	FeatureVMFlags   = "vm_flags"
	FeatureLiveHeap  = "live_heap"
	FeatureProfiles  = "profiles"
)

// ErrorBody is the body of every error response.
type ErrorBody struct {
	Error   string         `json:"error"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// Value sources for memory values.
const (
	SourceAfterGC = "after_gc" // hsperfdata, updated by the JVM at each GC
	SourceLive    = "live"     // GC.heap_info via attach
)

// NodeStatusResponse is GET /api/v1/status (no server).
type NodeStatusResponse struct {
	BridgeVersion string     `json:"bridge_version"`
	APIVersion    int        `json:"api_version"`
	Features      []string   `json:"features"`
	StartedAt     int64      `json:"started_at"`
	UptimeMs      int64      `json:"uptime_ms"`
	SampledAt     int64      `json:"sampled_at"`
	TLS           bool       `json:"tls"`
	Limits        NodeLimits `json:"limits"`
}

// NodeLimits are the limits from mpa-bridge.yml.
type NodeLimits struct {
	MaxConcurrentProfiles int `json:"max_concurrent_profiles"`
	MaxProfileSeconds     int `json:"max_profile_seconds"`
}

// BridgeInfo identifies the bridge in /status.
type BridgeInfo struct {
	Version    string   `json:"version"`
	APIVersion int      `json:"api_version"`
	Features   []string `json:"features"`
}

// StatusResponse is GET /status.
type StatusResponse struct {
	State     string       `json:"state"`
	Bridge    BridgeInfo   `json:"bridge"`
	Server    ServerInfo   `json:"server"`
	Java      *JavaProcess `json:"java"`
	Attach    *AttachInfo  `json:"attach"`
	SampledAt int64        `json:"sampled_at"`
}

// ServerInfo describes the container.
type ServerInfo struct {
	UUID           string `json:"uuid"`
	ContainerState string `json:"container_state"`
	Running        bool   `json:"running"`
	StartedAt      *int64 `json:"started_at"`
}

// JavaProcess describes the Java process found in the container.
type JavaProcess struct {
	HostPID         int     `json:"host_pid"`
	ContainerPID    int     `json:"container_pid"`
	User            *string `json:"user"`
	HsperfAvailable bool    `json:"hsperf_available"`
	Version         *string `json:"version"`
	VMName          *string `json:"vm_name"`
	VMVendor        *string `json:"vm_vendor"`
	StartedAt       *int64  `json:"started_at"`
	UptimeMs        *int64  `json:"uptime_ms"`
}

// AttachInfo tells whether the bridge can attach to the JVM.
type AttachInfo struct {
	Supported       *bool   `json:"supported"`
	ListenerRunning bool    `json:"listener_running"`
	Available       bool    `json:"available"`
	Reason          *string `json:"reason"`
}

// VMResponse is GET /vm.
type VMResponse struct {
	SampledAt      int64             `json:"sampled_at"`
	Java           VMJava            `json:"java"`
	Launch         VMLaunch          `json:"launch"`
	MemorySettings MemorySettings    `json:"memory_settings"`
	GC             VMGC              `json:"gc"`
	HeapMaxBytes   int64             `json:"heap_max_bytes"`
	Metaspace      *MemoryArea       `json:"metaspace"`
	Attach         AttachInfo        `json:"attach"`
	Container      ContainerLimits   `json:"container"`
	Flags          map[string]string `json:"flags"`
	FlagsError     *string           `json:"flags_error"`
}

// VMJava is the Java runtime.
type VMJava struct {
	Version         *string `json:"version"`
	FeatureVersion  *int    `json:"feature_version"`
	VMName          *string `json:"vm_name"`
	VMVendor        *string `json:"vm_vendor"`
	VMVersion       *string `json:"vm_version"`
	VMInfo          *string `json:"vm_info"`
	Home            *string `json:"home"`
	InternalVersion *string `json:"internal_version"`
}

// VMLaunch is how the JVM was started.
type VMLaunch struct {
	VMArgs      *string `json:"vm_args"`
	VMFlags     *string `json:"vm_flags"`
	JavaCommand *string `json:"java_command"`
	ClassPath   *string `json:"class_path"`
	StartedAt   *int64  `json:"started_at"`
	UptimeMs    *int64  `json:"uptime_ms"`
}

// MemorySettings are parsed from vm_args.
type MemorySettings struct {
	XmsBytes             *int64   `json:"xms_bytes"`
	XmxBytes             *int64   `json:"xmx_bytes"`
	XssBytes             *int64   `json:"xss_bytes"`
	MaxMetaspaceBytes    *int64   `json:"max_metaspace_bytes"`
	MaxRAMPercentage     *float64 `json:"max_ram_percentage"`
	InitialRAMPercentage *float64 `json:"initial_ram_percentage"`
	ExplicitGC           *string  `json:"explicit_gc"`
	Xrs                  bool     `json:"xrs"`
	DisableAttach        bool     `json:"disable_attach"`
}

// VMGC describes the collector.
type VMGC struct {
	Type       string        `json:"type"`
	SelectedBy string        `json:"selected_by"`
	Policy     *string       `json:"policy"`
	Collectors []CollectorID `json:"collectors"`
}

// CollectorID identifies a collector.
type CollectorID struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	TimeIsPause *bool  `json:"time_is_pause"`
}

// ContainerLimits are the Docker limits of the server container.
type ContainerLimits struct {
	MemoryLimitBytes       *int64   `json:"memory_limit_bytes"`
	MemoryReservationBytes *int64   `json:"memory_reservation_bytes"`
	CPULimit               *float64 `json:"cpu_limit"`
	CPUQuotaUs             *int64   `json:"cpu_quota_us"`
	CPUPeriodUs            *int64   `json:"cpu_period_us"`
	CpusetCpus             *string  `json:"cpuset_cpus"`
	CpusetCount            *int     `json:"cpuset_count"`
	HostCPUs               int      `json:"host_cpus"`
	EffectiveCPUs          float64  `json:"effective_cpus"`
	TmpfsSizeBytes         int64    `json:"tmpfs_size_bytes"`
}

// MetricsResponse is GET /metrics.
type MetricsResponse struct {
	SampledAt     int64        `json:"sampled_at"`
	UptimeMs      *int64       `json:"uptime_ms"`
	Heap          HeapTotals   `json:"heap"`
	Generations   []Generation `json:"generations"`
	Metaspace     *MemoryArea  `json:"metaspace"`
	ClassSpace    *MemoryArea  `json:"class_space"`
	GC            MetricsGC    `json:"gc"`
	Threads       ThreadCounts `json:"threads"`
	Classes       ClassCounts  `json:"classes"`
	Safepoints    Safepoints   `json:"safepoints"`
	JIT           JIT          `json:"jit"`
	LiveHeap      *LiveHeap    `json:"live_heap"`
	LiveHeapError *string      `json:"live_heap_error"`
}

// HeapTotals are the totals over all generations.
type HeapTotals struct {
	UsedBytes      int64  `json:"used_bytes"`
	CommittedBytes int64  `json:"committed_bytes"`
	MaxBytes       int64  `json:"max_bytes"`
	Source         string `json:"source"`
}

// Generation is one heap generation.
type Generation struct {
	Index          int     `json:"index"`
	Name           string  `json:"name"`
	Kind           string  `json:"kind"`
	UsedBytes      int64   `json:"used_bytes"`
	CommittedBytes int64   `json:"committed_bytes"`
	MaxBytes       int64   `json:"max_bytes"`
	Source         string  `json:"source"`
	Spaces         []Space `json:"spaces"`
}

// Space is one space of a generation.
type Space struct {
	Index          int    `json:"index"`
	Name           string `json:"name"`
	UsedBytes      int64  `json:"used_bytes"`
	CommittedBytes int64  `json:"committed_bytes"`
	MaxBytes       int64  `json:"max_bytes"`
	Source         string `json:"source"`
}

// MemoryArea is a non-heap area (metaspace, class space).
type MemoryArea struct {
	UsedBytes      int64  `json:"used_bytes"`
	CommittedBytes int64  `json:"committed_bytes"`
	MaxBytes       int64  `json:"max_bytes"`
	Source         string `json:"source"`
}

// MetricsGC holds the cumulative GC counters.
type MetricsGC struct {
	Type         string             `json:"type"`
	Count        int64              `json:"count"`
	TimeMs       float64            `json:"time_ms"`
	LastCause    *string            `json:"last_cause"`
	CurrentCause *string            `json:"current_cause"`
	Collectors   []CollectorMetrics `json:"collectors"`
}

// CollectorMetrics are the counters of one collector.
type CollectorMetrics struct {
	Index               int      `json:"index"`
	Name                string   `json:"name"`
	Kind                string   `json:"kind"`
	TimeIsPause         *bool    `json:"time_is_pause"`
	Count               int64    `json:"count"`
	TimeMs              float64  `json:"time_ms"`
	LastStartedUptimeMs *float64 `json:"last_started_uptime_ms"`
	LastStartedAt       *int64   `json:"last_started_at"`
	LastDurationMs      *float64 `json:"last_duration_ms"`
}

// ThreadCounts are java.threads.*.
type ThreadCounts struct {
	Live    int64 `json:"live"`
	Daemon  int64 `json:"daemon"`
	Peak    int64 `json:"peak"`
	Started int64 `json:"started"`
}

// ClassCounts are java.cls.*.
type ClassCounts struct {
	Loaded         int64 `json:"loaded"`
	Unloaded       int64 `json:"unloaded"`
	SharedLoaded   int64 `json:"shared_loaded"`
	SharedUnloaded int64 `json:"shared_unloaded"`
	Current        int64 `json:"current"`
}

// Safepoints are sun.rt.safepoint*.
type Safepoints struct {
	Count      int64   `json:"count"`
	TimeMs     float64 `json:"time_ms"`
	SyncTimeMs float64 `json:"sync_time_ms"`
}

// JIT are the compiler counters.
type JIT struct {
	Compiles int64   `json:"compiles"`
	TimeMs   float64 `json:"time_ms"`
	Threads  int64   `json:"threads"`
}

// LiveHeap is the current heap from GC.heap_info (milestone 2, ?live=1).
type LiveHeap struct {
	SampledAt      int64  `json:"sampled_at"`
	Source         string `json:"source"`
	UsedBytes      int64  `json:"used_bytes"`
	CommittedBytes *int64 `json:"committed_bytes"`
	MaxBytes       *int64 `json:"max_bytes"`
	Raw            string `json:"raw"`
}

// ThreadsResponse is GET /threads.
type ThreadsResponse struct {
	SampledAt  int64          `json:"sampled_at"`
	DurationMs float64        `json:"duration_ms"`
	DumpedAt   *string        `json:"dumped_at_local"`
	VM         *string        `json:"vm"`
	Summary    ThreadSummary  `json:"summary"`
	Threads    []ThreadInfo   `json:"threads"`
	Deadlocks  []DeadlockInfo `json:"deadlocks"`
	JNIRefs    *JNIRefs       `json:"jni_refs"`
	Raw        string         `json:"raw"`
}

// ThreadSummary counts threads.
type ThreadSummary struct {
	Total   int            `json:"total"`
	Java    int            `json:"java"`
	VM      int            `json:"vm"`
	Daemon  int            `json:"daemon"`
	ByState map[string]int `json:"by_state"`
}

// ThreadInfo is one thread.
type ThreadInfo struct {
	Name                string      `json:"name"`
	JavaID              *int64      `json:"java_id"`
	OSID                *int64      `json:"os_id"`
	VMThread            bool        `json:"vm_thread"`
	Daemon              bool        `json:"daemon"`
	Priority            *int64      `json:"priority"`
	OSPriority          *int64      `json:"os_priority"`
	CPUMs               *float64    `json:"cpu_ms"`
	ElapsedMs           *float64    `json:"elapsed_ms"`
	State               *string     `json:"state"`
	StateDetail         *string     `json:"state_detail"`
	OSState             *string     `json:"os_state"`
	Stack               []FrameInfo `json:"stack"`
	LockedSynchronizers []LockInfo  `json:"locked_synchronizers"`
	Info                []string    `json:"info"`
}

// FrameInfo is one stack frame (top of stack first).
type FrameInfo struct {
	Method string     `json:"method"`
	Module *string    `json:"module"`
	File   *string    `json:"file"`
	Line   *int64     `json:"line"`
	Native bool       `json:"native"`
	Locks  []LockInfo `json:"locks"`
}

// LockInfo is a monitor or synchronizer.
type LockInfo struct {
	Type    string  `json:"type"`
	Address *string `json:"address"`
	Class   *string `json:"class"`
}

// DeadlockInfo is one deadlock cycle.
type DeadlockInfo struct {
	Threads []DeadlockThread `json:"threads"`
}

// DeadlockThread is one member of a deadlock cycle.
type DeadlockThread struct {
	Name       string  `json:"name"`
	WaitingFor *string `json:"waiting_for"`
	HeldBy     *string `json:"held_by"`
}

// JNIRefs are the JNI reference counts at the end of a dump.
type JNIRefs struct {
	Global int64 `json:"global"`
	Weak   int64 `json:"weak"`
}

// HistogramResponse is GET /histogram.
type HistogramResponse struct {
	SampledAt  int64            `json:"sampled_at"`
	DurationMs float64          `json:"duration_ms"`
	All        bool             `json:"all"`
	Limit      int              `json:"limit"`
	Total      HistogramTotal   `json:"total"`
	Omitted    HistogramTotal   `json:"omitted"`
	Classes    []HistogramEntry `json:"classes"`
}

// HistogramTotal sums classes.
type HistogramTotal struct {
	Classes   int   `json:"classes"`
	Instances int64 `json:"instances"`
	Bytes     int64 `json:"bytes"`
}

// HistogramEntry is one class.
type HistogramEntry struct {
	Rank        int64   `json:"rank"`
	Name        string  `json:"name"`
	ElementType string  `json:"element_type"`
	ArrayDepth  int     `json:"array_depth"`
	Module      *string `json:"module"`
	Instances   int64   `json:"instances"`
	Bytes       int64   `json:"bytes"`
}

// ProfileRequest is the body of POST /profiles.
type ProfileRequest struct {
	Seconds    int     `json:"seconds"`
	Mode       *string `json:"mode"`
	IntervalMs *int    `json:"interval_ms"`
}

// ProfileList is GET /profiles.
type ProfileList struct {
	Profiles []Profile `json:"profiles"`
}

// Profile is a profiling run (POST /profiles, GET /profiles/{id}).
type Profile struct {
	ID                 string         `json:"id"`
	ServerUUID         string         `json:"server_uuid"`
	Mode               string         `json:"mode"`
	Seconds            int            `json:"seconds"`
	IntervalMs         *int           `json:"interval_ms"`
	AllocIntervalBytes *int64         `json:"alloc_interval_bytes"`
	State              string         `json:"state"`
	StartedAt          int64          `json:"started_at"`
	EndsAt             int64          `json:"ends_at"`
	FinishedAt         *int64         `json:"finished_at"`
	ElapsedMs          int64          `json:"elapsed_ms"`
	Progress           float64        `json:"progress"`
	ExpiresAt          *int64         `json:"expires_at"`
	Error              *ErrorBody     `json:"error"`
	Result             *ProfileResult `json:"result"`
}

// ProfileResult is the result of a finished profile.
type ProfileResult struct {
	DurationMs float64           `json:"duration_ms"`
	Unit       string            `json:"unit"`
	IntervalMs *int              `json:"interval_ms"`
	Total      int64             `json:"total"`
	Warnings   []string          `json:"warnings"`
	Threads    []ProfileThread   `json:"threads"`
	GC         ProfileGC         `json:"gc"`
	Safepoints ProfileSafepoints `json:"safepoints"`
	CPU        ProfileCPU        `json:"cpu"`
}

// ProfileThread groups the stacks of one thread.
type ProfileThread struct {
	Name   string         `json:"name"`
	TID    *int64         `json:"tid"`
	Count  int64          `json:"count"`
	Stacks []ProfileStack `json:"stacks"`
}

// ProfileStack is one collapsed stack, root first, frames separated by ';'.
type ProfileStack struct {
	Stack string `json:"stack"`
	Count int64  `json:"count"`
}

// ProfileGC are the GC activity during a profile.
type ProfileGC struct {
	Count          int64              `json:"count"`
	TimeMs         float64            `json:"time_ms"`
	Collectors     []ProfileCollector `json:"collectors"`
	Pauses         []GCPause          `json:"pauses"`
	PausesComplete bool               `json:"pauses_complete"`
}

// ProfileCollector is the delta of one collector during a profile.
type ProfileCollector struct {
	Index       int     `json:"index"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	TimeIsPause *bool   `json:"time_is_pause"`
	Count       int64   `json:"count"`
	TimeMs      float64 `json:"time_ms"`
}

// GCPause is one observed collection.
type GCPause struct {
	CollectorIndex int     `json:"collector_index"`
	Kind           string  `json:"kind"`
	StartOffsetMs  float64 `json:"start_offset_ms"`
	DurationMs     float64 `json:"duration_ms"`
}

// ProfileSafepoints are the safepoint deltas during a profile.
type ProfileSafepoints struct {
	Count  int64   `json:"count"`
	TimeMs float64 `json:"time_ms"`
}

// ProfileCPU is the CPU time of the Java process during a profile.
type ProfileCPU struct {
	ProcessMs      float64 `json:"process_ms"`
	ProcessPercent float64 `json:"process_percent"`
}

// Server states in /status (the error code the data endpoints would return, or ready).
const StateReady = "ready"

// Profile states.
const (
	ProfileRunning    = "running"
	ProfileProcessing = "processing"
	ProfileDone       = "done"
	ProfileFailed     = "failed"
)

// Profile result units.
const (
	UnitSamples = "samples" // wall, cpu: count × interval_ms = time
	UnitBytes   = "bytes"   // alloc: count = allocated bytes
)

// Profile result warnings.
const (
	WarningNoSamples           = "no_samples"
	WarningServerThreadMissing = "server_thread_missing"
	WarningTruncated           = "truncated"
)
