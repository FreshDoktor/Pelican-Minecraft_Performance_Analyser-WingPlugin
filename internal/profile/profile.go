// Package profile runs async-profiler in a server's JVM: the library is
// copied into the container's /tmp, loaded with the attach operation "load"
// and started/stopped with its agent options. The collapsed output is read
// from the host. All files of the bridge in /tmp are named mpa-*.
package profile

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/parse"
)

// Files in the container's /tmp.
const (
	LibName = "mpa-libasyncProfiler.so"
	libPath = "/tmp/" + LibName // as the JVM sees it
)

// Limits and defaults.
const (
	Retention          = 15 * time.Minute
	AllocIntervalBytes = 512 << 10
	MaxOutput          = 32 << 20
	// tmpReserve is the free space required in /tmp besides the library.
	tmpReserve   = 8 << 20
	pollInterval = 250 * time.Millisecond
	stopTimeout  = 30 * time.Second
)

// Modes and states.
const (
	ModeWall  = "wall"
	ModeCPU   = "cpu"
	ModeAlloc = "alloc"

	StateRunning    = "running"
	StateProcessing = "processing"
	StateDone       = "done"
	StateFailed     = "failed"
)

// Result warnings.
const (
	WarnNoSamples           = "no_samples"
	WarnServerThreadMissing = "server_thread_missing"
	WarnTruncated           = "truncated"
)

// Error codes of failed runs; the same strings as the API error codes.
const (
	CodeServerNotRunning = "server_not_running"
	CodeNoJavaProcess    = "no_java_process"
	CodeAttachFailed     = "attach_failed"
	CodeAttachTimeout    = "attach_timeout"
	CodeOutputTooLarge   = "output_too_large"
	CodeInternal         = "internal_error"
)

// Errors of Start.
var ErrTooMany = errors.New("profile: too many profiles running")

// RunningError: the server already has a running profile.
type RunningError struct{ ID string }

func (e *RunningError) Error() string { return "profile: already running: " + e.ID }

// TmpSpaceError: not enough free space in the container's /tmp.
type TmpSpaceError struct{ Free, Required int64 }

func (e *TmpSpaceError) Error() string {
	return fmt.Sprintf("profile: %d bytes free in /tmp, %d required", e.Free, e.Required)
}

// SetupError is a local failure (files, hsperfdata), not one of the JVM.
type SetupError struct{ Err error }

func (e *SetupError) Error() string { return "profile: " + e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

// Error describes why a run failed.
type Error struct {
	Code    string
	Message string
	Details map[string]any
}

// ErrorFor maps an attach error to an Error.
func ErrorFor(err error) *Error {
	var ce *attach.CommandError
	switch {
	case errors.Is(err, attach.ErrTimeout):
		return &Error{Code: CodeAttachTimeout, Message: "The JVM did not answer in time."}
	case errors.Is(err, attach.ErrTooLarge):
		return &Error{Code: CodeOutputTooLarge, Message: "The JVM output exceeds the size limit."}
	case errors.As(err, &ce):
		return &Error{Code: CodeAttachFailed, Message: "The JVM rejected or failed the command.",
			Details: map[string]any{"code": ce.Code, "output": ce.Message}}
	}
	return &Error{Code: CodeAttachFailed, Message: "The JVM rejected or failed the command."}
}

// Loader runs "load <lib> true <options>" in a JVM (attach.Client).
type Loader interface {
	Load(ctx context.Context, t attach.Target, lib, options string) error
}

// Target is the JVM of a server, resolved by the API.
type Target struct {
	ServerUUID   string
	Attach       attach.Target
	Hsperf       string // host path of the hsperfdata file
	JVMStartedAt int64  // Unix ms (sun.rt.createVmBeginTime)
}

// Request is a validated POST /profiles body.
type Request struct {
	Mode       string
	Seconds    int
	IntervalMs int // wall, cpu; 0 for alloc
}

// Run is a snapshot of a profile.
type Run struct {
	ID                 string
	ServerUUID         string
	Mode               string
	Seconds            int
	IntervalMs         int   // 0 for alloc
	AllocIntervalBytes int64 // 0 unless alloc
	State              string
	StartedAt          time.Time
	EndsAt             time.Time
	FinishedAt         time.Time // zero while running
	ExpiresAt          time.Time // zero while running
	Err                *Error
	Result             *Result
}

// Result of a finished profile.
type Result struct {
	Duration       time.Duration
	Unit           string // samples, bytes
	Threads        []parse.ProfileThread
	Total          int64
	Warnings       []string
	Collectors     []CollectorDelta
	Pauses         []Pause
	PausesComplete bool
	SafepointCount int64
	SafepointMs    float64
	CPUMs          float64
}

// CollectorDelta is the activity of one collector during the profile.
type CollectorDelta struct {
	Index  int
	Name   string
	Count  int64
	TimeMs float64
}

// Pause is one observed collection.
type Pause struct {
	CollectorIndex int
	Name           string
	StartOffsetMs  float64
	DurationMs     float64
}

// Manager runs and keeps the profiles of all servers.
type Manager struct {
	MaxConcurrent int
	Lib           []byte
	Loader        Loader
	ProcRoot      string // "/proc"
	Log           *slog.Logger

	// Check is called when the JVM disappears during a run; it tells why
	// (server_not_running with container_state, or no_java_process).
	Check func(ctx context.Context, uuid string) *Error

	// Yield is called before a profile starts in a JVM (Ticks.Yield): the
	// tick monitor gives up the JVM's profiler session until release.
	Yield func(ctx context.Context, hostPID int) (release func())

	// Overridable in tests.
	Now        func() time.Time
	ReadHsperf func(path string) (*hsperf.Data, error)
	CPUTicks   func(pid int) (int64, error) // utime+stime in USER_HZ (100)
	FreeBytes  func(dir string) (int64, bool)
	After      func(d time.Duration) <-chan time.Time
	Poll       func() (<-chan time.Time, func())
	NewID      func() string

	mu     sync.Mutex
	runs   map[string]*run
	libSum string
}

type run struct {
	Run
	hidden  bool // starting, not visible yet
	target  Target
	cancel  context.CancelFunc
	done    chan struct{}
	release func() // of Yield, called when the run ends
}

func (r *run) active() bool { return r.State == StateRunning || r.State == StateProcessing }

func (m *Manager) init() {
	if m.runs == nil {
		m.runs = map[string]*run{}
	}
	if m.Now == nil {
		m.Now = time.Now
	}
	if m.ReadHsperf == nil {
		m.ReadHsperf = hsperf.ReadFile
	}
	if m.FreeBytes == nil {
		m.FreeBytes = freeBytes
	}
	if m.After == nil {
		m.After = time.After
	}
	if m.Poll == nil {
		m.Poll = func() (<-chan time.Time, func()) {
			t := time.NewTicker(pollInterval)
			return t.C, t.Stop
		}
	}
	if m.Log == nil {
		m.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if m.ProcRoot == "" {
		m.ProcRoot = "/proc"
	}
	if m.libSum == "" {
		s := sha256.Sum256(m.Lib)
		m.libSum = hex.EncodeToString(s[:])
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Start starts a profile. The attach listener must already run.
func (m *Manager) Start(ctx context.Context, t Target, req Request) (Run, error) {
	m.mu.Lock()
	m.init()
	m.expireLocked()
	active := 0
	for _, r := range m.runs {
		if r.active() {
			active++
			if r.ServerUUID == t.ServerUUID {
				m.mu.Unlock()
				return Run{}, &RunningError{ID: r.ID}
			}
		}
	}
	if active >= m.MaxConcurrent {
		m.mu.Unlock()
		return Run{}, ErrTooMany
	}
	id := newID()
	if m.NewID != nil {
		id = m.NewID()
	}
	r := &run{Run: Run{ID: id, ServerUUID: t.ServerUUID, Mode: req.Mode, Seconds: req.Seconds, State: StateRunning},
		hidden: true, target: t, done: make(chan struct{})}
	if req.Mode == ModeAlloc {
		r.AllocIntervalBytes = AllocIntervalBytes
	} else {
		r.IntervalMs = req.IntervalMs
	}
	m.runs[r.ID] = r
	m.mu.Unlock()

	r.release = func() {}
	if m.Yield != nil {
		r.release = m.Yield(ctx, t.Attach.HostPID)
	}
	if err := m.start(ctx, r); err != nil {
		m.mu.Lock()
		delete(m.runs, r.ID)
		m.mu.Unlock()
		m.removeFiles(t.Attach, r.ID)
		r.release()
		return Run{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return r.Run, nil
}

func (m *Manager) start(ctx context.Context, r *run) error {
	t := r.target
	libHost := filepath.Join(t.Attach.TmpDir, LibName)
	need := int64(tmpReserve)
	if _, err := os.Lstat(libHost); err != nil {
		need += int64(len(m.Lib))
	}
	if free, ok := m.FreeBytes(t.Attach.TmpDir); ok && free < need {
		return &TmpSpaceError{Free: free, Required: need}
	}
	if err := m.ensureLib(t.Attach); err != nil {
		return &SetupError{err}
	}
	before, err := m.ReadHsperf(t.Hsperf)
	if err != nil {
		return &SetupError{fmt.Errorf("hsperfdata: %w", err)}
	}
	cpu0, cpuErr := m.cpuTicks(t.Attach.HostPID)

	opts := startOptions(r.ID, r.Mode, r.IntervalMs)
	err = m.Loader.Load(ctx, t.Attach, libPath, opts)
	var ce *attach.CommandError
	if errors.As(err, &ce) && strings.Contains(m.readLog(t.Attach, r.ID), "already") {
		// A profiler of an earlier run (e.g. before a bridge restart) is still active.
		m.Log.Warn("stopping a leftover profiler", "uuid", t.ServerUUID)
		m.Loader.Load(ctx, t.Attach, libPath, stopOptions(r.ID, r.Mode))
		err = m.Loader.Load(ctx, t.Attach, libPath, opts)
	}
	if err != nil {
		if errors.As(err, &ce) {
			if msg := strings.TrimSpace(m.readLog(t.Attach, r.ID)); msg != "" {
				ce.Message = truncate(msg, 300)
			}
		}
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	r.StartedAt = m.Now()
	r.EndsAt = r.StartedAt.Add(time.Duration(r.Seconds) * time.Second)
	r.hidden = false
	r.cancel = cancel
	m.mu.Unlock()
	m.Log.Info("profile started", "id", r.ID, "uuid", t.ServerUUID, "mode", r.Mode, "seconds", r.Seconds)
	go m.loop(runCtx, r, before, cpu0, cpuErr == nil)
	return nil
}

func (m *Manager) cpuTicks(pid int) (int64, error) {
	if m.CPUTicks == nil {
		return 0, errors.New("no CPU source")
	}
	return m.CPUTicks(pid)
}

func startOptions(id, mode string, intervalMs int) string {
	log := ",log=/tmp/mpa-" + id + ".log"
	switch mode {
	case ModeCPU:
		return "start,event=itimer,interval=" + strconv.Itoa(intervalMs) + "ms,threads" + log
	case ModeAlloc:
		return "start,alloc=" + strconv.Itoa(AllocIntervalBytes) + ",threads" + log
	}
	return "start,event=wall,interval=" + strconv.Itoa(intervalMs) + "ms,threads" + log
}

// stopOptions always names an output file: without one async-profiler
// prints the profile to the JVM's stdout, i.e. the server console.
func stopOptions(id, mode string) string {
	s := "stop,collapsed"
	if mode == ModeAlloc {
		s += ",total"
	}
	return s + ",log=/tmp/mpa-" + id + ".log,file=/tmp/mpa-" + id + ".collapsed"
}

// ensureLib puts the library into the container's /tmp. A library the JVM
// has mapped is never touched (overwriting it crashes the JVM); otherwise a
// file that is not exactly ours is replaced.
func (m *Manager) ensureLib(t attach.Target) error {
	host := filepath.Join(t.TmpDir, LibName)
	if m.libLoaded(t.HostPID) {
		return nil
	}
	if st, err := os.Lstat(host); err == nil {
		if st.Mode().IsRegular() && st.Mode().Perm() == 0o644 && ownedByRoot(st) && m.sameLib(host) {
			return nil
		}
		if err := os.Remove(host); err != nil {
			return fmt.Errorf("profile: remove stale library: %w", err)
		}
	}
	// O_EXCL: never follows a symlink planted under that name.
	f, err := os.OpenFile(host, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("profile: write library: %w", err)
	}
	_, err = f.Write(m.Lib)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(host, 0o644) // umask
	}
	if err != nil {
		os.Remove(host)
		return fmt.Errorf("profile: write library: %w", err)
	}
	return nil
}

func (m *Manager) sameLib(path string) bool {
	f, err := openNoFollow(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, int64(len(m.Lib))+1)); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == m.libSum
}

// libLoaded reports whether the JVM has the library mapped.
func (m *Manager) libLoaded(pid int) bool {
	f, err := os.Open(filepath.Join(m.ProcRoot, strconv.Itoa(pid), "maps"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if strings.HasSuffix(sc.Text(), libPath) {
			return true
		}
	}
	return false
}

func (m *Manager) readLog(t attach.Target, id string) string {
	b, _ := m.readFile(filepath.Join(t.TmpDir, "mpa-"+id+".log"), 4096)
	return string(b)
}

// readFile reads at most limit bytes of a regular file without following symlinks.
func (m *Manager) readFile(path string, limit int64) ([]byte, bool) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false
	}
	if int64(len(b)) > limit {
		return b[:limit], true
	}
	return b, false
}

func (m *Manager) removeFiles(t attach.Target, id string) {
	for _, ext := range []string{".collapsed", ".log"} {
		removeRegular(filepath.Join(t.TmpDir, "mpa-"+id+ext))
	}
}

func removeRegular(path string) {
	if st, err := os.Lstat(path); err == nil && !st.IsDir() {
		os.Remove(path)
	}
}

func (m *Manager) loop(ctx context.Context, r *run, before *hsperf.Data, cpu0 int64, cpuOK bool) {
	defer close(r.done)
	defer r.release()
	t := r.target
	end := m.After(r.EndsAt.Sub(m.Now()))
	if m.Now().After(r.EndsAt) {
		end = closed()
	}
	tick, stopTick := m.Poll()
	defer stopTick()
	gc := newGCTracker(before, r.StartedAt.UnixMilli()-t.JVMStartedAt)
	last := before
	startedAt := r.StartedAt

	for done := false; !done; {
		select {
		case <-ctx.Done():
			m.stop(t, r)
			return
		case <-tick:
			d, err := m.ReadHsperf(t.Hsperf)
			if err != nil {
				m.failGone(r)
				return
			}
			gc.observe(d)
			last = d
		case <-end:
			done = true
		}
	}

	m.setState(r, StateProcessing)
	sctx, cancel := context.WithTimeout(ctx, stopTimeout)
	err := m.Loader.Load(sctx, t.Attach, libPath, stopOptions(r.ID, r.Mode))
	cancel()
	stoppedAt := m.Now()
	if err != nil {
		if ctx.Err() != nil {
			m.removeFiles(t.Attach, r.ID)
			return
		}
		if _, herr := m.ReadHsperf(t.Hsperf); herr != nil {
			m.failGone(r)
			return
		}
		m.fail(r, ErrorFor(err))
		m.removeFiles(t.Attach, r.ID)
		return
	}
	out, truncated := m.readFile(filepath.Join(t.Attach.TmpDir, "mpa-"+r.ID+".collapsed"), MaxOutput)
	m.removeFiles(t.Attach, r.ID)
	if truncated {
		if i := strings.LastIndexByte(string(out), '\n'); i >= 0 {
			out = out[:i+1]
		}
	}
	if after, err := m.ReadHsperf(t.Hsperf); err == nil {
		gc.observe(after)
		last = after
	}

	res := &Result{Duration: stoppedAt.Sub(startedAt), Unit: "samples", Warnings: []string{}}
	if r.Mode == ModeAlloc {
		res.Unit = "bytes"
	}
	res.Threads = parse.ParseCollapsed(string(out))
	hasServer := false
	for _, th := range res.Threads {
		res.Total += th.Count
		if th.Name == "Server thread" {
			hasServer = true
		}
	}
	switch {
	case res.Total == 0:
		res.Warnings = append(res.Warnings, WarnNoSamples)
	case !hasServer:
		res.Warnings = append(res.Warnings, WarnServerThreadMissing)
	}
	if truncated {
		res.Warnings = append(res.Warnings, WarnTruncated)
	}
	res.Collectors, res.Pauses, res.PausesComplete = gc.result(before, last)
	sp0, _ := before.Long("sun.rt.safepoints")
	sp1, _ := last.Long("sun.rt.safepoints")
	st0, _ := before.Long("sun.rt.safepointTime")
	st1, _ := last.Long("sun.rt.safepointTime")
	res.SafepointCount, res.SafepointMs = sp1-sp0, last.TicksToMs(st1-st0)
	if cpuOK {
		if cpu1, err := m.cpuTicks(t.Attach.HostPID); err == nil {
			res.CPUMs = float64((cpu1 - cpu0) * 10) // USER_HZ = 100
		}
	}

	m.mu.Lock()
	r.Result = res
	r.State = StateDone
	r.FinishedAt = m.Now()
	r.ExpiresAt = r.FinishedAt.Add(Retention)
	m.mu.Unlock()
	m.Log.Info("profile done", "id", r.ID, "samples", res.Total, "threads", len(res.Threads))
}

func closed() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

// stop ends a cancelled run and removes its files.
func (m *Manager) stop(t Target, r *run) {
	sctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := m.Loader.Load(sctx, t.Attach, libPath, stopOptions(r.ID, r.Mode)); err != nil {
		m.Log.Info("stop after cancel failed", "id", r.ID, "err", err)
	}
	m.removeFiles(t.Attach, r.ID)
}

func (m *Manager) failGone(r *run) {
	e := &Error{Code: CodeNoJavaProcess, Message: "The Java process ended during the profile."}
	if m.Check != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if ce := m.Check(ctx, r.ServerUUID); ce != nil {
			e = ce
		}
		cancel()
	}
	m.fail(r, e)
	m.removeFiles(r.target.Attach, r.ID)
}

func (m *Manager) fail(r *run, e *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.State = StateFailed
	r.Err = e
	r.FinishedAt = m.Now()
	r.ExpiresAt = r.FinishedAt.Add(Retention)
	m.Log.Warn("profile failed", "id", r.ID, "code", e.Code, "message", e.Message)
}

func (m *Manager) setState(r *run, s string) {
	m.mu.Lock()
	r.State = s
	m.mu.Unlock()
}

func (m *Manager) expireLocked() {
	now := m.Now()
	for id, r := range m.runs {
		if !r.active() && !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) {
			delete(m.runs, id)
		}
	}
}

// Get returns a profile of the server.
func (m *Manager) Get(uuid, id string) (Run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.expireLocked()
	r, ok := m.runs[id]
	if !ok || r.hidden || r.ServerUUID != uuid {
		return Run{}, false
	}
	return r.Run, true
}

// List returns the profiles of a server, newest first.
func (m *Manager) List(uuid string) []Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.expireLocked()
	out := []Run{}
	for _, r := range m.runs {
		if !r.hidden && r.ServerUUID == uuid {
			out = append(out, r.Run)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartedAt.After(out[j-1].StartedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Delete cancels a running profile (stopping the profiler and removing its
// files) or discards a finished one.
func (m *Manager) Delete(uuid, id string) bool {
	m.mu.Lock()
	m.init()
	r, ok := m.runs[id]
	if !ok || r.hidden || r.ServerUUID != uuid {
		m.mu.Unlock()
		return false
	}
	active := r.active()
	if active && r.cancel != nil {
		r.cancel()
	}
	m.mu.Unlock()
	if active {
		select {
		case <-r.done:
		case <-time.After(stopTimeout + 5*time.Second):
		}
	}
	m.mu.Lock()
	delete(m.runs, id)
	m.mu.Unlock()
	return true
}

// Cleanup removes leftover mpa-* files (except the library) from a JVM's
// /tmp, e.g. after a bridge restart, and stops a profiler that is still
// active there. Only regular files are removed; nothing is followed.
func (m *Manager) Cleanup(ctx context.Context, t attach.Target, listener bool) {
	m.mu.Lock()
	m.init()
	busy := false
	for _, r := range m.runs {
		if r.active() && r.target.Attach.HostPID == t.HostPID {
			busy = true
		}
	}
	m.mu.Unlock()
	if busy {
		return
	}
	if listener && m.libLoaded(t.HostPID) {
		// Stopping an inactive profiler only logs "Profiler is not active".
		m.Loader.Load(ctx, t, libPath, stopOptions("cleanup", ModeWall))
	}
	entries, err := os.ReadDir(t.TmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "mpa-") && e.Name() != LibName && e.Type().IsRegular() {
			removeRegular(filepath.Join(t.TmpDir, e.Name()))
		}
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
