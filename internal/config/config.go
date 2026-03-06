package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Wrapper around time.Duration supporting YAML duration strings ("30s", "1h").
// yaml.v3 cannot unmarshal duration strings directly into time.Duration (int64).
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		// Try decoding as a raw int64 (nanoseconds) as fallback.
		var ns int64
		if err2 := value.Decode(&ns); err2 != nil {
			return fmt.Errorf("cannot unmarshal duration (tried string: %v): %w", err, err2)
		}
		d.Duration = time.Duration(ns)
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

type UpstreamConfig struct {
	URL       string `yaml:"url"`
	Priority  int    `yaml:"priority"`
	PublicKey string `yaml:"public_key"` // Nix signing key "name:base64(key)"
}

type ServerConfig struct {
	Listen       string   `yaml:"listen"`
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
}

type CacheConfig struct {
	DBPath       string   `yaml:"db_path"`
	MaxEntries   int      `yaml:"max_entries"`
	TTL          Duration `yaml:"ttl"`
	LatencyAlpha float64  `yaml:"latency_alpha"`
}

// Mesh peer with its ed25519 public key for gossip message verification.
type PeerConfig struct {
	Addr      string `yaml:"addr"`
	PublicKey string `yaml:"public_key"` // hex-encoded ed25519 public key (32 bytes)
}

type MeshConfig struct {
	Enabled        bool         `yaml:"enabled"`
	BindAddr       string       `yaml:"bind_addr"`
	Peers          []PeerConfig `yaml:"peers"`
	PrivateKeyPath string       `yaml:"private_key"`
	GossipInterval Duration     `yaml:"gossip_interval"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Upstreams []UpstreamConfig `yaml:"upstreams"`
	Cache     CacheConfig      `yaml:"cache"`
	Mesh      MeshConfig       `yaml:"mesh"`
	Logging   LoggingConfig    `yaml:"logging"`
}

func defaults() Config {
	return Config{
		Server: ServerConfig{
			Listen:       ":8080",
			ReadTimeout:  Duration{30 * time.Second},
			WriteTimeout: Duration{30 * time.Second},
		},
		Upstreams: []UpstreamConfig{
			{URL: "https://cache.nixos.org", Priority: 10},
		},
		Cache: CacheConfig{
			DBPath:       "/var/lib/ncro/routes.db",
			MaxEntries:   100000,
			TTL:          Duration{time.Hour},
			LatencyAlpha: 0.3,
		},
		Mesh: MeshConfig{
			BindAddr:       "0.0.0.0:7946",
			GossipInterval: Duration{30 * time.Second},
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

// Validates config fields. Call after Load.
func (c *Config) Validate() error {
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream is required")
	}
	for i, u := range c.Upstreams {
		if u.URL == "" {
			return fmt.Errorf("upstream[%d]: URL is empty", i)
		}
		if _, err := url.ParseRequestURI(u.URL); err != nil {
			return fmt.Errorf("upstream[%d]: invalid URL %q: %w", i, u.URL, err)
		}
		if u.PublicKey != "" && !strings.Contains(u.PublicKey, ":") {
			return fmt.Errorf("upstream[%d]: public_key must be in 'name:base64(key)' Nix format", i)
		}
	}
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen is empty")
	}
	if c.Cache.LatencyAlpha <= 0 || c.Cache.LatencyAlpha >= 1 {
		return fmt.Errorf("cache.latency_alpha must be between 0 and 1 exclusive, got %f", c.Cache.LatencyAlpha)
	}
	if c.Cache.TTL.Duration <= 0 {
		return fmt.Errorf("cache.ttl must be positive")
	}
	if c.Cache.MaxEntries <= 0 {
		return fmt.Errorf("cache.max_entries must be positive")
	}
	if c.Mesh.Enabled && len(c.Mesh.Peers) == 0 {
		return fmt.Errorf("mesh.enabled is true but no peers configured")
	}
	for i, peer := range c.Mesh.Peers {
		if peer.Addr == "" {
			return fmt.Errorf("mesh.peers[%d]: addr is empty", i)
		}
		if peer.PublicKey != "" {
			b, err := hex.DecodeString(peer.PublicKey)
			if err != nil || len(b) != 32 {
				return fmt.Errorf("mesh.peers[%d]: public_key must be a hex-encoded 32-byte ed25519 key", i)
			}
		}
	}
	return nil
}

// Loads config from file (if non-empty) and applies env overrides.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, err
		}
	}

	// Env overrides
	if v := os.Getenv("NCRO_LISTEN"); v != "" {
		cfg.Server.Listen = v
	}
	if v := os.Getenv("NCRO_DB_PATH"); v != "" {
		cfg.Cache.DBPath = v
	}
	if v := os.Getenv("NCRO_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}

	return &cfg, nil
}
