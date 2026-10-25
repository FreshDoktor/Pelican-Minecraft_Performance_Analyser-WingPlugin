package profile

import (
	"math"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
)

// gcTracker observes collections during a profile by polling hsperfdata:
// every new lastEntryTime of a collector is one collection.
type gcTracker struct {
	startUptimeMs float64 // profile start, ms since JVM start
	seen          map[int]int64
	pauses        []Pause
}

func newGCTracker(before *hsperf.Data, startUptimeMs int64) *gcTracker {
	g := &gcTracker{startUptimeMs: float64(startUptimeMs), seen: map[int]int64{}}
	for _, c := range before.Collectors() {
		g.seen[c.Index] = c.LastEntry
	}
	return g
}

func (g *gcTracker) observe(d *hsperf.Data) {
	for _, c := range d.Collectors() {
		if c.LastEntry == g.seen[c.Index] || c.LastExit < c.LastEntry {
			continue // nothing new, or still running
		}
		g.seen[c.Index] = c.LastEntry
		g.pauses = append(g.pauses, Pause{
			CollectorIndex: c.Index,
			Name:           c.Name,
			StartOffsetMs:  math.Max(0, hsperf.RoundMs(d.TicksToMs(c.LastEntry)-g.startUptimeMs)),
			DurationMs:     d.TicksToMs(c.LastExit - c.LastEntry),
		})
	}
}

// result returns the counter deltas, the observed pauses and whether every
// collection was observed.
func (g *gcTracker) result(before, after *hsperf.Data) ([]CollectorDelta, []Pause, bool) {
	prev := map[int]hsperf.Collector{}
	for _, c := range before.Collectors() {
		prev[c.Index] = c
	}
	var total int64
	out := []CollectorDelta{}
	for _, c := range after.Collectors() {
		p := prev[c.Index]
		cd := CollectorDelta{Index: c.Index, Name: c.Name, Count: c.Invocations - p.Invocations, TimeMs: after.TicksToMs(c.Time - p.Time)}
		total += cd.Count
		out = append(out, cd)
	}
	pauses := append([]Pause{}, g.pauses...)
	return out, pauses, int64(len(pauses)) == total
}
