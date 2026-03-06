package router_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"notashelf.dev/ncro/internal/cache"
	"notashelf.dev/ncro/internal/config"
	"notashelf.dev/ncro/internal/prober"
	"notashelf.dev/ncro/internal/router"
)

func newTestRouter(t *testing.T, upstreams ...string) (*router.Router, func()) {
	t.Helper()
	f, _ := os.CreateTemp("", "ncro-router-*.db")
	f.Close()
	db, err := cache.Open(f.Name(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	p := prober.New(0.3)
	for _, u := range upstreams {
		p.RecordLatency(u, 10)
	}
	r := router.New(db, p, time.Hour, 5*time.Second, 10*time.Minute)
	return r, func() {
		db.Close()
		os.Remove(f.Name())
	}
}

func TestRouteHit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "StorePath: /nix/store/abc123-hello")
	}))
	defer srv.Close()

	r, cleanup := newTestRouter(t, srv.URL)
	defer cleanup()

	result, err := r.Resolve("abc123", []string{srv.URL})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.URL != srv.URL {
		t.Errorf("url = %q, want %q", result.URL, srv.URL)
	}
	if result.LatencyMs <= 0 {
		t.Error("expected positive latency")
	}
}

func TestRouteRacePicksFastest(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer fast.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer slow.Close()

	r, cleanup := newTestRouter(t, fast.URL, slow.URL)
	defer cleanup()

	result, err := r.Resolve("somehash", []string{slow.URL, fast.URL})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.URL != fast.URL {
		t.Errorf("expected fast server to win, got %q", result.URL)
	}
}

func TestRouteAllFail(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	_, err := r.Resolve("somehash", []string{"http://127.0.0.1:1"})
	if err == nil {
		t.Error("expected error when all upstreams fail")
	}
}

func TestRouteAllNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	r, cleanup := newTestRouter(t, srv.URL)
	defer cleanup()

	_, err := r.Resolve("somehash", []string{srv.URL})
	if !errors.Is(err, router.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestRouteAllUnavailable(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	_, err := r.Resolve("somehash", []string{"http://127.0.0.1:1"})
	if !errors.Is(err, router.ErrUpstreamUnavailable) {
		t.Errorf("expected ErrUpstreamUnavailable, got %v", err)
	}
}

func TestRaceWithMalformedURL(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	_, err := r.Resolve("somehash", []string{"://bad-url"})
	if err == nil {
		t.Error("expected error for malformed upstream URL")
	}
}

func TestCacheHit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	r, cleanup := newTestRouter(t, srv.URL)
	defer cleanup()

	r.Resolve("abc123", []string{srv.URL})

	result, err := r.Resolve("abc123", []string{srv.URL})
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if !result.CacheHit {
		t.Error("expected cache hit on second resolve")
	}
}

func TestResolveWithDownUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	f, _ := os.CreateTemp("", "ncro-router-*.db")
	f.Close()
	db, _ := cache.Open(f.Name(), 1000)
	defer db.Close()
	defer os.Remove(f.Name())

	p := prober.New(0.3)
	p.RecordLatency(srv.URL, 10)
	// Force the upstream to StatusDown
	for range 10 {
		p.RecordFailure(srv.URL)
	}

	r := router.New(db, p, time.Hour, 5*time.Second, 10*time.Minute)
	// Router should still attempt the race (the race uses HEAD, not the prober status)
	// The upstream is actually healthy (httptest), so the race should succeed.
	result, err := r.Resolve("somehash", []string{srv.URL})
	if err != nil {
		t.Fatalf("Resolve with down-flagged upstream: %v", err)
	}
	if result.URL != srv.URL {
		t.Errorf("url = %q", result.URL)
	}
}

func TestNegativeCaching(t *testing.T) {
	var raceCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&raceCount, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	db, err := cache.Open(":memory:", 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := prober.New(0.3)
	p.InitUpstreams([]config.UpstreamConfig{{URL: ts.URL}})
	r := router.New(db, p, time.Hour, 5*time.Second, 10*time.Minute)

	_, err = r.Resolve("not-on-any-upstream", []string{ts.URL})
	if !errors.Is(err, router.ErrNotFound) {
		t.Fatalf("first resolve: expected ErrNotFound, got %v", err)
	}
	count1 := atomic.LoadInt32(&raceCount)

	_, err = r.Resolve("not-on-any-upstream", []string{ts.URL})
	if !errors.Is(err, router.ErrNotFound) {
		t.Fatalf("second resolve: expected ErrNotFound, got %v", err)
	}
	count2 := atomic.LoadInt32(&raceCount)

	if count2 != count1 {
		t.Errorf("second resolve hit upstream %d extra times, want 0 (should be negatively cached)", count2-count1)
	}
}

func TestSingleflightDedup(t *testing.T) {
	var headCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			atomic.AddInt32(&headCount, 1)
			time.Sleep(30 * time.Millisecond) // ensure goroutines overlap
			w.WriteHeader(http.StatusOK)
		} else {
			w.Header().Set("Content-Type", "text/x-nix-narinfo")
			fmt.Fprintln(w, "StorePath: /nix/store/abc123-test")
		}
	}))
	defer ts.Close()

	db, err := cache.Open(":memory:", 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := prober.New(0.3)
	p.InitUpstreams([]config.UpstreamConfig{{URL: ts.URL}})
	r := router.New(db, p, time.Hour, 5*time.Second, 10*time.Minute)

	const N = 10
	var wg sync.WaitGroup
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Resolve("abc123dedup", []string{ts.URL})
		}()
	}
	wg.Wait()

	if hc := atomic.LoadInt32(&headCount); hc > 1 {
		t.Errorf("upstream HEAD hit %d times for %d concurrent callers; want 1", hc, N)
	}
}
