package server_test

import (
	"fmt"
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

func makeTestServer(t *testing.T, upstreams ...string) *httptest.Server {
	t.Helper()
	f, _ := os.CreateTemp("", "ncro-srv-*.db")
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	db, err := cache.Open(f.Name(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	p := prober.New(0.3)
	for _, u := range upstreams {
		p.RecordLatency(u, 10)
	}

	upsCfg := make([]config.UpstreamConfig, len(upstreams))
	for i, u := range upstreams {
		upsCfg[i] = config.UpstreamConfig{URL: u}
	}

	r := router.New(db, p, time.Hour, 5*time.Second, 10*time.Minute)
	return httptest.NewServer(server.New(r, p, upsCfg))
}

func TestNixCacheInfo(t *testing.T) {
	ts := makeTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nix-cache-info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "StoreDir:") {
		t.Errorf("body missing StoreDir: %q", body)
	}
}

func TestCacheInfoFields(t *testing.T) {
	ts := makeTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nix-cache-info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{"StoreDir:", "WantMassQuery:", "Priority:"} {
		if !strings.Contains(s, want) {
			t.Errorf("nix-cache-info missing %q", want)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	ts := makeTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	ts := makeTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestNarinfoProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			w.Header().Set("Content-Type", "text/x-nix-narinfo")
			fmt.Fprint(w, "StorePath: /nix/store/abc123-hello-2.12\nURL: nar/abc123.nar\nCompression: none\n")
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	ts := makeTestServer(t, upstream.URL)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/abc123def456.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("narinfo status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "StorePath:") {
		t.Errorf("expected narinfo body, got: %q", body)
	}
}

func TestNarinfoHEADRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			w.Header().Set("Content-Type", "text/x-nix-narinfo")
			fmt.Fprint(w, "StorePath: /nix/store/abc-head-test\nURL: nar/abc.nar\n")
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	ts := makeTestServer(t, upstream.URL)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/abc123.narinfo", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("HEAD narinfo status = %d, want 200", resp.StatusCode)
	}
}

func TestNarinfoNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	ts := makeTestServer(t, upstream.URL)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/notfound000000.narinfo")
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestNarinfoUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer upstream.Close()

	ts := makeTestServer(t, upstream.URL)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/abc123.narinfo")
	// 404 (not found) or 502 (upstream error) are both acceptable
	if resp.StatusCode == 200 {
		t.Errorf("expected non-200 for upstream error, got %d", resp.StatusCode)
	}
}

func TestNarinfoNoUpstreams(t *testing.T) {
	ts := makeTestServer(t) // no upstreams
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/abc123.narinfo")
	if resp.StatusCode == 200 {
		t.Error("expected non-200 with no upstreams")
	}
}

func TestUnknownPath(t *testing.T) {
	ts := makeTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/unknown/path")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestNARStreamingPassthrough(t *testing.T) {
	narContent := []byte("fake-nar-content-bytes")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/nar/") {
			w.Header().Set("Content-Type", "application/x-nix-archive")
			w.Write(narContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	ts := makeTestServer(t, upstream.URL)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nar/abc123.nar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("NAR status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != string(narContent) {
		t.Errorf("NAR body mismatch: got %q, want %q", body, narContent)
	}
}

func TestNARRangeHeaderForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/nar/") {
			if r.Header.Get("Range") == "" {
				http.Error(w, "Range header missing", 400)
				return
			}
			w.WriteHeader(206)
			w.Write([]byte("partial"))
			return
		}
		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	ts := makeTestServer(t, upstream.URL)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/nar/abc.nar", nil)
	req.Header.Set("Range", "bytes=0-1023")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 206 {
		t.Errorf("Range request status = %d, want 206", resp.StatusCode)
	}
}

func TestNARFallbackWhenFirstUpstreamMissing(t *testing.T) {
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer missing.Close()

	hasIt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nix-archive")
		w.Write([]byte("nar-bytes"))
	}))
	defer hasIt.Close()

	f, _ := os.CreateTemp("", "ncro-nar-fallback-*.db")
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	db, _ := cache.Open(f.Name(), 1000)
	t.Cleanup(func() { db.Close() })

	p := prober.New(0.3)
	// missing appears faster
	p.RecordLatency(missing.URL, 1)
	p.RecordLatency(hasIt.URL, 50)

	upsCfg := []config.UpstreamConfig{{URL: missing.URL}, {URL: hasIt.URL}}
	r := router.New(db, p, time.Hour, 5*time.Second, 10*time.Minute)
	ts := httptest.NewServer(server.New(r, p, upsCfg))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nar/abc123.nar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected fallback NAR response 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "nar-bytes" {
		t.Errorf("NAR body = %q, want nar-bytes", body)
	}
}
