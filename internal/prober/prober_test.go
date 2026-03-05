package prober_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"notashelf.dev/ncro/internal/prober"
)

func TestEMACalculation(t *testing.T) {
	p := prober.New(0.3)
	p.RecordLatency("https://example.com", 100)
	p.RecordLatency("https://example.com", 50)

	// EMA after 2 measurements: first=100, second = 0.3*50 + 0.7*100 = 85
	health := p.GetHealth("https://example.com")
	if health == nil {
		t.Fatal("expected health entry")
	}
	if health.EMALatency < 84 || health.EMALatency > 86 {
		t.Errorf("EMA = %.2f, want ~85", health.EMALatency)
	}
}

func TestStatusProgression(t *testing.T) {
	p := prober.New(0.3)
	p.RecordLatency("https://example.com", 10)

	for range 3 {
		p.RecordFailure("https://example.com")
	}
	h := p.GetHealth("https://example.com")
	if h.Status != prober.StatusDegraded {
		t.Errorf("status = %v, want Degraded after 3 failures", h.Status)
	}

	for range 7 {
		p.RecordFailure("https://example.com")
	}
	h = p.GetHealth("https://example.com")
	if h.Status != prober.StatusDown {
		t.Errorf("status = %v, want Down after 10 failures", h.Status)
	}
}

func TestRecoveryAfterSuccess(t *testing.T) {
	p := prober.New(0.3)
	for range 10 {
		p.RecordFailure("https://example.com")
	}
	p.RecordLatency("https://example.com", 20)
	h := p.GetHealth("https://example.com")
	if h.Status != prober.StatusActive {
		t.Errorf("status = %v, want Active after recovery", h.Status)
	}
	if h.ConsecutiveFails != 0 {
		t.Errorf("ConsecutiveFails = %d, want 0", h.ConsecutiveFails)
	}
}

func TestSortedByLatency(t *testing.T) {
	p := prober.New(0.3)
	p.RecordLatency("https://slow.example.com", 200)
	p.RecordLatency("https://fast.example.com", 10)
	p.RecordLatency("https://medium.example.com", 50)

	sorted := p.SortedByLatency()
	if len(sorted) != 3 {
		t.Fatalf("expected 3, got %d", len(sorted))
	}
	if sorted[0].URL != "https://fast.example.com" {
		t.Errorf("first = %q, want fast", sorted[0].URL)
	}
}

func TestProbeUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p := prober.New(0.3)
	p.ProbeUpstream(srv.URL)

	h := p.GetHealth(srv.URL)
	if h == nil || h.Status != prober.StatusActive {
		t.Errorf("expected Active after successful probe, got %v", h)
	}
}

func TestProbeUpstreamFailure(t *testing.T) {
	p := prober.New(0.3)
	p.ProbeUpstream("http://127.0.0.1:1") // nothing listening

	h := p.GetHealth("http://127.0.0.1:1")
	if h == nil || h.ConsecutiveFails == 0 {
		t.Error("expected failure recorded")
	}
}
