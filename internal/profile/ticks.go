package profile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/parse"
)

// Tick monitor (docs/API.md, "GET /ticks"): per server a small async-profiler
// session on the Server thread only, read every TickWindow.
//
//   - sampling (default): wall-clock samples of the Server thread. The share
//     of samples outside the idle methods is the tick load; with 20 TPS the
//     average tick takes load × 50 ms.
//   - counting: when the tick loop has no idle time left, the samples can't
//     tell the TPS any more. Then async-profiler instruments tickServer and
//     counts its calls (exact TPS; without idle time a tick takes 1000/TPS ms).
//     Back to sampling once the server keeps up again. Switching instruments
//     MinecraftServer (class retransformation), so it happens rarely.
//
// async-profiler 4.1 has no method durations (no --trace) and supports
// several events only with JFR output, hence the two modes.
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
	TickStatePaused      = "paused"
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
	overloadLoad   = 0.95  // sampling: no idle time left, count instead
	keepUpTPS      = 19.5  // counting: keeps up again, sample instead
	countCooldown  = 6     // sampling windows before counting again
	pausedLoad     = 0.005 // below: the server does no tick work (pause-when-empty-seconds)
	threadDumpWait = time.Minute
)

// ErrTooManyMonitors: max_tick_monitors reached on this node.
var ErrTooManyMonitors = errors.New("profile: too many tick monitors")

// TickWindowResult is one measured window. Pointers are nil when unknown.
type TickWindowResult struct {
	StartedAt  time.Time
	EndedAt    time.Time
	State      string // ok, paused, suspended, unsupported
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
	Log  *slog.Logger

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
	tid       int
	since     time.Time // start of the session, or the last dump
	lastCount int64
	cooldown  int
	classOK   bool // the tick frames are of ServerClass, which counting instruments
	prepared  bool // free space checked, library in place
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
		if _, err := os.Lstat(filepath.Join(t.Attach.TmpDir, LibName)); err != nil {
			need += int64(len(k.Profiles.Lib))
		}
		if free, ok := k.Profiles.FreeBytes(t.Attach.TmpDir); ok && free < need {
			return &TmpSpaceError{Free: free, Required: need}
		}
		if err := k.Profiles.ensureLib(t.Attach); err != nil {
			return &SetupError{err}
		}
		m.prepared = true
	}
	if err := m.startSession(ctx, t, TickMethodSampling); err != nil {
		return err
	}
	k.mu.Lock()
	if len(m.windows) == 0 || m.state != TickStateOK && m.state != TickStatePaused {
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
	b, _ := m.k.Profiles.readFile(filepath.Join(t.Attach.TmpDir, "mpa-ticks-"+m.id+".log"), 4096)
	return string(b)
}

// output reads and removes the collapsed output of the last command.
func (m *monitor) output(t Target) string {
	b, truncated := m.k.Profiles.readFile(filepath.Join(t.Attach.TmpDir, "mpa-ticks-"+m.id+".collapsed"), tickMaxOutput)
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
		removeRegular(filepath.Join(t.Attach.TmpDir, "mpa-ticks-"+m.id+ext))
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
	if m.cooldown > 0 {
		m.cooldown--
	} else if w.Overloaded && m.classOK {
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
	case load < pausedLoad:
		w.State = TickStatePaused
	case load >= overloadLoad:
		// No idle time left: the TPS may be below 20, counting tells.
		w.State, w.Overloaded = TickStateOK, true
	default:
		tps, ms := float64(TickTargetTPS), load*1000/TickTargetTPS
		w.State, w.TPS, w.TickMs = TickStateOK, &tps, &ms
	}
	return w, mapping, classOK
}

// collectCounting ends the window: dump the calls counted so far (op held).
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
	ticks := max(count-m.lastCount, 0)
	w := TickWindowResult{StartedAt: m.since, EndedAt: at, State: TickStateOK, Method: TickMethodCounting, Ticks: ticks}
	m.lastCount, m.since = count, at
	sec := w.EndedAt.Sub(w.StartedAt).Seconds()
	if sec <= 0 {
		return nil
	}
	tps := float64(ticks) / sec
	w.TPS = &tps
	if tps < keepUpTPS {
		w.Overloaded = true
		if ticks > 0 {
			ms := sec * 1000 / float64(ticks)
			w.TickMs = &ms
		}
	}
	m.record(w)
	if tps >= keepUpTPS {
		m.stopSession(ctx, t)
		m.cooldown = countCooldown
		return m.startSession(ctx, t, TickMethodSampling)
	}
	return nil
}
