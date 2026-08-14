package arcade

import (
	"sync"
	"time"
)

// Phase 4: per-server resource history.
//
// A bounded in-memory ring, sampled on a fixed cadence. This is the shape
// `nucleus/ts` would take over when Nucleus is the default (PLAN.md §5) - the
// API below is deliberately the same one a timeseries store would expose, so
// the swap is a store implementation rather than a rewrite.
//
// It is not persisted: history is cheap to rebuild and a panel restart losing
// the last hour of graphs is not worth the write amplification.

type Sample struct {
	T       int64   `json:"t"`   // unix seconds
	CPU     float64 `json:"cpu"` // percent of the server's own limit
	MemMB   int     `json:"mem_mb"`
	Players int     `json:"players"`
}

const (
	sampleEvery     = 5 * time.Second
	samplesKept     = 720 // 1 hour at 5s
	hostSamplesKept = 2880
)

type Metrics struct {
	mu     sync.RWMutex
	series map[string][]Sample // server id -> ring
	host   []Sample            // aggregate across all servers
}

func NewMetrics() *Metrics {
	return &Metrics{series: map[string][]Sample{}}
}

func (m *Metrics) push(id string, s Sample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := append(m.series[id], s)
	if len(r) > samplesKept {
		r = r[len(r)-samplesKept:]
	}
	m.series[id] = r
}

func (m *Metrics) pushHost(s Sample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.host = append(m.host, s)
	if len(m.host) > hostSamplesKept {
		m.host = m.host[len(m.host)-hostSamplesKept:]
	}
}

// Series returns the samples inside the window, thinned to at most `points`
// so a long window doesn't ship 720 points to draw 200 pixels.
func (m *Metrics) Series(id string, window time.Duration, points int) []Sample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return thin(clip(m.series[id], window), points)
}

func (m *Metrics) HostSeries(window time.Duration, points int) []Sample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return thin(clip(m.host, window), points)
}

func (m *Metrics) drop(id string) {
	m.mu.Lock()
	delete(m.series, id)
	m.mu.Unlock()
}

func clip(in []Sample, window time.Duration) []Sample {
	if window <= 0 {
		return in
	}
	cutoff := time.Now().Add(-window).Unix()
	for i, s := range in {
		if s.T >= cutoff {
			return in[i:]
		}
	}
	return nil
}

func thin(in []Sample, points int) []Sample {
	if points <= 0 || len(in) <= points {
		out := make([]Sample, len(in))
		copy(out, in)
		return out
	}
	step := float64(len(in)) / float64(points)
	out := make([]Sample, 0, points)
	for i := 0; i < points; i++ {
		out = append(out, in[int(float64(i)*step)])
	}
	// always keep the newest sample so the graph ends at "now"
	out[len(out)-1] = in[len(in)-1]
	return out
}

// sampleLoop is the collector. One goroutine for the whole agent.
func (m *Manager) sampleLoop() {
	defer recoverPanic("metrics sampler")
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for range t.C {
		now := time.Now().Unix()
		var hostCPU float64
		var hostMem, hostPlayers int

		for _, s := range m.List() {
			s.mu.Lock()
			cpu, mem, players := s.cpuPct, s.memMB, len(s.players)
			running := s.Status == StatusRunning
			s.mu.Unlock()
			if !running {
				cpu, mem, players = 0, 0, 0
			}
			m.metrics.push(s.ID, Sample{T: now, CPU: round1(cpu), MemMB: mem, Players: players})

			// host CPU is expressed in vCPU-equivalents so servers with
			// different limits sum honestly
			hostCPU += cpu / 100 * s.CPU
			hostMem += mem
			hostPlayers += players
		}
		m.metrics.pushHost(Sample{T: now, CPU: round1(hostCPU), MemMB: hostMem, Players: hostPlayers})
	}
}

// guardLoop is the Phase 4 safety net: a server that sits at its memory ceiling
// is heading for an OOM kill, and the operator should hear it from the panel
// before the kernel says it with a SIGKILL.
func (m *Manager) guardLoop() {
	defer recoverPanic("resource guard")
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	warned := map[string]time.Time{}

	for range t.C {
		for _, s := range m.List() {
			s.mu.Lock()
			mem, limit, status := s.memMB, s.MemoryMB, s.Status //nolint:gocritic // already under s.mu
			s.mu.Unlock()
			if status != StatusRunning || limit == 0 {
				continue
			}
			pct := float64(mem) / float64(limit) * 100
			if pct < 92 {
				continue
			}
			if last, ok := warned[s.ID]; ok && time.Since(last) < 5*time.Minute {
				continue
			}
			warned[s.ID] = time.Now()
			m.panelLine(s, "warn", sprintf(
				"Memory is at %.0f%% of the %d MB ceiling. The kernel will kill this container if it reaches the limit - raise the limit or lower view-distance.",
				pct, limit))
			m.audit("system", "server.memory_pressure", s.ID, sprintf("%.0f%%", pct))
		}
	}
}
