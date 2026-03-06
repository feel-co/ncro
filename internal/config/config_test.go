package config_test

import (
	"os"
	"testing"
	"time"

	"notashelf.dev/ncro/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Errorf("default listen = %q, want :8080", cfg.Server.Listen)
	}
	if len(cfg.Upstreams) == 0 {
		t.Error("expected at least one default upstream")
	}
	if cfg.Cache.MaxEntries != 100000 {
		t.Errorf("default max_entries = %d, want 100000", cfg.Cache.MaxEntries)
	}
}

func TestLoadFromYAML(t *testing.T) {
	yamlContent := `
server:
  listen: ":9090"
upstreams:
  - url: "https://cache.nixos.org"
    priority: 10
cache:
  db_path: "/tmp/test.db"
  max_entries: 500
`
	f, _ := os.CreateTemp("", "ncro-*.yaml")
	defer os.Remove(f.Name())
	f.WriteString(yamlContent)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Errorf("listen = %q, want :9090", cfg.Server.Listen)
	}
	if cfg.Cache.MaxEntries != 500 {
		t.Errorf("max_entries = %d, want 500", cfg.Cache.MaxEntries)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("NCRO_LISTEN", ":1234")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Server.Listen != ":1234" {
		t.Errorf("env override listen = %q, want :1234", cfg.Server.Listen)
	}
}

func TestDurationParsing(t *testing.T) {
	yamlContent := `
server:
  listen: ":8080"
  read_timeout: 30s
  write_timeout: 1m
cache:
  ttl: 2h
mesh:
  gossip_interval: 45s
`
	f, _ := os.CreateTemp("", "ncro-dur-*.yaml")
	defer os.Remove(f.Name())
	f.WriteString(yamlContent)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Server.ReadTimeout.Duration != 30*time.Second {
		t.Errorf("read_timeout = %v, want 30s", cfg.Server.ReadTimeout.Duration)
	}
	if cfg.Server.WriteTimeout.Duration != time.Minute {
		t.Errorf("write_timeout = %v, want 1m", cfg.Server.WriteTimeout.Duration)
	}
	if cfg.Cache.TTL.Duration != 2*time.Hour {
		t.Errorf("ttl = %v, want 2h", cfg.Cache.TTL.Duration)
	}
	if cfg.Mesh.GossipInterval.Duration != 45*time.Second {
		t.Errorf("gossip_interval = %v, want 45s", cfg.Mesh.GossipInterval.Duration)
	}
}

func TestValidateValid(t *testing.T) {
	cfg, _ := config.Load("")
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config should be valid: %v", err)
	}
}

func TestValidateNoUpstreams(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Upstreams = nil
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for no upstreams")
	}
}

func TestValidateBadURL(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Upstreams = []config.UpstreamConfig{{URL: "not-a-url"}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestValidateBadAlpha(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Cache.LatencyAlpha = 0
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for alpha=0")
	}
	cfg.Cache.LatencyAlpha = 1
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for alpha=1")
	}
}

func TestValidateZeroTTL(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Cache.TTL = config.Duration{}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for zero TTL")
	}
}

func TestValidateNegativeTTL(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Cache.NegativeTTL = config.Duration{}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for zero negative_ttl")
	}
}

func TestValidateMeshEnabledNoPeers(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Mesh.Enabled = true
	cfg.Mesh.Peers = nil
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for mesh enabled without peers")
	}
}

func TestValidateMeshBadPeerKey(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Mesh.Enabled = true
	cfg.Mesh.Peers = []config.PeerConfig{
		{Addr: "127.0.0.1:7946", PublicKey: "not-hex!"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid mesh peer public key")
	}
}

func TestValidateUpstreamBadPublicKey(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Upstreams = []config.UpstreamConfig{
		{URL: "https://cache.nixos.org", PublicKey: "no-colon-here"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for upstream public_key missing ':'")
	}
}

func TestInvalidDuration(t *testing.T) {
	yamlContent := `
server:
  read_timeout: "bananas"
`
	f, _ := os.CreateTemp("", "ncro-bad-*.yaml")
	defer os.Remove(f.Name())
	f.WriteString(yamlContent)
	f.Close()

	_, err := config.Load(f.Name())
	if err == nil {
		t.Error("expected error for invalid duration string, got nil")
	}
}
