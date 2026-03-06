package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"notashelf.dev/ncro/internal/cache"
	"notashelf.dev/ncro/internal/config"
	"notashelf.dev/ncro/internal/mesh"
	"notashelf.dev/ncro/internal/metrics"
	"notashelf.dev/ncro/internal/prober"
	"notashelf.dev/ncro/internal/router"
	"notashelf.dev/ncro/internal/server"
)

func main() {
	configPath := flag.String("config", "", "path to config YAML file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}

	level := slog.LevelInfo
	switch cfg.Logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	var handler slog.Handler
	if cfg.Logging.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(handler))

	metrics.Register(prometheus.DefaultRegisterer)

	db, err := cache.Open(cfg.Cache.DBPath, cfg.Cache.MaxEntries)
	if err != nil {
		slog.Error("failed to open database", "path", cfg.Cache.DBPath, "error", err)
		os.Exit(1)
	}
	defer db.Close()

	expireDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-expireDone:
				return
			case <-ticker.C:
				if err := db.ExpireOldRoutes(); err != nil {
					slog.Warn("expire routes error", "error", err)
				}
				if err := db.ExpireNegatives(); err != nil {
					slog.Warn("expire negatives error", "error", err)
				}
				if count, err := db.RouteCount(); err == nil {
					metrics.RouteEntries.Set(float64(count))
				}
			}
		}
	}()

	p := prober.New(cfg.Cache.LatencyAlpha)
	p.InitUpstreams(cfg.Upstreams)

	// Seed prober with persisted health data from the previous run.
	if rows, err := db.LoadAllHealth(); err == nil {
		for _, row := range rows {
			p.Seed(row.URL, row.EMALatency, row.ConsecutiveFails, int64(row.TotalQueries))
		}
	} else {
		slog.Warn("failed to load persisted health data", "error", err)
	}

	// Persist health updates to SQLite.
	p.SetHealthPersistence(func(url string, ema float64, cf uint32, tq uint64) {
		if err := db.SaveHealth(url, ema, int(cf), int64(tq)); err != nil {
			slog.Warn("failed to save health", "url", url, "error", err)
		}
	})

	for _, u := range cfg.Upstreams {
		go p.ProbeUpstream(u.URL)
	}

	probeDone := make(chan struct{})
	go p.RunProbeLoop(30*time.Second, probeDone)

	r := router.New(db, p, cfg.Cache.TTL.Duration, 5*time.Second, cfg.Cache.NegativeTTL.Duration)
	for _, u := range cfg.Upstreams {
		if u.PublicKey != "" {
			if err := r.SetUpstreamKey(u.URL, u.PublicKey); err != nil {
				slog.Error("invalid upstream public key", "url", u.URL, "error", err)
				os.Exit(1)
			}
			slog.Info("narinfo signature verification enabled", "upstream", u.URL)
		}
	}

	var gossipDone chan struct{}
	if cfg.Mesh.Enabled {
		store := mesh.NewRouteStore()
		node, err := mesh.NewNode(cfg.Mesh.PrivateKeyPath, store)
		if err != nil {
			slog.Error("failed to create mesh node", "error", err)
			os.Exit(1)
		}
		slog.Info("mesh node identity", "node_id", node.ID(),
			"public_key", hex.EncodeToString(node.PublicKey()))

		allowedKeys := make([]ed25519.PublicKey, 0, len(cfg.Mesh.Peers))
		for _, peer := range cfg.Mesh.Peers {
			if peer.PublicKey != "" {
				b, _ := hex.DecodeString(peer.PublicKey)
				allowedKeys = append(allowedKeys, ed25519.PublicKey(b))
			}
		}

		if err := mesh.ListenAndServe(cfg.Mesh.BindAddr, store, allowedKeys...); err != nil {
			slog.Error("failed to start mesh listener", "addr", cfg.Mesh.BindAddr, "error", err)
			os.Exit(1)
		}

		peerAddrs := make([]string, len(cfg.Mesh.Peers))
		for i, p := range cfg.Mesh.Peers {
			peerAddrs[i] = p.Addr
		}

		gossipDone = make(chan struct{})
		go mesh.RunGossipLoop(node, db, peerAddrs, cfg.Mesh.GossipInterval.Duration, gossipDone)
		slog.Info("mesh enabled", "addr", cfg.Mesh.BindAddr, "peers", len(cfg.Mesh.Peers))
	}

	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      server.New(r, p, db, cfg.Upstreams, cfg.Server.CachePriority),
		ReadTimeout:  cfg.Server.ReadTimeout.Duration,
		WriteTimeout: cfg.Server.WriteTimeout.Duration,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("ncro listening", "addr", cfg.Server.Listen, "upstreams", len(cfg.Upstreams))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-stop
	slog.Info("shutting down")

	close(expireDone)
	close(probeDone)
	if gossipDone != nil {
		close(gossipDone)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}
