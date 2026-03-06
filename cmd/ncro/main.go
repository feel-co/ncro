package main

import (
	"context"
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
			}
		}
	}()

	p := prober.New(cfg.Cache.LatencyAlpha)
	p.InitUpstreams(cfg.Upstreams)
	for _, u := range cfg.Upstreams {
		go p.ProbeUpstream(u.URL)
	}

	probeDone := make(chan struct{})
	go p.RunProbeLoop(30*time.Second, probeDone)

	if cfg.Mesh.Enabled {
		node, err := mesh.NewNode(cfg.Mesh.PrivateKeyPath, nil)
		if err != nil {
			slog.Error("failed to create mesh node", "error", err)
			os.Exit(1)
		}
		slog.Info("mesh enabled", "node_id", node.ID(), "peers", len(cfg.Mesh.Peers))
		slog.Warn("mesh gossip not yet implemented")
	}

	r := router.New(db, p, cfg.Cache.TTL.Duration, 5*time.Second)
	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      server.New(r, p, cfg.Upstreams),
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}
