// Command synthusersd is the ready-made daemon of the synthusers library:
// it loads targets from a YAML config, runs the synthetic users on a
// schedule and serves a dashboard (UI + JSON API) on one port.
//
// Usage:
//
//	synthusersd -config config.yaml
//
// Environment overrides: SYNTHUSERS_ADDR, SYNTHUSERS_DSN, SYNTHUSERS_SHARD,
// SYNTHUSERS_SHARDS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"synthusers"
	"synthusers/checkers"
	"synthusers/explorer"
	"synthusers/httpui"
	"synthusers/journey"
	"synthusers/jserror"
	"synthusers/storepg"
)

// Config is the YAML schema of the daemon.
type Config struct {
	Server struct {
		Addr string `yaml:"addr"`
	} `yaml:"server"`

	Store struct {
		Backend string `yaml:"backend"` // memory | postgres
		DSN     string `yaml:"dsn"`
	} `yaml:"store"`

	Engine struct {
		Interval      synthusers.Duration `yaml:"interval"`
		Workers       int                 `yaml:"workers"`
		ShardIndex    int                 `yaml:"shard"`
		ShardCount    int                 `yaml:"shards"`
		RateLimit     float64             `yaml:"rate_limit"`
		Timeout       synthusers.Duration `yaml:"timeout"`
		RunTimeout    synthusers.Duration `yaml:"run_timeout"`
		DisableDelays bool                `yaml:"disable_delays"` // tests/benchmarks only
	} `yaml:"engine"`

	Explorer struct {
		MaxPages int `yaml:"max_pages"`
		MaxDepth int `yaml:"max_depth"`
	} `yaml:"explorer"`

	Checks struct {
		SlowMS      int `yaml:"slow_ms"`
		SSLWarnDays int `yaml:"ssl_warn_days"`
		MaxAssets   int `yaml:"max_assets"`
		MaxLinks    int `yaml:"max_links"`
	} `yaml:"checks"`

	JS struct {
		Enabled       bool                `yaml:"enabled"`
		RemoteURL     string              `yaml:"remote_url"` // e.g. http://chrome:9222 (headless-shell sidecar)
		ExecPath      string              `yaml:"exec_path"`  // local chrome binary; empty = search PATH
		Timeout       synthusers.Duration `yaml:"timeout"`
		SampleDefault int                 `yaml:"sample_default"` // for targets without js.sample
	} `yaml:"js"`

	Personas []synthusers.Persona `yaml:"personas"`
	Targets  []synthusers.Target  `yaml:"targets"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML config")
	flag.Parse()
	if err := run(*configPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Store.
	var store synthusers.Store
	switch cfg.Store.Backend {
	case "", "memory":
		store = synthusers.NewMemoryStore()
		logger.Info("store: in-memory (history resets on restart)")
	case "postgres":
		dsn := os.Getenv("SYNTHUSERS_DSN")
		if dsn == "" {
			dsn = cfg.Store.DSN
		}
		if dsn == "" {
			return errors.New("store.backend=postgres but no dsn (store.dsn or SYNTHUSERS_DSN)")
		}
		pg, err := storepg.New(ctx, dsn)
		if err != nil {
			return err
		}
		store = pg
		logger.Info("store: postgres")
	default:
		return fmt.Errorf("unknown store.backend %q", cfg.Store.Backend)
	}

	// Engine.
	eng := synthusers.New(synthusers.Config{
		Interval:      cfg.Engine.Interval.Std(),
		Workers:       cfg.Engine.Workers,
		ShardIndex:    cfg.Engine.ShardIndex,
		ShardCount:    cfg.Engine.ShardCount,
		RateLimit:     cfg.Engine.RateLimit,
		Timeout:       cfg.Engine.Timeout.Std(),
		RunTimeout:    cfg.Engine.RunTimeout.Std(),
		DisableDelays: cfg.Engine.DisableDelays,
		Personas:      cfg.Personas,
		Logger:        logger,
	}, store)

	// Sharding via env (container-friendly).
	if v := os.Getenv("SYNTHUSERS_SHARD"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.Engine.ShardIndex)
	}
	if v := os.Getenv("SYNTHUSERS_SHARDS"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.Engine.ShardCount)
	}
	if cfg.Engine.ShardCount > 1 {
		logger.Info("sharding", "shard", cfg.Engine.ShardIndex, "of", cfg.Engine.ShardCount)
	}

	// Checkers.
	eng.Use(checkers.HTTP(checkers.HTTPOptions{
		SlowThreshold: time.Duration(cfg.Checks.SlowMS) * time.Millisecond,
		SSLWarnDays:   cfg.Checks.SSLWarnDays,
	}))
	eng.Use(checkers.Assets(checkers.AssetsOptions{
		MaxAssets: cfg.Checks.MaxAssets,
		MaxLinks:  cfg.Checks.MaxLinks,
	}))
	eng.Use(checkers.Content())

	// Runners.
	eng.SetRunner("explorer", explorer.New(explorer.Options{
		MaxPages: cfg.Explorer.MaxPages,
		MaxDepth: cfg.Explorer.MaxDepth,
	}))
	eng.SetRunner("journey", journey.New())

	// Optional real-browser JS checks.
	var jsChecker *jserror.Checker
	if cfg.JS.Enabled {
		jsChecker = jserror.New(jserror.Options{
			RemoteURL: cfg.JS.RemoteURL,
			ExecPath:  cfg.JS.ExecPath,
			Timeout:   cfg.JS.Timeout.Std(),
			Logger:    logger,
		})
		eng.UseJS(jsChecker)
		for i := range cfg.Targets {
			if cfg.Targets[i].JS.Sample == 0 && cfg.JS.SampleDefault > 0 {
				cfg.Targets[i].JS.Sample = cfg.JS.SampleDefault
			}
		}
	} else {
		logger.Info("js checks disabled (set js.enabled: true to turn them on)")
	}

	// Targets from config.
	for _, t := range cfg.Targets {
		t.Enabled = true
		if err := eng.AddTarget(t); err != nil {
			logger.Error("skipping target", "id", t.ID, "err", err)
			continue
		}
		logger.Info("target attached", "id", t.ID, "url", t.StartURL)
	}

	eng.Start(ctx)

	// Dashboard.
	addr := os.Getenv("SYNTHUSERS_ADDR")
	if addr == "" {
		addr = cfg.Server.Addr
	}
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpui.Handler(eng),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	logger.Info("dashboard listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if jsChecker != nil {
		jsChecker.Close()
	}
	logger.Info("synthusersd stopped")
	return nil
}
