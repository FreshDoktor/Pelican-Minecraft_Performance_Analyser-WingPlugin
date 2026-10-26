package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

// TimeoutProfileStart covers listener start, library copy and "load start".
var TimeoutProfileStart = 15 * time.Second

// Default sampling intervals.
const (
	DefaultWallIntervalMs = 50
	DefaultCPUIntervalMs  = 10
)

func (s *Server) profiles(w http.ResponseWriter, r *http.Request, uuid string) {
	if s.Profiles == nil {
		s.notImplemented(w, r, uuid)
		return
	}
	if r.Method == http.MethodPost {
		s.startProfile(w, r, uuid)
		return
	}
	// The server must exist; its state does not matter for the list.
	if _, e := s.resolve(r.Context(), uuid); e != nil {
		s.fail(w, r, e)
		return
	}
	list := ProfileList{Profiles: []Profile{}}
	for _, run := range s.Profiles.List(uuid) {
		p := s.profileJSON(run)
		p.Result = nil
		list.Profiles = append(list.Profiles, p)
	}
	s.write(w, http.StatusOK, list)
}

func (s *Server) profileByID(w http.ResponseWriter, r *http.Request, uuid, id string) {
	if s.Profiles == nil {
		s.notImplemented(w, r, uuid)
		return
	}
	if r.Method == http.MethodDelete {
		if !s.Profiles.Delete(uuid, id) {
			s.fail(w, r, newError(CodeProfileNotFound, "", nil))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	run, ok := s.Profiles.Get(uuid, id)
	if !ok {
		s.fail(w, r, newError(CodeProfileNotFound, "", nil))
		return
	}
	s.write(w, http.StatusOK, s.profileJSON(run))
}

func invalid(field, reason string) *apiError {
	return newError(CodeInvalidRequest, "Invalid field "+field+".", map[string]any{"field": field, "reason": reason})
}

// parseProfileRequest validates the body; nothing of it reaches a command
// except the checked integers and the mode enum.
func (s *Server) parseProfileRequest(r *http.Request) (profile.Request, *apiError) {
	var body ProfileRequest
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return profile.Request{}, invalid("body", "larger than 4 KiB")
		}
		return profile.Request{}, invalid("body", "must be a JSON object with seconds, mode, interval_ms: "+err.Error())
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return profile.Request{}, invalid("body", "must contain one JSON object")
	}
	max := s.Limits.MaxProfileSeconds
	if body.Seconds < 1 || body.Seconds > max {
		return profile.Request{}, invalid("seconds", "must be between 1 and "+strconv.Itoa(max))
	}
	req := profile.Request{Mode: profile.ModeWall, Seconds: body.Seconds}
	if body.Mode != nil {
		req.Mode = *body.Mode
	}
	switch req.Mode {
	case profile.ModeWall:
		req.IntervalMs = DefaultWallIntervalMs
	case profile.ModeCPU:
		req.IntervalMs = DefaultCPUIntervalMs
	case profile.ModeAlloc:
		if body.IntervalMs != nil {
			return profile.Request{}, invalid("interval_ms", "not supported for mode alloc")
		}
	default:
		return profile.Request{}, invalid("mode", "must be wall, cpu or alloc")
	}
	if body.IntervalMs != nil {
		if *body.IntervalMs < 1 || *body.IntervalMs > 1000 {
			return profile.Request{}, invalid("interval_ms", "must be between 1 and 1000")
		}
		req.IntervalMs = *body.IntervalMs
	}
	return req, nil
}

func (s *Server) startProfile(w http.ResponseWriter, r *http.Request, uuid string) {
	req, e := s.parseProfileRequest(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	t, e := s.ready(r.Context(), uuid)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), TimeoutProfileStart)
	defer cancel()
	at, e := s.ensureAttach(ctx, t, "profile")
	if e != nil {
		s.fail(w, r, e)
		return
	}
	started, _ := jvmTimes(t.perf, s.nowMs())
	pt := profile.Target{ServerUUID: uuid, Attach: at, Hsperf: t.java.Hsperf}
	if started != nil {
		pt.JVMStartedAt = *started
	}
	run, err := s.Profiles.Start(ctx, pt, req)
	if err != nil {
		var re *profile.RunningError
		var te *profile.TmpSpaceError
		switch {
		case errors.As(err, &re):
			e = newError(CodeProfileRunning, "", map[string]any{"profile_id": re.ID})
		case errors.Is(err, profile.ErrTooMany):
			e = newError(CodeTooManyProfiles, "", map[string]any{"limit": s.Limits.MaxConcurrentProfiles})
		case errors.As(err, &te):
			e = newError(CodeInsufficientTmpSpace, "", map[string]any{"free_bytes": te.Free, "required_bytes": te.Required})
		default:
			s.Log.Warn("profile start failed", "uuid", uuid, "err", err)
			var se *profile.SetupError
			if errors.As(err, &se) && !errors.Is(err, ctmp.ErrGone) {
				e = newError(CodeInternal, "", nil)
			} else {
				pe := profile.ErrorFor(err)
				e = newError(pe.Code, "", pe.Details)
			}
		}
		s.fail(w, r, e)
		return
	}
	w.Header().Set("Location", "/api/v1/servers/"+uuid+"/profiles/"+run.ID)
	s.write(w, http.StatusAccepted, s.profileJSON(run))
}

func (s *Server) profileJSON(run profile.Run) Profile {
	now := s.Now()
	p := Profile{
		ID: run.ID, ServerUUID: run.ServerUUID, Mode: run.Mode, Seconds: run.Seconds,
		State: run.State, StartedAt: run.StartedAt.UnixMilli(), EndsAt: run.EndsAt.UnixMilli(),
	}
	if run.IntervalMs > 0 {
		p.IntervalMs = ptr(run.IntervalMs)
	}
	if run.AllocIntervalBytes > 0 {
		p.AllocIntervalBytes = ptr(run.AllocIntervalBytes)
	}
	end := now
	if !run.FinishedAt.IsZero() {
		end = run.FinishedAt
		p.FinishedAt = ptr(run.FinishedAt.UnixMilli())
		p.ExpiresAt = ptr(run.ExpiresAt.UnixMilli())
	}
	p.ElapsedMs = max(end.Sub(run.StartedAt).Milliseconds(), 0)
	p.Progress = math.Min(1, round3(float64(p.ElapsedMs)/float64(run.Seconds*1000)))
	if run.State == profile.StateDone {
		p.Progress = 1
	}
	if run.Err != nil {
		p.Error = &ErrorBody{Error: run.Err.Code, Message: run.Err.Message, Details: run.Err.Details}
	}
	if res := run.Result; res != nil {
		threads, _ := BuildProfileThreads(res.Threads)
		pr := &ProfileResult{
			DurationMs: round3(float64(res.Duration.Microseconds()) / 1000),
			Unit:       res.Unit,
			IntervalMs: p.IntervalMs,
			Total:      res.Total,
			Warnings:   append([]string{}, res.Warnings...),
			Threads:    threads,
			GC:         ProfileGC{Collectors: []ProfileCollector{}, Pauses: []GCPause{}, PausesComplete: res.PausesComplete},
			Safepoints: ProfileSafepoints{Count: res.SafepointCount, TimeMs: res.SafepointMs},
			CPU:        ProfileCPU{ProcessMs: round3(res.CPUMs)},
		}
		var gcMs float64
		for _, c := range res.Collectors {
			pr.GC.Collectors = append(pr.GC.Collectors, ProfileCollector{Index: c.Index, Name: c.Name,
				Kind: hsperf.CollectorKind(c.Name), TimeIsPause: hsperf.TimeIsPause(c.Name), Count: c.Count, TimeMs: c.TimeMs})
			pr.GC.Count += c.Count
			gcMs += c.TimeMs
		}
		pr.GC.TimeMs = round3(gcMs)
		for _, pa := range res.Pauses {
			pr.GC.Pauses = append(pr.GC.Pauses, GCPause{CollectorIndex: pa.CollectorIndex, Kind: hsperf.CollectorKind(pa.Name),
				StartOffsetMs: pa.StartOffsetMs, DurationMs: pa.DurationMs})
		}
		if pr.DurationMs > 0 {
			pr.CPU.ProcessPercent = round3(res.CPUMs / pr.DurationMs * 100)
		}
		p.Result = pr
	}
	return p
}

// CheckServer tells why a JVM disappeared during a profile.
func (s *Server) CheckServer(ctx context.Context, uuid string) *profile.Error {
	t, e := s.resolve(ctx, uuid)
	switch {
	case e != nil:
		return &profile.Error{Code: e.body.Error, Message: e.body.Message, Details: e.body.Details}
	case !t.info.Running:
		return &profile.Error{Code: CodeServerNotRunning, Message: "The server stopped during the profile.", Details: map[string]any{"container_state": t.info.State}}
	}
	return &profile.Error{Code: CodeNoJavaProcess, Message: "The Java process ended during the profile."}
}

// ContainerLister lists the names (UUIDs) of the Pelican server containers.
type ContainerLister interface {
	List(ctx context.Context) ([]string, error)
}

// CleanupAll removes leftover profiler files from every running server,
// e.g. after a restart of the bridge. It never sends a signal.
func (s *Server) CleanupAll(ctx context.Context, l ContainerLister) {
	if s.Profiles == nil {
		return
	}
	uuids, err := l.List(ctx)
	if err != nil {
		s.Log.Warn("cleanup: cannot list containers", "err", err)
		return
	}
	for _, uuid := range uuids {
		s.cleanup(ctx, uuid)
	}
}

// cleanup runs in its own goroutine at the start: a panic (it reads what the
// tenant's container holds) is logged and skips the server, it must not end
// the bridge.
func (s *Server) cleanup(ctx context.Context, uuid string) {
	defer func() {
		if v := recover(); v != nil {
			s.Log.Error("cleanup panicked", "uuid", uuid, "panic", v, "stack", string(debug.Stack()))
		}
	}()
	t, e := s.resolve(ctx, uuid)
	if e != nil || t.java == nil {
		return
	}
	j := *t.java
	s.Profiles.Cleanup(ctx, attach.Target{HostPID: j.HostPID, NSPID: j.NSPID, Tmp: j.Tmp, StartTime: j.StartTime}, s.SocketExists(j.AttachSocket()))
}
