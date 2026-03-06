package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"notashelf.dev/ncro/internal/cache"
	"notashelf.dev/ncro/internal/metrics"
	"notashelf.dev/ncro/internal/narinfo"
	"notashelf.dev/ncro/internal/prober"
)

// Returned when all upstreams were reached but none had the path.
var ErrNotFound = errors.New("not found in any upstream")

// Returned when all upstreams failed with network errors.
var ErrUpstreamUnavailable = errors.New("all upstreams unavailable")

// Result of a Resolve call.
type Result struct {
	URL          string
	LatencyMs    float64
	CacheHit     bool
	NarInfoBytes []byte // raw narinfo response on cache miss; nil on cache hit
}

// Resolves store paths to the best upstream via cache lookup or parallel racing.
type Router struct {
	db           *cache.DB
	prober       *prober.Prober
	routeTTL     time.Duration
	raceTimeout  time.Duration
	negativeTTL  time.Duration
	client       *http.Client
	mu           sync.RWMutex
	upstreamKeys map[string]string // upstream URL → Nix public key string
	group        singleflight.Group
}

// Creates a Router.
func New(db *cache.DB, p *prober.Prober, routeTTL, raceTimeout, negativeTTL time.Duration) *Router {
	return &Router{
		db:           db,
		prober:       p,
		routeTTL:     routeTTL,
		raceTimeout:  raceTimeout,
		negativeTTL:  negativeTTL,
		client:       &http.Client{Timeout: raceTimeout},
		upstreamKeys: make(map[string]string),
	}
}

// Registers a Nix public key for narinfo signature verification on a given upstream.
// pubKeyStr must be in "name:base64(key)" format (e.g. "cache.nixos.org-1:...").
func (r *Router) SetUpstreamKey(url, pubKeyStr string) error {
	if _, _, err := narinfo.ParsePublicKey(pubKeyStr); err != nil {
		return err
	}
	r.mu.Lock()
	r.upstreamKeys[url] = pubKeyStr
	r.mu.Unlock()
	return nil
}

// Returns the best upstream for the given store hash.
// Checks the route cache first; on miss races the provided candidates.
func (r *Router) Resolve(storeHash string, candidates []string) (*Result, error) {
	entry, err := r.db.GetRoute(storeHash)
	if err == nil && entry != nil && entry.IsValid() {
		h := r.prober.GetHealth(entry.UpstreamURL)
		if h == nil || h.Status == prober.StatusActive {
			metrics.NarinfoCacheHits.Inc()
			return &Result{
				URL:       entry.UpstreamURL,
				LatencyMs: entry.LatencyEMA,
				CacheHit:  true,
			}, nil
		}
	}
	metrics.NarinfoCacheMisses.Inc()

	v, raceErr, _ := r.group.Do(storeHash, func() (interface{}, error) {
		return r.race(storeHash, candidates)
	})
	if raceErr != nil {
		return nil, raceErr
	}
	return v.(*Result), nil
}

type raceResult struct {
	url       string
	latencyMs float64
}

func (r *Router) race(storeHash string, candidates []string) (*Result, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates for %q", storeHash)
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.raceTimeout)
	defer cancel()

	ch := make(chan raceResult, len(candidates))
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		netErrs   int
		notFounds int
	)

	for _, u := range candidates {
		wg.Add(1)
		go func(upstream string) {
			defer wg.Done()
			start := time.Now()
			req, err := http.NewRequestWithContext(ctx, http.MethodHead,
				upstream+"/"+storeHash+".narinfo", nil)
			if err != nil {
				slog.Warn("bad upstream URL in race", "upstream", upstream, "error", err)
				mu.Lock()
				netErrs++
				mu.Unlock()
				return
			}
			resp, err := r.client.Do(req)
			if err != nil {
				mu.Lock()
				netErrs++
				mu.Unlock()
				return
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				mu.Lock()
				notFounds++
				mu.Unlock()
				return
			}
			ms := float64(time.Since(start).Nanoseconds()) / 1e6
			select {
			case ch <- raceResult{url: upstream, latencyMs: ms}:
			default:
			}
		}(u)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	winner, ok := <-ch
	if !ok {
		mu.Lock()
		ne, nf := netErrs, notFounds
		mu.Unlock()
		if ne > 0 && nf == 0 {
			return nil, ErrUpstreamUnavailable
		}
		return nil, ErrNotFound
	}
	cancel()

	for res := range ch {
		if res.latencyMs < winner.latencyMs {
			winner = res
		}
	}

	metrics.UpstreamRaceWins.WithLabelValues(winner.url).Inc()
	metrics.UpstreamLatency.WithLabelValues(winner.url).Observe(winner.latencyMs / 1000)

	// Fetch narinfo body to parse metadata and forward to caller.
	narInfoBytes, narHash, narSize := r.fetchNarInfo(winner.url, storeHash)

	health := r.prober.GetHealth(winner.url)
	ema := winner.latencyMs
	if health != nil {
		ema = 0.3*winner.latencyMs + 0.7*health.EMALatency
	}
	r.prober.RecordLatency(winner.url, winner.latencyMs)

	now := time.Now()
	_ = r.db.SetRoute(&cache.RouteEntry{
		StorePath:    storeHash,
		UpstreamURL:  winner.url,
		LatencyMs:    winner.latencyMs,
		LatencyEMA:   ema,
		LastVerified: now,
		QueryCount:   1,
		TTL:          now.Add(r.routeTTL),
		NarHash:      narHash,
		NarSize:      narSize,
	})

	return &Result{URL: winner.url, LatencyMs: winner.latencyMs, NarInfoBytes: narInfoBytes}, nil
}

// Fetches narinfo content from upstream, verifies its signature if a key is
// configured for that upstream, and returns (body, narHash, narSize).
// Returns (nil, "", 0) if the fetch fails or signature verification fails.
func (r *Router) fetchNarInfo(upstream, storeHash string) ([]byte, string, uint64) {
	url := upstream + "/" + storeHash + ".narinfo"
	resp, err := r.client.Get(url)
	if err != nil {
		return nil, "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", 0
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", 0
	}
	ni, err := narinfo.Parse(bytes.NewReader(body))
	if err != nil {
		return body, "", 0
	}
	r.mu.RLock()
	pubKeyStr := r.upstreamKeys[upstream]
	r.mu.RUnlock()
	if pubKeyStr != "" {
		ok, err := ni.Verify(pubKeyStr)
		if err != nil {
			slog.Warn("narinfo: public key parse error", "upstream", upstream, "error", err)
			return nil, "", 0
		}
		if !ok {
			slog.Warn("narinfo: signature verification failed", "upstream", upstream, "store", storeHash)
			return nil, "", 0
		}
	}
	return body, ni.NarHash, ni.NarSize
}
