package mesh_test

import (
	"testing"
	"time"

	"notashelf.dev/ncro/internal/cache"
	"notashelf.dev/ncro/internal/mesh"
)

func TestSignVerify(t *testing.T) {
	node, err := mesh.NewNode("", nil)
	if err != nil {
		t.Fatal(err)
	}

	msg := mesh.Message{
		Type:      mesh.MsgAnnounce,
		NodeID:    node.ID(),
		Timestamp: time.Now().UnixNano(),
		Routes:    []cache.RouteEntry{{StorePath: "abc123", UpstreamURL: "https://cache.nixos.org"}},
	}

	data, sig, err := node.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := mesh.Verify(node.PublicKey(), data, sig); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerifyFailsOnTamper(t *testing.T) {
	node, _ := mesh.NewNode("", nil)
	msg := mesh.Message{Type: mesh.MsgAnnounce, NodeID: node.ID()}
	data, sig, _ := node.Sign(msg)
	data[0] ^= 0xFF
	if err := mesh.Verify(node.PublicKey(), data, sig); err == nil {
		t.Error("expected verification failure on tampered data")
	}
}

func TestMergeLowerLatencyWins(t *testing.T) {
	store := mesh.NewRouteStore()
	store.Merge([]cache.RouteEntry{
		{StorePath: "pkg-a", UpstreamURL: "https://slow.example.com", LatencyEMA: 200, TTL: time.Now().Add(time.Hour)},
	})
	store.Merge([]cache.RouteEntry{
		{StorePath: "pkg-a", UpstreamURL: "https://fast.example.com", LatencyEMA: 10, TTL: time.Now().Add(time.Hour)},
	})

	entry := store.Get("pkg-a")
	if entry == nil {
		t.Fatal("entry is nil")
	}
	if entry.UpstreamURL != "https://fast.example.com" {
		t.Errorf("expected fast upstream, got %q", entry.UpstreamURL)
	}
}

func TestMergeNewerTimestampWinsOnTie(t *testing.T) {
	store := mesh.NewRouteStore()
	now := time.Now()
	store.Merge([]cache.RouteEntry{
		{StorePath: "pkg-b", UpstreamURL: "https://a.example.com", LatencyEMA: 50, LastVerified: now.Add(-time.Minute), TTL: time.Now().Add(time.Hour)},
	})
	store.Merge([]cache.RouteEntry{
		{StorePath: "pkg-b", UpstreamURL: "https://b.example.com", LatencyEMA: 50, LastVerified: now, TTL: time.Now().Add(time.Hour)},
	})

	entry := store.Get("pkg-b")
	if entry.UpstreamURL != "https://b.example.com" {
		t.Errorf("expected newer upstream, got %q", entry.UpstreamURL)
	}
}
