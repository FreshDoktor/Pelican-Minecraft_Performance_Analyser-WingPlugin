package profile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/parse"
)

// Tick monitor (docs/API.md, "GET /ticks"): per server a small async-profiler
// session, read every TickWindow.
//
//   - sampling (first window): wall-clock samples of the Server thread tell
//     the mapping of the tick loop and whether its frames are MinecraftServer's.
//     The share of samples outside the idle methods is the tick load; with
//     20 TPS the average tick takes load × 50 ms.
//   - counting (from then on, for good): async-profiler instruments tickServer
//     and counts its calls, the TPS are exact. The tick time comes from the
//     kernel: the time the Server thread ran or waited for a CPU
//     (/proc/.../schedstat), plus its share of the JVM's safepoints (GC
//     pauses), divided by the ticks. Without idle time left (TPS below 19.5)
//     a tick takes the window divided by the ticks, also time the thread was
//     blocked. Instrumenting retransforms MinecraftServer, once per session.
//
// async-profiler 4.1 has no method durations (no --trace) and supports
// several events only with JFR output, hence counting plus the kernel's
// times. Sampling stays the mode where counting can't work (tick frames of
// another class).
const (
	TickWindow      = 5 * time.Second
	TickHistory     = 120 // windows, 10 minutes
	TickIdleTimeout = 60 * time.Second
	TickTargetTPS   = 20
	TickIntervalMs  = 20 // wall interval; with 50 ms ticks the phases 0, 20, 40, 10, 30 ms alternate

	TickMethodSampling = "sampling"
	TickMethodCounting = "counting"

	TickStateStarting    = "starting"
	TickStateOK          = "ok"
	TickStateIdle        = "idle"
	TickStateSuspended   = "suspended"
	TickStateUnsupported = "unsupported"
	TickStateError       = "error"

	TickReasonFirstWindow    = "first_window"
	TickReasonProfileRunning = "profile_running"
	TickReasonNoServerThread = "no_server_thread"
	TickReasonUnknownMapping = "unknown_mapping"

	// ServerThread is the name of the tick loop's thread.
	ServerThread = "Server thread"
)

const (
	tickTmpReserve = 4 << 20
	tickMaxOutput  = 8 << 20
	tickOpTimeout  = 15 * time.Second
	overloadLoad   = 0.95  // sampling: no idle time left, the samples can't tell the TPS
	keepUpTPS      = 19.5  // counting: below, the server falls behind
	idleLoad       = 0.005 // below: the Server thread has hardly anything to do (idle)
	threadDumpWait = time.Minute
)

// ErrTooManyMonitors: max_tick_monitors reached on this node.
var ErrTooManyMonitors = errors.New("profile: too many tick monitors")

// TickWindowResult is one measured window. Pointers are nil when unknown.
type TickWindowResult struct {
	StartedAt  time.Time
	EndedAt    time.Time
	State      string // ok, idle, suspended, unsupported
	Method     string // "" for suspended
	Samples    int64  // sampling: samples of the Server thread
	Ticks      int64  // counting: calls of tickServer
	Load       *float64
	TPS        *float64
	TickMs     *float64
	Overloaded bool
}

// TickSnapshot is the state of a server's tick monitor.
type TickSnapshot struct {
	State     string
	Reason    string
	Method    string // of the running session, "" if none
	Mapping   string
	StartedAt time.Time
	Err       *Error
	Windows   []TickWindowResult // oldest first
}

// Ticks runs the tick monitors of all servers. A monitor starts with the
// first Get and ends TickIdleTimeout after the last one, or with its JVM.
type Ticks struct {
	Max      int
	Profiles *Manager // library, loader and files are shared with the profiles
	// ThreadID finds the Server thread by its native name (container.Proc).
	ThreadID func(pid int, name string) (tid int, named bool, err error)
	// Jcmd runs Thread.print when the JVM has no native thread names (Java 8).
	Jcmd func(ctx context.Context, t attach.Target, cmd string) (string, error)
	// HostTID turns the Server thread's id into the host's (container.Proc).
	HostTID func(pid, nsTID int) (int, error)
	// SchedStat returns the ns a thread ran and waited for a CPU (container.Proc);
	// nil: counting windows have no tick time while the server keeps up.
	SchedStat func(pid, tid int) (run, wait int64, err error)
	Log       *slog.Logger

	// Overridable in tests.
	Now   func() time.Time
	Every func(d time.Duration) (<-chan time.Time, func())
	NewID func() string

	mu     sync.Mutex
	mons   map[string]*monitor
	yields map[int]int // host PID → profiles running there
}

type monitor struct {
	k    *Ticks
	id   string
	uuid string
	quit chan struct{}

	// guarded by k.mu
	target    Target
	lastSeen  time.Time
	startedAt time.Time
	state     string
	reason    string
	method    string
	mapping   string
	err       *Error
	windows   []TickWindowResult
	closed    bool

	op        sync.Mutex // one profiler command at a time; guards the fields below
	active    bool
	mode      string
	tid       int        // Server thread in the container's PID namespace
	hostTID   int        // the same thread as the host knows it, 0 until looked up
	since     time.Time  // start of the session, or the last dump
	lastCount int64      // counting: calls of tickServer at since
	busy0     busySample // counting: times of the Server thread at since
	classOK   bool       // the tick frames are of ServerClass, which counting instruments
	prepared  bool       // free space checked, library in place
	nextDump  time.Time
	noSamples bool // logged that a window had no samples of the Server thread
}

func (k *Ticks) init() {
	if k.mons == nil {
		k.mons = map[string]*monitor{}
		k.yields = map[int]int{}
	}
	if k.Now == nil {
		k.Now = time.Now
	}
	if k.Every == nil {
		k.Every = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	if k.Log == nil {
		k.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	k.Profiles.mu.Lock()
	k.Profiles.init()
	k.Profiles.mu.Unlock()
}

// Get returns the tick monitor of a server and starts it if needed. The
// attach listener must already run. A new JVM (other PID or start time)
// starts a new monitor; the history of the old one is void.
func (k *Ticks) Get(ctx context.Context, t Target) (TickSnapshot, error) {
	k.mu.Lock()
	k.init()
	now := k.Now()
	m := k.mons[t.ServerUUID]
	if m != nil && (m.target.Attach.HostPID != t.Attach.HostPID || m.target.JVMStartedAt != t.JVMStartedAt) {
		k.endLocked(m)
		m = nil
	}
	if m != nil {
		m.lastSeen, m.target = now, t
		s := k.snapshotLocked(m)
		k.mu.Unlock()
		return s, nil
	}
	if len(k.mons) >= k.Max {
		k.mu.Unlock()
		return TickSnapshot{}, ErrTooManyMonitors
	}
	id := newID()
	if k.NewID != nil {
		id = k.NewID()
	}
	m = &monitor{k: k, id: id, uuid: t.ServerUUID, quit: make(chan struct{}), target: t,
		lastSeen: now, startedAt: now, state: TickStateStarting, reason: TickReasonFirstWindow}
	k.mons[t.ServerUUID] = m
	k.mu.Unlock()

	m.op.Lock()
	err := m.begin(ctx)
	m.op.Unlock()
	if err != nil {
		k.mu.Lock()
		if k.mons[t.ServerUUID] == m {
			k.endLocked(m)
		}
		k.mu.Unlock()
		m.removeFiles(t)
		return TickSnapshot{}, err
	}
	k.Log.Info("tick monitor started", "uuid", t.ServerUUID, "id", id)
	go m.loop()
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.snapshotLocked(m), nil
}

// Yield stops the tick monitors of a JVM for a profile (async-profiler has
// one session per JVM) and keeps them stopped until release is called.
func (k *Ticks) Yield(ctx context.Context, pid int) (release func()) {
	k.mu.Lock()
	k.init()
	k.yields[pid]++
	var ms []*monitor
	for _, m := range k.mons {
		if m.target.Attach.HostPID == pid {
			ms = append(ms, m)
		}
	}
	k.mu.Unlock()
	for _, m := range ms {
		m.op.Lock()
		k.mu.Lock()
		t := m.target
		k.mu.Unlock()
		if m.active {
			m.stopSession(ctx, t)
		}
		m.setState(TickStateSuspended, TickReasonProfileRunning, nil)
		m.op.Unlock()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			if k.yields[pid]--; k.yields[pid] <= 0 {
				delete(k.yields, pid)
			}
			k.mu.Unlock()
		})
	}
}

// endLocked removes a monitor; its loop stops the session and cleans up.
func (k *Ticks) endLocked(m *monitor) {
	if m.closed {
		return
	}
	m.closed = true
	if k.mons[m.uuid] == m {
		delete(k.mons, m.uuid)
	}
	close(m.quit)
}

func (k *Ticks) snapshotLocked(m *monitor) TickSnapshot {
	s := TickSnapshot{State: m.state, Reason: m.reason, Method: m.method, Mapping: m.mapping,
		StartedAt: m.startedAt, Windows: append([]TickWindowResult{}, m.windows...)}
	if s.State == TickStateError {
		s.Err = m.err
	}
	if k.yields[m.target.Attach.HostPID] > 0 {
		s.State, s.Reason, s.Method, s.Err = TickStateSuspended, TickReasonProfileRunning, "", nil
	}
	return s
}

func (m *monitor) setState(state, reason string, err *Error) {
	m.k.mu.Lock()
	m.state, m.reason, m.err = state, reason, err
	if state == TickStateSuspended || state == TickStateError || state == TickStateUnsupported && reason == TickReasonNoServerThread {
		m.method = ""
	}
	m.k.mu.Unlock()
}

func (m *monitor) record(w TickWindowResult) {
	k := m.k
	k.mu.Lock()
	defer k.mu.Unlock()
	m.windows = append(m.windows, w)
	if n := len(m.windows); n > TickHistory {
		m.windows = append([]TickWindowResult{}, m.windows[n-TickHistory:]...)
	}
	m.state, m.reason, m.err = w.State, "", nil
	switch w.State {
	case TickStateSuspended:
		m.reason = TickReasonProfileRunning
	case TickStateUnsupported:
		m.reason = TickReasonUnknownMapping
	}
}

func (m *monitor) loop() {
	tick, stop := m.k.Every(TickWindow)
	defer stop()
	for {
		select {
		case <-m.quit:
			m.shutdown()
			return
		case <-tick:
			if !m.step() {
				m.shutdown()
				return
			}
		}
	}
}

func (m *monitor) shutdown() {
	k := m.k
	k.mu.Lock()
	k.endLocked(m)
	t := m.target
	k.mu.Unlock()
	m.op.Lock()
	defer m.op.Unlock()
	if m.active {
		ctx, cancel := context.WithTimeout(context.Background(), tickOpTimeout)
		m.stopSession(ctx, t)
		cancel()
	}
	m.removeFiles(t)
	k.Log.Info("tick monitor stopped", "uuid", m.uuid, "id", m.id)
}

// step measures one window. false ends the monitor.
func (m *monitor) step() bool {
	k := m.k
	k.mu.Lock()
	if m.closed {
		k.mu.Unlock()
		return false
	}
	if k.Now().Sub(m.lastSeen) > TickIdleTimeout {
		k.mu.Unlock()
		return false
	}
	t := m.target
	suspended := k.yields[t.Attach.HostPID] > 0
	k.mu.Unlock()

	m.op.Lock()
	defer m.op.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), tickOpTimeout)
	defer cancel()
	if suspended {
		now := k.Now()
		m.record(TickWindowResult{StartedAt: now.Add(-TickWindow), EndedAt: now, State: TickStateSuspended})
		return true
	}
	var err error
	switch {
	case !m.active:
		err = m.begin(ctx)
	case m.mode == TickMethodCounting:
		err = m.collectCounting(ctx, t)
	default:
		err = m.collectSampling(ctx, t)
	}
	if err != nil {
		return m.failed(t, err)
	}
	return true
}

// failed records an error; false if the JVM is gone.
func (m *monitor) failed(t Target, err error) bool {
	m.active = false
	if _, herr := m.k.Profiles.ReadHsperf(t.Hsperf); herr != nil {
		return false
	}
	var e *Error
	var te *TmpSpaceError
	var se *SetupError
	switch {
	case errors.As(err, &te):
		e = &Error{Code: "insufficient_tmp_space", Message: "Not enough free space in the container's /tmp.",
			Details: map[string]any{"free_bytes": te.Free, "required_bytes": te.Required}}
	case errors.As(err, &se):
		e = &Error{Code: CodeInternal, Message: "Internal error."}
	default:
		e = ErrorFor(err)
	}
	m.k.Log.Warn("tick monitor failed", "uuid", m.uuid, "err", err)
	m.setState(TickStateError, "", e)
	return true
}

// begin finds the Server thread and starts sampling (op held).
func (m *monitor) begin(ctx context.Context) error {
	k := m.k
	k.mu.Lock()
	t := m.target
	suspended := k.yields[t.Attach.HostPID] > 0
	k.mu.Unlock()
	if suspended {
		m.setState(TickStateSuspended, TickReasonProfileRunning, nil)
		return nil
	}
	tid := m.serverThread(ctx, t)
	if tid == 0 {
		m.setState(TickStateUnsupported, TickReasonNoServerThread, nil)
		return nil
	}
	m.tid = tid
	if !m.prepared {
		need := int64(tickTmpReserve)
		if _, err := t.Attach.Tmp.Mode(LibName); err != nil {
			need += int64(len(k.Profiles.Lib))
		}
		if free, ok := k.Profiles.FreeBytes(t.Attach.Tmp); ok && free < need {
			return &TmpSpaceError{Free: free, Required: need}
		}
		if err := k.Profiles.ensureLib(t.Attach); err != nil {
			return &SetupError{err}
		}
		m.prepared = true
	}
	mode := TickMethodSampling
	k.mu.Lock()
	if m.classOK && parse.TickServerMethod(m.mapping) != "" {
		mode = TickMethodCounting // known from an earlier session
	}
	k.mu.Unlock()
	if err := m.startSession(ctx, t, mode); err != nil {
		return err
	}
	k.mu.Lock()
	if len(m.windows) == 0 || m.state != TickStateOK && m.state != TickStateIdle {
		m.state, m.reason, m.err = TickStateStarting, TickReasonFirstWindow, nil
	}
	k.mu.Unlock()
	return nil
}

// serverThread returns the native id of the Server thread, 0 if there is
// none. Without native thread names (Java 8) it asks for a thread dump, at
// most once a minute.
func (m *monitor) serverThread(ctx context.Context, t Target) int {
	k := m.k
	tid, named, err := k.ThreadID(t.Attach.HostPID, ServerThread)
	if err == nil {
		return tid
	}
	if named || k.Jcmd == nil || k.Now().Before(m.nextDump) {
		return 0
	}
	m.nextDump = k.Now().Add(threadDumpWait)
	out, err := k.Jcmd(ctx, t.Attach, "Thread.print")
	if err != nil {
		k.Log.Info("tick monitor: thread dump failed", "uuid", m.uuid, "err", err)
		return 0
	}
	for _, th := range parse.ParseThreadDump(out).Threads {
		if th.Name == ServerThread && th.OSID != nil {
			return int(*th.OSID)
		}
	}
	return 0
}

func (m *monitor) file(ext string) string { return "/tmp/mpa-ticks-" + m.id + ext }

func (m *monitor) log() string { return ",log=" + m.file(".log") }

func (m *monitor) startOptions(mode string) string {
	if mode == TickMethodCounting {
		m.k.mu.Lock()
		method := parse.TickServerMethod(m.mapping)
		m.k.mu.Unlock()
		event := strings.ReplaceAll(parse.ServerClass, "/", ".") + "." + method
		return "start,event=" + event + ",interval=1,threads" + m.log()
	}
	return "start,event=wall,interval=" + strconv.Itoa(TickIntervalMs) + "ms,filter=" + strconv.Itoa(m.tid) +
		",threads,cstack=no" + m.log()
}

// startSession starts the profiler (op held).
func (m *monitor) startSession(ctx context.Context, t Target, mode string) error {
	k := m.k
	opts := m.startOptions(mode)
	err := k.Profiles.Loader.Load(ctx, t.Attach, libPath, opts)
	var ce *attach.CommandError
	if errors.As(err, &ce) && strings.Contains(m.readLog(t), "already") {
		// A profiler of an earlier bridge run is still active (profiles yield first).
		k.Log.Warn("tick monitor: stopping a leftover profiler", "uuid", m.uuid)
		k.Profiles.Loader.Load(ctx, t.Attach, libPath, "stop,collapsed,file="+m.file(".collapsed")+m.log())
		err = k.Profiles.Loader.Load(ctx, t.Attach, libPath, opts)
	}
	if err != nil {
		if errors.As(err, &ce) {
			if msg := strings.TrimSpace(m.readLog(t)); msg != "" {
				ce.Message = truncate(msg, 300)
			}
		}
		m.removeFiles(t)
		return err
	}
	m.active, m.mode, m.since, m.lastCount = true, mode, k.Now(), 0
	if mode == TickMethodCounting {
		m.busy0 = m.busyNow(ctx, t)
	}
	k.mu.Lock()
	m.method = mode
	k.mu.Unlock()
	return nil
}

// stopSession stops the profiler and drops its output (op held).
func (m *monitor) stopSession(ctx context.Context, t Target) {
	if err := m.k.Profiles.Loader.Load(ctx, t.Attach, libPath, "stop,collapsed,file="+m.file(".collapsed")+m.log()); err != nil {
		m.k.Log.Info("tick monitor: stop failed", "uuid", m.uuid, "err", err)
	}
	m.active = false
	m.removeFiles(t)
	m.k.mu.Lock()
	m.method = ""
	m.k.mu.Unlock()
}

func (m *monitor) readLog(t Target) string {
	b, _ := readFile(t.Attach.Tmp, "mpa-ticks-"+m.id+".log", 4096)
	return string(b)
}

// output reads and removes the collapsed output of the last command.
func (m *monitor) output(t Target) string {
	b, truncated := readFile(t.Attach.Tmp, "mpa-ticks-"+m.id+".collapsed", tickMaxOutput)
	m.removeFiles(t)
	if truncated {
		if i := strings.LastIndexByte(string(b), '\n'); i >= 0 {
			b = b[:i+1]
		}
	}
	return string(b)
}

func (m *monitor) removeFiles(t Target) {
	for _, ext := range []string{".collapsed", ".log"} {
		t.Attach.Tmp.Remove("mpa-ticks-" + m.id + ext)
	}
}

// collectSampling ends the window: stop (writes the samples), evaluate,
// start again (op held).
func (m *monitor) collectSampling(ctx context.Context, t Target) error {
	k := m.k
	stoppedAt := k.Now()
	err := k.Profiles.Loader.Load(ctx, t.Attach, libPath, "stop,collapsed,file="+m.file(".collapsed")+m.log())
	m.active = false
	k.mu.Lock()
	m.method = ""
	k.mu.Unlock()
	if err != nil {
		m.removeFiles(t)
		return err
	}
	w, mapping, classOK := evalSamples(m.output(t), m.tid)
	if w.Samples == 0 {
		// The Server thread was not sampled (ended or renamed): find it again next window.
		if !m.noSamples {
			m.noSamples = true
			k.Log.Warn("tick monitor: no samples of the Server thread", "uuid", m.uuid, "tid", m.tid)
		}
		m.tid = 0
		return nil
	}
	w.StartedAt, w.EndedAt = m.since, stoppedAt
	if mapping != "" {
		k.mu.Lock()
		m.mapping = mapping
		k.mu.Unlock()
		m.classOK = classOK
	}
	m.record(w)
	next := TickMethodSampling
	if m.classOK && parse.TickServerMethod(mapping) != "" {
		next = TickMethodCounting
	}
	return m.startSession(ctx, t, next)
}

// evalSamples evaluates the collapsed wall samples of the Server thread.
func evalSamples(out string, tid int) (w TickWindowResult, mapping string, classOK bool) {
	w.Method = TickMethodSampling
	var idle int64
	byMapping := map[string]int64{}
	for _, th := range parse.ParseCollapsed(out) {
		if th.TID != nil && tid != 0 && *th.TID != int64(tid) {
			continue
		}
		for _, s := range th.Stacks {
			w.Samples += s.Count
			frames := strings.Split(s.Stack, ";")
			kind, mp := parse.ClassifyTickStack(frames)
			if mp == "" {
				continue
			}
			byMapping[mp] += s.Count
			if kind == parse.TickIdle {
				idle += s.Count
			}
			for _, f := range frames {
				if strings.HasPrefix(f, parse.ServerClass+".") {
					classOK = true
					break
				}
			}
		}
	}
	for mp, n := range byMapping {
		if n > byMapping[mapping] {
			mapping = mp
		}
	}
	if w.Samples == 0 {
		return w, "", false
	}
	if mapping == "" {
		w.State = TickStateUnsupported
		return w, "", false
	}
	load := float64(w.Samples-idle) / float64(w.Samples)
	w.Load = &load
	switch {
	case load < idleLoad:
		w.State = TickStateIdle
	case load >= overloadLoad:
		// No idle time left: the TPS may be below 20, counting tells.
		w.State, w.Overloaded = TickStateOK, true
	default:
		tps, ms := float64(TickTargetTPS), load*1000/TickTargetTPS
		w.State, w.TPS, w.TickMs = TickStateOK, &tps, &ms
	}
	return w, mapping, classOK
}

// collectCounting ends the window: dump the calls counted so far and read the
// times of the Server thread (op held). The session keeps running.
func (m *monitor) collectCounting(ctx context.Context, t Target) error {
	k := m.k
	at := k.Now()
	if err := k.Profiles.Loader.Load(ctx, t.Attach, libPath, "dump,collapsed,file="+m.file(".collapsed")+m.log()); err != nil {
		m.removeFiles(t)
		return err
	}
	var count int64
	for _, th := range parse.ParseCollapsed(m.output(t)) {
		count += th.Count
	}
	b := m.busyNow(ctx, t)
	w := evalCounting(max(count-m.lastCount, 0), at.Sub(m.since), m.busy0, b)
	w.StartedAt, w.EndedAt = m.since, at
	m.lastCount, m.since, m.busy0 = count, at, b
	if w.State != "" {
		m.record(w)
	}
	return nil
}

// busySample are the times of the Server thread at one moment, in ms.
type busySample struct {
	ok        bool    // run and wait are known
	run, wait float64 // ran on a CPU, waited for one (schedstat)
	safepoint float64 // the JVM's time in safepoints (all threads stopped), -1 if unknown
}

// busyNow reads the times of the Server thread (op held). Without them the
// sample is not ok; a thread that is gone is looked up again next time.
func (m *monitor) busyNow(ctx context.Context, t Target) busySample {
	k := m.k
	b := busySample{safepoint: -1}
	if d, err := k.Profiles.ReadHsperf(t.Hsperf); err == nil {
		if v, ok := d.Long("sun.rt.safepointTime"); ok {
			b.safepoint = d.TicksToMs(v)
		}
	}
	if k.SchedStat == nil || k.HostTID == nil {
		return b
	}
	if m.hostTID == 0 {
		if m.tid == 0 {
			m.tid = m.serverThread(ctx, t)
		}
		if m.tid == 0 {
			return b
		}
		h, err := k.HostTID(t.Attach.HostPID, m.tid)
		if err != nil {
			m.tid = 0
			return b
		}
		m.hostTID = h
	}
	run, wait, err := k.SchedStat(t.Attach.HostPID, m.hostTID)
	if err != nil {
		m.tid, m.hostTID = 0, 0
		return b
	}
	b.ok, b.run, b.wait = true, float64(run)/1e6, float64(wait)/1e6
	return b
}

// evalCounting evaluates a counting window of length d with ticks calls of
// tickServer; b0 and b1 are the times of the Server thread at its start and
// end. The thread is busy while it runs or waits for a CPU. Safepoints stop
// it too, but only hurt a tick while it works: they count with the busy
// share of the rest of the window. State "" means nothing to record.
func evalCounting(ticks int64, d time.Duration, b0, b1 busySample) TickWindowResult {
	ms := float64(d.Microseconds()) / 1000
	if ms <= 0 {
		return TickWindowResult{}
	}
	tps := float64(ticks) * 1000 / ms
	w := TickWindowResult{State: TickStateOK, Method: TickMethodCounting, Ticks: ticks, TPS: &tps, Overloaded: tps < keepUpTPS}
	if b0.ok && b1.ok && b1.run >= b0.run && b1.wait >= b0.wait {
		busy := (b1.run - b0.run) + (b1.wait - b0.wait)
		if b0.safepoint >= 0 && b1.safepoint >= b0.safepoint {
			sp := min(b1.safepoint-b0.safepoint, ms)
			busy += sp * min(1, busy/max(ms-sp, 1))
		}
		load := min(busy/ms, 1)
		w.Load = &load
		if load < idleLoad {
			// Idle: nobody online, or paused (pause-when-empty-seconds, tickServer still runs).
			w.State, w.Overloaded = TickStateIdle, false
			if ticks == 0 {
				w.TPS = nil // no tick loop running (paused without ticks), not 0 TPS of a stuck one
			}
		}
		if !w.Overloaded && ticks > 0 {
			tick := busy / float64(ticks)
			w.TickMs = &tick
		}
	}
	if w.Overloaded && ticks > 0 {
		// No idle time left: a tick takes its share of the window, also while blocked.
		tick := ms / float64(ticks)
		w.TickMs = &tick
	}
	return w
}
