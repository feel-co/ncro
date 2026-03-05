package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a wrapper around time.Duration that supports YAML unmarshaling
// from Go duration strings (e.g., "30s", "1h"). yaml.v3 cannot unmarshal
// duration strings directly into time.Duration (int64), so we handle it here.
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
	PublicKey string `yaml:"public_key"`
}

type ServerConfig struct {
	Listen       string   `yaml:"listen"`
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
}

type CacheConfig struct {
	DBPath       string  `yaml:"db_path"`
	MaxEntries   int     `yaml:"max_entries"`
	TTL          Duration `yaml:"ttl"`
	LatencyAlpha float64 `yaml:"latency_alpha"`
}

type MeshConfig struct {
	Enabled        bool     `yaml:"enabled"`
	BindAddr       string   `yaml:"bind_addr"`
	Peers          []string `yaml:"peers"`
	PrivateKeyPath string   `yaml:"private_key"`
	GossipInterval Duration `yaml:"gossip_interval"`
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

// Load loads config from file (if non-empty) and applies env overrides.
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
