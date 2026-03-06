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

func TestRouteEntryIsValidExpired(t *testing.T) {
	expired := &cache.RouteEntry{TTL: time.Now().Add(-time.Minute)}
	if expired.IsValid() {
		t.Error("expired entry should not be valid")
	}
}

func TestRouteEntryIsValidFuture(t *testing.T) {
	valid := &cache.RouteEntry{TTL: time.Now().Add(time.Hour)}
	if !valid.IsValid() {
		t.Error("future-TTL entry should be valid")
	}
}

func TestDBOpenCreatesSchema(t *testing.T) {
	db := newTestDB(t)
	// RouteCount works only if schema was created.
	count, err := db.RouteCount()
	if err != nil {
		t.Fatalf("RouteCount after fresh open: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 routes in fresh DB, got %d", count)
	}
}

func TestRouteCountAfterExpiry(t *testing.T) {
	db := newTestDB(t)

	for i := range 3 {
		ttl := time.Now().Add(-time.Minute) // all expired
		db.SetRoute(&cache.RouteEntry{
			StorePath:   "pkg-" + string(rune('a'+i)),
			UpstreamURL: "https://cache.nixos.org",
			TTL:         ttl,
		})
	}

	before, _ := db.RouteCount()
	if err := db.ExpireOldRoutes(); err != nil {
		t.Fatal(err)
	}
	after, _ := db.RouteCount()
	if after >= before {
		t.Errorf("count did not decrease after expiry: before=%d after=%d", before, after)
	}
	if after != 0 {
		t.Errorf("expected 0 routes after expiring all, got %d", after)
	}
}

func TestNegativeCacheSetAndCheck(t *testing.T) {
	db, err := cache.Open(":memory:", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	neg, err := db.IsNegative("missing-path")
	if err != nil {
		t.Fatalf("IsNegative: %v", err)
	}
	if neg {
		t.Error("expected false for unknown path")
	}

	if err := db.SetNegative("missing-path", 10*time.Minute); err != nil {
		t.Fatalf("SetNegative: %v", err)
	}

	neg, err = db.IsNegative("missing-path")
	if err != nil {
		t.Fatalf("IsNegative after set: %v", err)
	}
	if !neg {
		t.Error("expected true after SetNegative")
	}
}

func TestNegativeCacheExpiry(t *testing.T) {
	db, err := cache.Open(":memory:", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Set with negative duration so it's already expired.
	if err := db.SetNegative("expires-now", -time.Second); err != nil {
		t.Fatalf("SetNegative: %v", err)
	}
	if err := db.ExpireNegatives(); err != nil {
		t.Fatalf("ExpireNegatives: %v", err)
	}

	neg, _ := db.IsNegative("expires-now")
	if neg {
		t.Error("expired negative should not be returned")
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
