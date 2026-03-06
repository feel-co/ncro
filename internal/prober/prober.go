package prober

import (
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"notashelf.dev/ncro/internal/config"
)

// Upstream health status.
type Status int

const (
	StatusActive   Status = iota
	StatusDegraded        // 3+ consecutive failures
	StatusDown            // 10+ consecutive failures
)

func (s Status) String() string {
	switch s {
	case StatusActive:
		return "ACTIVE"
	case StatusDegraded:
		return "DEGRADED"
	default:
		return "DOWN"
	}
}

// In-memory metrics for one upstream.
type UpstreamHealth struct {
	URL              string
	Priority         int
	EMALatency       float64
	LastProbe        time.Time
	ConsecutiveFails uint32
	TotalQueries     uint64
	Status           Status
}

// Tracks latency and health for a set of upstreams.
type Prober struct {
	mu            sync.RWMutex
	alpha         float64
	table         map[string]*UpstreamHealth
	client        *http.Client
	persistHealth func(url string, ema float64, consecutiveFails uint32, totalQueries uint64)
}

// Creates a Prober with the given EMA alpha coefficient.
func New(alpha float64) *Prober {
	return &Prober{
		alpha: alpha,
		table: make(map[string]*UpstreamHealth),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Seeds the prober with upstream configs (records priority, no measurements yet).
func (p *Prober) InitUpstreams(upstreams []config.UpstreamConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range upstreams {
		if _, ok := p.table[u.URL]; !ok {
			p.table[u.URL] = &UpstreamHealth{URL: u.URL, Priority: u.Priority, Status: StatusActive}
		}
	}
}

// Derives Status from the number of consecutive failures, matching the logic
// in RecordFailure.
func computeStatus(consecutiveFails uint32) Status {
	switch {
	case consecutiveFails >= 10:
		return StatusDown
	case consecutiveFails >= 3:
		return StatusDegraded
	default:
		return StatusActive
	}
}

// Seeds an upstream's health state from persisted data. Should be called
// after InitUpstreams to restore state from the previous run.
func (p *Prober) Seed(url string, emaLatency float64, consecutiveFails int, totalQueries int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.table[url]
	if !ok {
		return
	}
	h.EMALatency = emaLatency
	h.TotalQueries = uint64(totalQueries)
	h.ConsecutiveFails = uint32(consecutiveFails)
	h.Status = computeStatus(uint32(consecutiveFails))
}

// Registers a callback invoked after each RecordLatency or RecordFailure call.
// The callback runs in a separate goroutine and must be safe for concurrent use.
func (p *Prober) SetHealthPersistence(fn func(url string, ema float64, consecutiveFails uint32, totalQueries uint64)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persistHealth = fn
}

// Records a successful latency measurement and updates the EMA.
func (p *Prober) RecordLatency(url string, ms float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.getOrCreate(url)
	if h.TotalQueries == 0 {
		h.EMALatency = ms
	} else {
		h.EMALatency = p.alpha*ms + (1-p.alpha)*h.EMALatency
	}
	h.ConsecutiveFails = 0
	h.TotalQueries++
	h.Status = StatusActive
	h.LastProbe = time.Now()
	if p.persistHealth != nil {
		u, ema, cf, tq := h.URL, h.EMALatency, h.ConsecutiveFails, h.TotalQueries
		fn := p.persistHealth
		go fn(u, ema, cf, tq)
	}
}

// Records a probe failure.
func (p *Prober) RecordFailure(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.getOrCreate(url)
	h.ConsecutiveFails++
	switch {
	case h.ConsecutiveFails >= 10:
		h.Status = StatusDown
	case h.ConsecutiveFails >= 3:
		h.Status = StatusDegraded
	}
	if p.persistHealth != nil {
		u, ema, cf, tq := h.URL, h.EMALatency, h.ConsecutiveFails, h.TotalQueries
		fn := p.persistHealth
		go fn(u, ema, cf, tq)
	}
}

// Returns a copy of the health entry for url, or nil if unknown.
func (p *Prober) GetHealth(url string) *UpstreamHealth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.table[url]
	if !ok {
		return nil
	}
	cp := *h
	return &cp
}

// Returns all known upstreams sorted by EMA latency ascending.
// DOWN upstreams are sorted last. Within 10% EMA difference, lower Priority wins.
func (p *Prober) SortedByLatency() []*UpstreamHealth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*UpstreamHealth, 0, len(p.table))
	for _, h := range p.table {
		cp := *h
		result = append(result, &cp)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		aDown := a.Status == StatusDown
		bDown := b.Status == StatusDown
		if aDown != bDown {
			return bDown // non-down first
		}
		// Within 10% latency difference: prefer lower priority number, then lower latency.
		if b.EMALatency > 0 && math.Abs(a.EMALatency-b.EMALatency)/b.EMALatency < 0.10 {
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
		}
		return a.EMALatency < b.EMALatency
	})
	return result
}

// Performs a HEAD /nix-cache-info against url and updates health.
func (p *Prober) ProbeUpstream(url string) {
	start := time.Now()
	resp, err := p.client.Head(url + "/nix-cache-info")
	elapsed := float64(time.Since(start).Nanoseconds()) / 1e6

	if err != nil || resp.StatusCode != 200 {
		p.RecordFailure(url)
		return
	}
	resp.Body.Close()
	p.RecordLatency(url, elapsed)
}

// Probes all known upstreams on interval until stop is closed.
func (p *Prober) RunProbeLoop(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.mu.RLock()
			urls := make([]string, 0, len(p.table))
			for u := range p.table {
				urls = append(urls, u)
			}
			p.mu.RUnlock()
			for _, u := range urls {
				go p.ProbeUpstream(u)
			}
		}
	}
}

func (p *Prober) getOrCreate(url string) *UpstreamHealth {
	h, ok := p.table[url]
	if !ok {
		h = &UpstreamHealth{URL: url, Status: StatusActive}
		p.table[url] = h
	}
	return h
}
