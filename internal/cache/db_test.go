package cache_test

import (
	"os"
	"testing"
	"time"

	"notashelf.dev/ncro/internal/cache"
)

func newTestDB(t *testing.T) *cache.DB {
	t.Helper()
	f, err := os.CreateTemp("", "ncro-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	db, err := cache.Open(f.Name(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestGetSetRoute(t *testing.T) {
	db := newTestDB(t)

	entry := &cache.RouteEntry{
		StorePath:    "abc123xyz-hello-2.12",
		UpstreamURL:  "https://cache.nixos.org",
		LatencyMs:    12.5,
		LatencyEMA:   12.5,
		LastVerified: time.Now().UTC().Truncate(time.Second),
		QueryCount:   1,
		TTL:          time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}

	if err := db.SetRoute(entry); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}

	got, err := db.GetRoute("abc123xyz-hello-2.12")
	if err != nil {
		t.Fatalf("GetRoute: %v", err)
	}
	if got == nil {
		t.Fatal("GetRoute returned nil")
	}
	if got.UpstreamURL != entry.UpstreamURL {
		t.Errorf("upstream = %q, want %q", got.UpstreamURL, entry.UpstreamURL)
	}
	if got.QueryCount != 1 {
		t.Errorf("query_count = %d, want 1", got.QueryCount)
	}
}

func TestGetRouteNotFound(t *testing.T) {
	db := newTestDB(t)
	got, err := db.GetRoute("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestSetRouteUpsert(t *testing.T) {
	db := newTestDB(t)

	entry := &cache.RouteEntry{
		StorePath:   "abc123-pkg",
		UpstreamURL: "https://cache.nixos.org",
		LatencyMs:   20.0,
		LatencyEMA:  20.0,
		QueryCount:  1,
		TTL:         time.Now().Add(time.Hour),
	}
	db.SetRoute(entry)

	entry.LatencyEMA = 18.0
	entry.QueryCount = 2
	if err := db.SetRoute(entry); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, _ := db.GetRoute("abc123-pkg")
	if got.LatencyEMA != 18.0 {
		t.Errorf("ema = %f, want 18.0", got.LatencyEMA)
	}
	if got.QueryCount != 2 {
		t.Errorf("query_count = %d, want 2", got.QueryCount)
	}
}

func TestExpireOldRoutes(t *testing.T) {
	db := newTestDB(t)

	// Insert expired route
	expired := &cache.RouteEntry{
		StorePath:   "expired-pkg",
		UpstreamURL: "https://cache.nixos.org",
		TTL:         time.Now().Add(-time.Minute), // already expired
	}
	db.SetRoute(expired)

	// Insert valid route
	valid := &cache.RouteEntry{
		StorePath:   "valid-pkg",
		UpstreamURL: "https://cache.nixos.org",
		TTL:         time.Now().Add(time.Hour),
	}
	db.SetRoute(valid)

	if err := db.ExpireOldRoutes(); err != nil {
		t.Fatalf("ExpireOldRoutes: %v", err)
	}

	got, _ := db.GetRoute("expired-pkg")
	if got != nil {
		t.Error("expired route should have been deleted")
	}
	got2, _ := db.GetRoute("valid-pkg")
	if got2 == nil {
		t.Error("valid route should still exist")
	}
}

func TestLRUEviction(t *testing.T) {
	// Use maxEntries=3 to trigger eviction easily
	f, _ := os.CreateTemp("", "ncro-lru-*.db")
	f.Close()
	defer os.Remove(f.Name())

	db, _ := cache.Open(f.Name(), 3)
	defer db.Close()

	for i := range 4 {
		db.SetRoute(&cache.RouteEntry{
			StorePath:    "pkg-" + string(rune('a'+i)),
			UpstreamURL:  "https://cache.nixos.org",
			LastVerified: time.Now().Add(time.Duration(i) * time.Second),
			TTL:          time.Now().Add(time.Hour),
		})
	}

	count, err := db.RouteCount()
	if err != nil {
		t.Fatal(err)
	}
	if count > 3 {
		t.Errorf("expected count <= 3 after LRU eviction, got %d", count)
	}
}
