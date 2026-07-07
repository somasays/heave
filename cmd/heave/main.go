// Command gateway is the self-hostable LLM gateway: one OpenAI-compatible
// endpoint in front of many models, with cost accounting applied before any
// request reaches a vendor. See docs/INVARIANTS.md for the architectural rules
// this binary upholds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/somasays/heave/internal/broker"
	"github.com/somasays/heave/internal/config"
	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/enforcer"
	"github.com/somasays/heave/internal/firewall"
	"github.com/somasays/heave/internal/health"
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/pgledger"
	"github.com/somasays/heave/internal/policy"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/redact"
	"github.com/somasays/heave/internal/redisstore"
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
			Fallbacks:       m.Fallbacks,
		})
	}
	rtr := router.New(models, cfg.Routing.DefaultModel)
	led := ledger.New(log)
	var ledgerReader server.LedgerReader
	if cfg.Ledger.DatabaseURLEnv != "" {
		dbURL := os.Getenv(cfg.Ledger.DatabaseURLEnv)
		if dbURL == "" {
			log.Warn("ledger.database_url_env is set but the env var is empty; durable ledger OFF", "env", cfg.Ledger.DatabaseURLEnv)
		} else {
			if !strings.Contains(dbURL, "sslmode=require") && !strings.Contains(dbURL, "sslmode=verify") {
				// pgx defaults to sslmode=prefer, which silently falls back to
				// PLAINTEXT — the durable store holds client attribution + costs.
				log.Warn("durable ledger DSN has no sslmode=require; a remote Postgres connection may transmit in plaintext — set sslmode=require")
			}
			pgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			sink, err := pgledger.New(pgCtx, dbURL)
			cancel()
			if err != nil {
				// Don't wrap the raw error — it can echo the connection string
				// (with the password). Surface only that durable persistence failed.
				return fmt.Errorf("durable ledger: could not connect to the configured database")
			}
			defer func() { _ = sink.Close() }()
			led.WithSink(sink)
			ledgerReader = sink // the sink also serves durable reads (/v1/spend)
			log.Info("durable spend ledger enabled (postgres)")
		}
	}

	clients := make([]controls.Client, 0, len(cfg.Clients))
	for _, c := range cfg.Clients {
		clients = append(clients, controls.Client{
			Name: c.Name, KeySHA256: c.KeySHA256,
			MonthlyBudgetUSD: c.MonthlyBudgetUSD, RateLimitRPM: c.RateLimitRPM, Admin: c.Admin,
		})
	}
	guard := controls.New(cfg.Auth.Enabled, clients, nil)
	if !cfg.Auth.Enabled {
		log.Warn("authentication is DISABLED; do not expose this gateway to untrusted callers (set auth.enabled: true)")
	}

	tracker := health.New(cfg.Failover.ConsecutiveFailures, cfg.Failover.Cooldown, nil)
	redactor := redact.New(cfg.Redaction.Enabled, cfg.Redaction.CustomPatterns)
	if redactor.Enabled() {
		log.Info("PII redaction is enabled (regex-based, best-effort)")
	}
	// Resolve the kill TTL once so the local map and the Redis key expiry agree
	// (single source of truth). A zero TTL passed to Redis means "never expire".
	killTTL := cfg.Firewall.KillTTL
	if killTTL <= 0 {
		killTTL = firewall.DefaultKillTTL
	}
	fw := firewall.New(cfg.Firewall.Enabled, firewall.Limits{
		MaxUSDPerMin:    cfg.Firewall.MaxUSDPerMin,
		MaxTokensPerMin: cfg.Firewall.MaxTokensPerMin,
		MaxConcurrent:   cfg.Firewall.MaxConcurrent,
		LoopThreshold:   cfg.Firewall.LoopThreshold,
		MaxUSDPerRun:    cfg.Firewall.MaxUSDPerRun,
		KillTTL:         killTTL,
	}, nil)
	var sharedStore *redisstore.Store
	if cfg.Firewall.RedisURL != "" {
		store, err := redisstore.New(cfg.Firewall.RedisURL, killTTL)
		if err != nil {
			return fmt.Errorf("firewall redis: %w", err)
		}
		defer func() { _ = store.Close() }()
		// Keep a concurrency hold alive longer than the longest request so a live
		// hold is never reaped as a crash leak (over-admitting the concurrency cap).
		store.SetHoldTTL(int(cfg.Server.RequestTimeout.Seconds()) + 60)
		pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = store.Ping(pingCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("firewall redis unreachable: %w", err)
		}
		fw.WithKillStore(store)
		fw.WithScopeStore(store)
		sharedStore = store
		log.Info("firewall kill state + velocity/concurrency shared via redis")
	}

	// Provider-quota broker (Invariant #9, ADR 0003): active only with a shared
	// store, since a provider limit is global and per-instance brokering would be
	// N× the real ceiling.
	provLimits := make(map[string]broker.Limit, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p.RateLimitRPM > 0 || p.RateLimitTPM > 0 {
			provLimits[p.Name] = broker.Limit{RPM: p.RateLimitRPM, TPM: p.RateLimitTPM}
		}
	}
	var qb *broker.Broker
	if sharedStore != nil && len(provLimits) > 0 {
		qb = broker.New(sharedStore, provLimits)
		log.Info("provider-quota brokering enabled", "providers", len(provLimits))
	} else {
		qb = broker.New(nil, nil) // inert
		if len(provLimits) > 0 {
			log.Warn("provider rate limits are configured but brokering is OFF (needs firewall.redis_url); relying on failover-after-429")
		}
	}
	if fw.Enabled() {
		log.Info("spend/quota firewall enabled",
			"usd_per_min", cfg.Firewall.MaxUSDPerMin, "max_concurrent", cfg.Firewall.MaxConcurrent,
			"loop_threshold", cfg.Firewall.LoopThreshold, "shared_kill", cfg.Firewall.RedisURL != "")
		if !cfg.Auth.Enabled {
			// With auth off the firewall scope owner collapses to "" for everyone:
			// run scoping is cross-caller, and the kill endpoint is unauthenticated.
			// The firewall's per-run isolation is effectively OFF — dev use only.
			log.Warn("firewall is enabled but auth is DISABLED: run scoping collapses across callers and the kill endpoint is unauthenticated; enable auth for real isolation")
		}
	}

	// Org control plane (ADR 0006): when enabled, provision an empty in-memory
	// policy store; operators populate it via the admin-gated management API. nil
	// when off ⇒ the management routes are not mounted and enforcement is flat.
	var polStore *policy.Store
	var chainResolver server.ChainResolver
	var guardSecret []byte
	var guardDedup server.GuardDedup
	if cfg.ControlPlane.Enabled {
		polStore = policy.New()
		chainResolver = enforcer.NewResolver(polStore) // resolves + enforces per-scope on the request path
		log.Info("org control plane enabled (management API mounted at /v1/policy; store is in-memory, not durable across restarts)")
		if !cfg.Auth.Enabled {
			log.Warn("control plane is enabled but auth is DISABLED: the management API is unauthenticated and no key maps to a policy node; enable auth (with an admin key) before exposing it")
		}
		if env := cfg.ControlPlane.GuardSecretEnv; env != "" {
			secret := []byte(os.Getenv(env))
			switch {
			case len(secret) < 32:
				log.Warn("control_plane.guard_secret_env is set but the secret is missing/too short (<32 bytes); the /v1/guard decision API is OFF", "env", env)
			case sharedStore == nil:
				// The decision API's cross-replica idempotency + orphaned-hold reaping
				// both depend on the shared store; without it, it silently under-enforces.
				log.Warn("control_plane.guard_secret_env is set but the /v1/guard decision API requires the shared store (set firewall.redis_url) for cross-replica idempotency and orphaned-hold reaping; guard API is OFF")
			default:
				guardSecret = secret
				guardDedup = sharedStore // redisstore satisfies server.GuardDedup (SET NX)
				log.Info("OOB decision API enabled (/v1/guard/reserve|settle|release; admin-gated, signed reservation tokens, redis-backed)")
			}
		}
	}

	srv := server.New(server.Deps{
		Router: rtr, Providers: providers, Ledger: led, Guard: guard,
		Health: tracker, Redactor: redactor, Firewall: fw, Broker: qb,
		Policy: polStore, Resolver: chainResolver,
		GuardSecret: guardSecret, GuardDedup: guardDedup,
		LedgerReader: ledgerReader, Log: log,
	}, server.Options{
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
