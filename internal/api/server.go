// Package api implements the HTTP API of docs/API.md.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/parse"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

// Implemented lists the features of this build.
var Implemented = []string{FeatureStatus, FeatureVM, FeatureMetrics, FeatureThreads, FeatureHistogram, FeatureVMFlags, FeatureLiveHeap, FeatureProfiles, FeatureTicks}

// Attacher runs diagnostic commands in a JVM (attach.Client in production).
type Attacher interface {
	Start(ctx context.Context, t attach.Target) error
	Jcmd(ctx context.Context, t attach.Target, command string) (string, error)
	Load(ctx context.Context, t attach.Target, lib, options string) error
}

// MaxAttachOutput limits the output of one attach command (32 MiB).
const MaxAttachOutput = 32 << 20

// Timeouts of the attach commands, including starting the listener.
var (
	TimeoutThreads   = 15 * time.Second
	TimeoutHistogram = 90 * time.Second
	TimeoutQuick     = 5 * time.Second // VM.flags, GC.heap_info
)

// Credentials yields the token id of Wings (for the User-Agent) and the
// current bridge token.
type Credentials interface {
	Get() (id, token string)
}

// ProcessFinder finds the JVM of a container (container.Proc in production).
type ProcessFinder interface {
	FindJava(initPID int) (container.JavaProcess, error)
}

// Server serves API v1.
type Server struct {
	Version    string
	Creds      Credentials
	Allowed    []netip.Prefix
	Docker     container.Inspector
	Proc       ProcessFinder
	Attach     Attacher
	Profiles   *profile.Manager // nil: /profiles answers not_implemented
	Ticks      *profile.Ticks   // nil: /ticks answers not_implemented
	Features   []string         // defaults to Implemented
	TmpfsBytes int64
	TLS        bool
	Limits     NodeLimits
	Log        *slog.Logger

	// Overridable in tests.
	Now          func() time.Time
	HostCPUs     int
	ReadHsperf   func(path string) (*hsperf.Data, error)
	SocketExists func(path string) bool

	started time.Time
}

// Init fills defaults; call before serving.
func (s *Server) Init() {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.HostCPUs == 0 {
		s.HostCPUs = runtime.NumCPU()
	}
	if s.ReadHsperf == nil {
		s.ReadHsperf = hsperf.ReadFile
	}
	if s.SocketExists == nil {
		s.SocketExists = socketExists
	}
	if s.Features == nil {
		s.Features = Implemented
	}
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	s.started = s.Now()
}

func socketExists(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

var (
	reUUID      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reProfileID = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ServeHTTP checks IP and token, then routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, token := s.Creds.Get()
	// Sent on every response so the panel can verify the node (also on errors).
	w.Header().Set("User-Agent", "MPA Bridge/v"+s.Version+" (id:"+id+")")

	if !s.ipAllowed(r.RemoteAddr) {
		s.fail(w, r, newError(CodeIPNotAllowed, "", nil))
		return
	}
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" || subtle.ConstantTimeCompare([]byte(auth), []byte(token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		s.fail(w, r, newError(CodeUnauthorized, "", nil))
		return
	}
	s.route(w, r)
}

func (s *Server) ipAllowed(remote string) bool {
	if len(s.Allowed) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range s.Allowed {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "v1" {
		s.fail(w, r, newError(CodeNotFound, "", nil))
		return
	}
	parts = parts[2:]
	if len(parts) == 1 && parts[0] == "status" {
		if s.method(w, r, http.MethodGet) {
			s.nodeStatus(w, r)
		}
		return
	}
	if parts[0] != "servers" || len(parts) < 3 {
		s.fail(w, r, newError(CodeNotFound, "", nil))
		return
	}
	uuid := strings.ToLower(parts[1])
	rest := parts[2:]

	var methods []string
	var h func(w http.ResponseWriter, r *http.Request, uuid string)
	switch {
	case len(rest) == 1 && rest[0] == "status":
		methods, h = []string{http.MethodGet}, s.serverStatus
	case len(rest) == 1 && rest[0] == "vm":
		methods, h = []string{http.MethodGet}, s.vm
	case len(rest) == 1 && rest[0] == "metrics":
		methods, h = []string{http.MethodGet}, s.metrics
	case len(rest) == 1 && rest[0] == "threads":
		methods, h = []string{http.MethodGet}, s.threads
	case len(rest) == 1 && rest[0] == "histogram":
		methods, h = []string{http.MethodGet}, s.histogram
	case len(rest) == 1 && rest[0] == "profiles":
		methods, h = []string{http.MethodGet, http.MethodPost}, s.profiles
	case len(rest) == 1 && rest[0] == "ticks":
		methods, h = []string{http.MethodGet}, s.ticks
	case len(rest) == 2 && rest[0] == "profiles":
		methods = []string{http.MethodGet, http.MethodDelete}
		h = func(w http.ResponseWriter, r *http.Request, uuid string) {
			if !reProfileID.MatchString(rest[1]) {
				s.fail(w, r, newError(CodeInvalidRequest, "Invalid profile id.", map[string]any{"field": "id", "reason": "must be 16 lowercase hex characters"}))
				return
			}
			s.profileByID(w, r, uuid, rest[1])
		}
	default:
		s.fail(w, r, newError(CodeNotFound, "", nil))
		return
	}
	if !s.method(w, r, methods...) {
		return
	}
	if !reUUID.MatchString(uuid) {
		s.fail(w, r, newError(CodeInvalidRequest, "Invalid server UUID.", map[string]any{"field": "uuid", "reason": "must be a UUID"}))
		return
	}
	h(w, r, uuid)
}

func (s *Server) method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m || (m == http.MethodGet && r.Method == http.MethodHead) {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	s.fail(w, r, newError(CodeMethodNotAllowed, "", nil))
	return false
}

func (s *Server) nowMs() int64 { return s.Now().UnixMilli() }

func (s *Server) nodeStatus(w http.ResponseWriter, r *http.Request) {
	now := s.Now()
	s.write(w, http.StatusOK, NodeStatusResponse{
		BridgeVersion: s.Version, APIVersion: APIVersion, Features: s.Features,
		StartedAt: s.started.UnixMilli(), UptimeMs: now.Sub(s.started).Milliseconds(), SampledAt: now.UnixMilli(),
		TLS: s.TLS, Limits: s.Limits,
	})
}

// target is a resolved server: container, JVM and its hsperfdata.
type target struct {
	info    container.Info
	java    *container.JavaProcess
	perf    *hsperf.Data
	perfErr error
}

// state returns StateReady or the error code the data endpoints return.
func (t *target) state() string {
	switch {
	case !t.info.Running:
		return CodeServerNotRunning
	case t.java == nil:
		return CodeNoJavaProcess
	case t.perf == nil:
		return CodeHsperfUnavailable
	}
	return StateReady
}

func (s *Server) resolve(ctx context.Context, uuid string) (*target, *apiError) {
	info, err := s.Docker.Inspect(ctx, uuid)
	switch {
	case errors.Is(err, container.ErrNotFound):
		return nil, newError(CodeServerNotFound, "", nil)
	case err != nil:
		s.Log.Warn("docker inspect failed", "uuid", uuid, "err", err)
		return nil, newError(CodeDockerUnavailable, "", nil)
	}
	t := &target{info: info}
	if !info.Running || info.PID <= 0 {
		return t, nil
	}
	j, err := s.Proc.FindJava(info.PID)
	if err != nil {
		if !errors.Is(err, container.ErrNoJava) {
			s.Log.Warn("process lookup failed", "uuid", uuid, "err", err)
		}
		return t, nil
	}
	t.java = &j
	if j.Hsperf != "" {
		t.perf, t.perfErr = s.ReadHsperf(j.Hsperf)
		if t.perfErr != nil {
			s.Log.Info("hsperfdata unreadable", "uuid", uuid, "path", j.Hsperf, "err", t.perfErr)
			t.perf = nil
		}
	}
	return t, nil
}

// ready resolves and returns an error unless the server is ready.
func (s *Server) ready(ctx context.Context, uuid string) (*target, *apiError) {
	t, e := s.resolve(ctx, uuid)
	if e != nil {
		return nil, e
	}
	switch st := t.state(); st {
	case StateReady:
		return t, nil
	case CodeServerNotRunning:
		return nil, newError(st, "", map[string]any{"container_state": t.info.State})
	default:
		return nil, newError(st, "", nil)
	}
}

func (s *Server) serverStatus(w http.ResponseWriter, r *http.Request, uuid string) {
	t, e := s.resolve(r.Context(), uuid)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	now := s.nowMs()
	resp := StatusResponse{
		State:     t.state(),
		Bridge:    BridgeInfo{Version: s.Version, APIVersion: APIVersion, Features: s.Features},
		Server:    serverInfo(t.info),
		SampledAt: now,
	}
	if t.java != nil {
		resp.Java = buildJavaProcess(*t.java, t.perf, now)
		a := buildAttach(t.perf, s.SocketExists(t.java.AttachSocket()), resp.Java.UptimeMs)
		resp.Attach = &a
	}
	s.write(w, http.StatusOK, resp)
}

func (s *Server) vm(w http.ResponseWriter, r *http.Request, uuid string) {
	t, e := s.ready(r.Context(), uuid)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	resp := BuildVM(t.perf, t.info, s.SocketExists(t.java.AttachSocket()), s.HostCPUs, s.TmpfsBytes, s.nowMs())
	if out, _, e := s.jcmd(r.Context(), t, "VM.flags", TimeoutQuick); e != nil {
		resp.FlagsError = &e.body.Error
	} else {
		resp.Flags, resp.FlagsError = parse.ParseVMFlags(out), nil
	}
	// The listener may have been started just now.
	resp.Attach = buildAttach(t.perf, s.SocketExists(t.java.AttachSocket()), resp.Launch.UptimeMs)
	s.write(w, http.StatusOK, resp)
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request, uuid string) {
	live := false
	if v := r.URL.Query().Get("live"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			s.fail(w, r, newError(CodeInvalidRequest, "Invalid parameter live.", map[string]any{"field": "live", "reason": "must be 0 or 1"}))
			return
		}
		live = b
	}
	t, e := s.ready(r.Context(), uuid)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	m := BuildMetrics(t.perf, s.nowMs())
	if live {
		start := s.nowMs()
		out, _, e := s.jcmd(r.Context(), t, "GC.heap_info", TimeoutQuick)
		if e != nil {
			m.LiveHeapError = &e.body.Error
		} else if lh, ok := BuildLiveHeap(out, start); ok {
			// JDK 25 no longer prints metaspace in GC.heap_info.
			if lh.Metaspace == nil {
				if ms, _, e := s.jcmd(r.Context(), t, "VM.metaspace", TimeoutQuick); e == nil {
					lh.Metaspace = liveMetaspace(ms)
				}
			}
			m.LiveHeap = lh
		} else {
			s.Log.Info("unparsable GC.heap_info", "uuid", uuid, "output", truncate(out, 300))
			m.LiveHeapError = ptr(CodeParseFailed)
		}
	}
	s.write(w, http.StatusOK, m)
}

func (s *Server) threads(w http.ResponseWriter, r *http.Request, uuid string) {
	t, e := s.ready(r.Context(), uuid)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	start := s.nowMs()
	out, dur, e := s.jcmd(r.Context(), t, "Thread.print -l", TimeoutThreads)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.write(w, http.StatusOK, BuildThreads(parse.ParseThreadDump(out), out, start, dur))
}

func (s *Server) histogram(w http.ResponseWriter, r *http.Request, uuid string) {
	limit := DefaultHistogramLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxHistogramLimit {
			s.fail(w, r, newError(CodeInvalidRequest, "Invalid parameter limit.", map[string]any{"field": "limit", "reason": "must be between 1 and 100000"}))
			return
		}
		limit = n
	}
	t, e := s.ready(r.Context(), uuid)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	start := s.nowMs()
	out, dur, e := s.jcmd(r.Context(), t, "GC.class_histogram -all", TimeoutHistogram)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	h := parse.ParseHistogram(out)
	if len(h.Classes) == 0 {
		s.Log.Info("unparsable class histogram", "uuid", uuid, "output", truncate(out, 300))
		s.fail(w, r, newError(CodeParseFailed, "", nil))
		return
	}
	s.write(w, http.StatusOK, BuildHistogram(h, limit, start, dur))
}

// jcmd runs a diagnostic command under the safety rules of the attach object:
// the listener is only started (trigger file, then SIGQUIT) if attach.available,
// i.e. never with -Xrs, -XX:+DisableAttachMechanism, without hsperfdata or for
// a JVM younger than 30 s.
func (s *Server) jcmd(ctx context.Context, t *target, command string, timeout time.Duration) (string, float64, *apiError) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target, e := s.ensureAttach(ctx, t, command)
	if e != nil {
		return "", 0, e
	}
	start := s.Now()
	out, err := s.Attach.Jcmd(ctx, target, command)
	dur := float64(s.Now().Sub(start).Microseconds()) / 1000
	if err != nil {
		return "", 0, s.attachError(command, err)
	}
	return out, dur, nil
}

// ensureAttach checks the safety rules and starts the listener if needed.
func (s *Server) ensureAttach(ctx context.Context, t *target, what string) (attach.Target, *apiError) {
	target := attach.Target{HostPID: t.java.HostPID, NSPID: t.java.NSPID, TmpDir: t.java.TmpDir}
	if s.Attach == nil {
		return target, newError(CodeNotImplemented, "", nil)
	}
	_, uptime := jvmTimes(t.perf, s.nowMs())
	listener := s.SocketExists(t.java.AttachSocket())
	a := buildAttach(t.perf, listener, uptime)
	if !a.Available {
		return target, newError(CodeAttachUnavailable, "", map[string]any{"reason": *a.Reason})
	}
	if !listener {
		s.Log.Info("starting attach listener", "pid", target.HostPID, "ns_pid", target.NSPID)
		if err := s.Attach.Start(ctx, target); err != nil {
			return target, s.attachError(what, err)
		}
	}
	return target, nil
}

func (s *Server) attachError(command string, err error) *apiError {
	s.Log.Warn("attach failed", "command", command, "err", err)
	var ce *attach.CommandError
	switch {
	case errors.Is(err, attach.ErrTimeout):
		return newError(CodeAttachTimeout, "", nil)
	case errors.Is(err, attach.ErrTooLarge):
		return newError(CodeOutputTooLarge, "", nil)
	case errors.As(err, &ce):
		return newError(CodeAttachFailed, "", map[string]any{"code": ce.Code, "output": truncate(ce.Message, 300)})
	}
	return newError(CodeAttachFailed, "", nil)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Server) notImplemented(w http.ResponseWriter, r *http.Request, _ string) {
	s.fail(w, r, newError(CodeNotImplemented, "", nil))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, e *apiError) {
	if e.status >= 500 && e.status != http.StatusNotImplemented {
		s.Log.Warn("request failed", "method", r.Method, "path", r.URL.Path, "error", e.body.Error)
	}
	s.write(w, e.status, e.body)
}

func (s *Server) write(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		s.Log.Error("encode response", "err", err)
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(`{"error":"internal_error","message":"Internal error.","details":null}` + "\n")
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}
