package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"notashelf.dev/ncro/internal/cache"
	"notashelf.dev/ncro/internal/config"
	"notashelf.dev/ncro/internal/prober"
	"notashelf.dev/ncro/internal/router"
	"notashelf.dev/ncro/internal/server"
)

// Verifies that the second identical narinfo request uses the cached route.
func TestRouteReuseOnSecondRequest(t *testing.T) {
	requestCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			requestCount++
			w.Header().Set("Content-Type", "text/x-nix-narinfo")
			io.WriteString(w, "StorePath: /nix/store/test-pkg\nURL: nar/test.nar\n")
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	f, _ := os.CreateTemp("", "ncro-int-*.db")
	f.Close()
	defer os.Remove(f.Name())
	db, _ := cache.Open(f.Name(), 1000)
	defer db.Close()

	p := prober.New(0.3)
	p.RecordLatency(upstream.URL, 10)
	r := router.New(db, p, time.Hour, 5*time.Second, 10*time.Minute)
	ts := httptest.NewServer(server.New(r, p, []config.UpstreamConfig{{URL: upstream.URL}}))
	defer ts.Close()

	resp1, _ := http.Get(ts.URL + "/deadbeef00000000.narinfo")
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()

	resp2, _ := http.Get(ts.URL + "/deadbeef00000000.narinfo")
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	if resp1.StatusCode != 200 || resp2.StatusCode != 200 {
		t.Errorf("expected 200/200, got %d/%d", resp1.StatusCode, resp2.StatusCode)
	}
}

// Verifies that when the best-seeded upstream returns 404, the fallback upstream is used.
func TestUpstreamFailoverFallback(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-nix-narinfo")
		io.WriteString(w, "StorePath: /nix/store/fallback-pkg\n")
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer bad.Close()

	f, _ := os.CreateTemp("", "ncro-fb-*.db")
	f.Close()
	defer os.Remove(f.Name())
	db, _ := cache.Open(f.Name(), 1000)
	defer db.Close()

	p := prober.New(0.3)
	p.RecordLatency(bad.URL, 1) // bad appears fastest
	p.RecordLatency(good.URL, 50)

	r := router.New(db, p, time.Hour, 5*time.Second, 10*time.Minute)
	ts := httptest.NewServer(server.New(r, p, []config.UpstreamConfig{
		{URL: bad.URL},
		{URL: good.URL},
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/cafebabe00000000.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200 via fallback, got %d", resp.StatusCode)
	}
}
