// Command gateway is the self-hostable LLM gateway: one OpenAI-compatible
// endpoint in front of many models, with cost accounting applied before any
// request reaches a vendor. See docs/INVARIANTS.md for the architectural rules
// this binary upholds.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/somasays/heave/internal/config"
	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/router"
	"github.com/somasays/heave/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*configPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	providers := buildProviders(cfg, log)

	models := make([]router.ModelConfig, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		models = append(models, router.ModelConfig{
			Alias:           m.ID,
			Provider:        m.Provider,
			Upstream:        m.Upstream,
			Price:           router.Price{InputPerMTok: m.Price.InputPerMTok, OutputPerMTok: m.Price.OutputPerMTok},
			MaxOutputTokens: m.MaxOutputTokens,
			AcceptsSampling: m.AcceptsSampling(),
		})
	}
	rtr := router.New(models, cfg.Routing.DefaultModel)
	led := ledger.New(log)

	clients := make([]controls.Client, 0, len(cfg.Clients))
	for _, c := range cfg.Clients {
		clients = append(clients, controls.Client{
			Name: c.Name, KeySHA256: c.KeySHA256,
			MonthlyBudgetUSD: c.MonthlyBudgetUSD, RateLimitRPM: c.RateLimitRPM,
		})
	}
	guard := controls.New(cfg.Auth.Enabled, clients, nil)
	if !cfg.Auth.Enabled {
		log.Warn("authentication is DISABLED; do not expose this gateway to untrusted callers (set auth.enabled: true)")
	}

	srv := server.New(rtr, providers, led, guard, log, server.Options{
		MaxRequestBytes: cfg.Server.MaxRequestBytes,
		RequestTimeout:  cfg.Server.RequestTimeout,
	})

	// A cancelable base context lets us abort in-flight requests if a graceful
	// drain overruns the shutdown budget.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.Server.RequestTimeout + 30*time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Addr, "models", len(cfg.Models), "providers", len(providers))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			// Drain overran the budget: cancel in-flight work and force close.
			// This is an orderly stop, not a crash — do not exit non-zero.
			log.Warn("graceful drain timed out; forcing close", "err", err)
			cancelBase()
			_ = httpSrv.Close()
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// buildProviders constructs one adapter per configured provider. API keys are
// read from the environment here and passed to adapters; they never live in
// config or code.
func buildProviders(cfg *config.Config, log *slog.Logger) map[string]provider.Provider {
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	for _, p := range cfg.Providers {
		key := p.APIKey()
		if key == "" {
			log.Warn("provider has no API key; requests to it will fail", "provider", p.Name, "env", p.APIKeyEnv)
		}
		switch p.Type {
		case "anthropic":
			providers[p.Name] = provider.NewAnthropic(p.Name, key, p.BaseURL)
		case "openai":
			base := p.BaseURL
			if base == "" {
				base = "https://api.openai.com/v1"
			}
			providers[p.Name] = provider.NewOpenAICompat(p.Name, key, base)
		}
	}
	return providers
}
