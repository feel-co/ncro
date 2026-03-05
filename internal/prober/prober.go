package prober

import (
	"net/http"
	"sort"
	"sync"
	"time"
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
	EMALatency       float64
	LastProbe        time.Time
	ConsecutiveFails uint32
	TotalQueries     uint64
	Status           Status
}

// Tracks latency and health for a set of upstreams.
type Prober struct {
	mu     sync.RWMutex
	alpha  float64
	table  map[string]*UpstreamHealth
	client *http.Client
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

// Seeds the prober with upstream URLs (no measurements yet).
func (p *Prober) InitUpstreams(urls []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range urls {
		if _, ok := p.table[u]; !ok {
			p.table[u] = &UpstreamHealth{URL: u, Status: StatusActive}
		}
	}
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

// Returns all known upstreams sorted by EMA latency (ascending). DOWN upstreams last.
func (p *Prober) SortedByLatency() []*UpstreamHealth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*UpstreamHealth, 0, len(p.table))
	for _, h := range p.table {
		cp := *h
		result = append(result, &cp)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Status == StatusDown && result[j].Status != StatusDown {
			return false
		}
		if result[j].Status == StatusDown && result[i].Status != StatusDown {
			return true
		}
		return result[i].EMALatency < result[j].EMALatency
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
