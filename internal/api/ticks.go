package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

// TimeoutTicksStart covers listener start, library copy and the first "start".
var TimeoutTicksStart = 15 * time.Second

// ticks serves GET /ticks: starts the tick monitor of the server on the first
// call and returns its windows. The monitor ends when nobody asked for a minute.
func (s *Server) ticks(w http.ResponseWriter, r *http.Request, uuid string) {
	if s.Ticks == nil {
		s.notImplemented(w, r, uuid)
		return
	}
	t, e := s.ready(r.Context(), uuid)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	started, uptime := jvmTimes(t.perf, s.nowMs())
	if a := buildAttach(t.perf, s.SocketExists(t.java.AttachSocket()), uptime); !a.Available && *a.Reason == ReasonJVMStarting {
		// Temporary: the listener may only be started once the JVM is 30 s old.
		s.write(w, http.StatusOK, s.ticksJSON(profile.TickSnapshot{State: profile.TickStateStarting, Reason: ReasonJVMStarting}))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), TimeoutTicksStart)
	defer cancel()
	at, e := s.ensureAttach(ctx, t, "ticks")
	if e != nil {
		s.fail(w, r, e)
		return
	}
	pt := profile.Target{ServerUUID: uuid, Attach: at, Hsperf: t.java.Hsperf}
	if started != nil {
		pt.JVMStartedAt = *started
	}
	snap, err := s.Ticks.Get(ctx, pt)
	if err != nil {
		var te *profile.TmpSpaceError
		var se *profile.SetupError
		switch {
		case errors.Is(err, profile.ErrTooManyMonitors):
			e = newError(CodeTooManyTickMonitors, "", map[string]any{"limit": s.Limits.MaxTickMonitors})
		case errors.As(err, &te):
			e = newError(CodeInsufficientTmpSpace, "", map[string]any{"free_bytes": te.Free, "required_bytes": te.Required})
		case errors.As(err, &se) && !errors.Is(err, ctmp.ErrGone):
			s.Log.Warn("tick monitor start failed", "uuid", uuid, "err", err)
			e = newError(CodeInternal, "", nil)
		default:
			s.Log.Warn("tick monitor start failed", "uuid", uuid, "err", err)
			pe := profile.ErrorFor(err)
			e = newError(pe.Code, "", pe.Details)
		}
		s.fail(w, r, e)
		return
	}
	s.write(w, http.StatusOK, s.ticksJSON(snap))
}

func (s *Server) ticksJSON(snap profile.TickSnapshot) TicksResponse {
	resp := TicksResponse{
		SampledAt:     s.nowMs(),
		State:         snap.State,
		Reason:        nonEmpty(snap.Reason),
		Method:        nonEmpty(snap.Method),
		Mapping:       nonEmpty(snap.Mapping),
		TargetTPS:     profile.TickTargetTPS,
		WindowMs:      profile.TickWindow.Milliseconds(),
		IntervalMs:    profile.TickIntervalMs,
		IdleTimeoutMs: profile.TickIdleTimeout.Milliseconds(),
		History:       []TickWindow{},
	}
	if !snap.StartedAt.IsZero() {
		resp.MonitorStartedAt = ptr(snap.StartedAt.UnixMilli())
	}
	if snap.Err != nil {
		resp.Error = &ErrorBody{Error: snap.Err.Code, Message: snap.Err.Message, Details: snap.Err.Details}
	}
	for _, w := range snap.Windows {
		tw := TickWindow{
			StartedAt:  w.StartedAt.UnixMilli(),
			EndedAt:    w.EndedAt.UnixMilli(),
			DurationMs: round3(float64(w.EndedAt.Sub(w.StartedAt).Microseconds()) / 1000),
			State:      w.State,
			Method:     nonEmpty(w.Method),
			Overloaded: w.Overloaded,
		}
		if w.TPS != nil {
			tw.TPS = ptr(round2(*w.TPS))
		}
		if w.TickMs != nil {
			tw.TickTimeMs = ptr(round3(*w.TickMs))
		}
		if w.Load != nil {
			tw.LoadPercent = ptr(round2(*w.Load * 100))
		}
		switch w.Method {
		case profile.TickMethodSampling:
			tw.Samples = ptr(w.Samples)
		case profile.TickMethodCounting:
			tw.Ticks = ptr(w.Ticks)
		}
		resp.History = append(resp.History, tw)
	}
	if n := len(resp.History); n > 0 {
		resp.Current = &resp.History[n-1]
	}
	return resp
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
