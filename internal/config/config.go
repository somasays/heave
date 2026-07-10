// Package config loads the declarative gateway configuration. Prices and model
// routing are data, not code (docs/INVARIANTS.md, Invariant #6). Secrets are
// never in the file or in code: providers name an environment variable that
// holds the API key (Invariant #4).
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole gateway configuration.
type Config struct {
	Server       Server       `yaml:"server"`
	Providers    []Provider   `yaml:"providers"`
	Models       []Model      `yaml:"models"`
	Routing      Routing      `yaml:"routing"`
	Auth         Auth         `yaml:"auth"`
	Clients      []Client     `yaml:"clients"`
	Failover     Failover     `yaml:"failover"`
	Redaction    Redaction    `yaml:"redaction"`
	Firewall     Firewall     `yaml:"firewall"`
	Ledger       Ledger       `yaml:"ledger"`
	ControlPlane ControlPlane `yaml:"control_plane"`
}

// ControlPlane toggles the org control plane (ADR 0005/0006): the hierarchical
// budget model + admin-gated management API. Off by default; when on, the store
// starts empty and is provisioned via the management API (POST /v1/policy/...).
// The in-memory store is not durable across restarts yet — a later increment adds
// Postgres persistence.
type ControlPlane struct {
	Enabled bool `yaml:"enabled"`
	// GuardSecretEnv names the env var holding the HMAC secret (>=32 bytes) that
	// signs /v1/guard reservation tokens (ADR 0007). Like all secrets it comes from
	// the ENVIRONMENT, never the file (Invariant #4). Empty ⇒ the OOB decision API
	// is OFF (only the inline path + management API are available).
	GuardSecretEnv string `yaml:"guard_secret_env"`
	// Console configures the admin console's login (SSO + local accounts).
	Console Console `yaml:"console"`
}

// Console configures the admin console authentication. Empty/disabled ⇒ the console
// login endpoints are not mounted (the management + guard APIs still work with an
// admin bearer key). Secrets (session HMAC key, OAuth client secrets) come from the
// ENVIRONMENT (Invariant #4); the file holds only non-secret ids/hashes.
type Console struct {
	Enabled bool `yaml:"enabled"`
	// SessionSecretEnv names the env var holding the session-cookie HMAC key (>=32B).
	SessionSecretEnv string        `yaml:"session_secret_env"`
	SessionTTL       time.Duration `yaml:"session_ttl"`
	// BaseURL is the console's externally-reachable base (e.g. https://gw.acme.com),
	// used to build OAuth redirect URIs. Required for SSO.
	BaseURL string `yaml:"base_url"`
	// AllowInsecure emits session cookies without the Secure flag — LOCAL HTTP DEV
	// ONLY. Leave false in production.
	AllowInsecure bool `yaml:"allow_insecure"`
	// AdminEmails / AdminDomains authorize SSO identities as admin.
	AdminEmails  []string `yaml:"admin_emails"`
	AdminDomains []string `yaml:"admin_domains"`
	// Accounts are local (username/password) operators. PasswordHash is a PBKDF2
	// string (not the plaintext); generate it out-of-band.
	Accounts []ConsoleAccount `yaml:"accounts"`
	// Google / GitHub OAuth apps. ClientID is public; the secret is from env.
	Google OAuthApp `yaml:"google"`
	GitHub OAuthApp `yaml:"github"`
}

// ConsoleAccount is one local operator login.
type ConsoleAccount struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
	Admin        bool   `yaml:"admin"`
}

// OAuthApp is one SSO provider's app credentials. Empty ClientID ⇒ that provider
// is not offered.
type OAuthApp struct {
	ClientID        string `yaml:"client_id"`
	ClientSecretEnv string `yaml:"client_secret_env"`
}

// Firewall configures the runtime spend & quota firewall (Invariant #9): hard,
// pre-vendor enforcement for agentic traffic. All limits are per-instance for
// now; 0 disables a given limit.
type Firewall struct {
	Enabled         bool    `yaml:"enabled"`
	MaxUSDPerMin    float64 `yaml:"max_usd_per_min"`
	MaxTokensPerMin int     `yaml:"max_tokens_per_min"`
	MaxConcurrent   int     `yaml:"max_concurrent"`
	// LoopThreshold auto-kills a run after it repeats the same prompt-prefix
	// this many times (a runaway agent). 0 disables loop detection.
	LoopThreshold int `yaml:"loop_threshold"`
	// MaxUSDPerRun caps a single run's estimated spend over its ACTIVE lifetime
	// (needs a run id): once a run's spend would exceed it the run is auto-killed.
	// The backstop for runaways whose prompts keep changing (which loop detection
	// cannot catch). Not an absolute cap — it is on the estimate and resets if a
	// run idles out; the per-client monthly_budget_usd is the absolute ceiling.
	// 0 disables it.
	MaxUSDPerRun float64 `yaml:"max_usd_per_run"`
	// RedisURL, when set (redis://host:port/db), shares run-kill state across
	// replicas so a kill on one gateway stops the run on all. Velocity and
	// concurrency remain per-instance for now. Empty = in-memory (single node).
	RedisURL string `yaml:"redis_url"`
	// KillTTL bounds how long a kill is remembered (local map + Redis key TTL).
	// Long enough that a long-lived run stays dead; <= 0 uses the built-in
	// default (24h).
	KillTTL time.Duration `yaml:"kill_ttl"`
}

// Failover tunes the per-provider circuit breaker used during model failover.
type Failover struct {
	// ConsecutiveFailures opens a provider's breaker (default 3).
	ConsecutiveFailures int `yaml:"consecutive_failures"`
	// Cooldown is how long an opened breaker stays open (default 30s).
	Cooldown time.Duration `yaml:"cooldown"`
}

// Redaction configures the pre-flight PII/secret scrubber.
type Redaction struct {
	// Enabled turns redaction on (default off — it is lossy).
	Enabled bool `yaml:"enabled"`
	// CustomPatterns maps a rule name to a Go regexp, applied after the
	// built-in detectors (email, SSN, credit card, phone, API keys).
	CustomPatterns map[string]string `yaml:"custom_patterns"`
}

// Auth toggles gateway authentication.
type Auth struct {
	// Enabled gates the auth/rate/budget controls. Defaults to false so local
	// dev is frictionless; a loud warning is logged when disabled. Production
	// deployments MUST set this true (docs/INVARIANTS.md, Invariant #7).
	Enabled bool `yaml:"enabled"`
}

// Client is one authenticated caller and its limits. The plaintext key is never
// stored: operators configure the hex SHA-256 of the bearer token.
type Client struct {
	Name             string  `yaml:"name"`
	KeySHA256        string  `yaml:"key_sha256"`
	MonthlyBudgetUSD float64 `yaml:"monthly_budget_usd"`
	RateLimitRPM     int     `yaml:"rate_limit_rpm"`
	// Admin grants access to the cross-tenant observability endpoints (/v1/stats,
	// /dashboard), which expose every tenant's spend/attribution. Off by default.
	Admin bool `yaml:"admin"`
}

// Server holds listen and hardening configuration.
type Server struct {
	Addr string `yaml:"addr"`
	// RequestTimeout bounds a single upstream call (client + gateway share it).
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// MaxRequestBytes caps the request body to prevent memory-exhaustion DoS.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

// Provider describes one vendor adapter to construct.
type Provider struct {
	Name string `yaml:"name"`
	// Type selects the adapter: "anthropic" or "openai".
	Type string `yaml:"type"`
	// BaseURL optionally overrides the vendor endpoint (also used for
	// OpenAI-compatible vendors like OpenRouter).
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the environment variable holding the API key. The key
	// itself is never written in config.
	APIKeyEnv string `yaml:"api_key_env"`
	// RateLimitRPM / RateLimitTPM are the vendor account's KNOWN shared quota
	// (requests / tokens per minute). When set AND firewall.redis_url is
	// configured, the gateway brokers this quota PRE-vendor across replicas
	// (Invariant #9, ADR 0003): it reserves headroom before dispatch and fails
	// over to another provider — or returns 429 — instead of hitting the vendor's
	// 429. 0 disables a dimension. Ignored without a shared store.
	RateLimitRPM int `yaml:"rate_limit_rpm"`
	RateLimitTPM int `yaml:"rate_limit_tpm"`
}

// Model is one client-facing routable model.
type Model struct {
	// ID is the alias clients request.
	ID string `yaml:"id"`
	// Provider references a Provider.Name.
	Provider string `yaml:"provider"`
	// Upstream is the vendor model id actually sent.
	Upstream string `yaml:"upstream"`
	Price    Price  `yaml:"price"`
	// MaxOutputTokens is the default max_tokens when the client omits it.
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// Sampling reports whether the model accepts temperature/top_p. Pointer so
	// an omitted value defaults to true; set false for models that reject
	// sampling params (e.g. Claude Opus 4.8 / Sonnet 5).
	Sampling *bool `yaml:"sampling"`
	// Fallbacks are other model ids to try, in order, on a retryable failure.
	Fallbacks []string `yaml:"fallbacks"`
}

// AcceptsSampling reports whether the model accepts sampling params (default
// true when unset).
func (m Model) AcceptsSampling() bool { return m.Sampling == nil || *m.Sampling }

// Price is per-million-token cost in USD.
type Price struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
}

// Routing holds routing policy.
type Routing struct {
	DefaultModel string `yaml:"default_model"`
}

// Ledger configures durable spend persistence. Empty = in-memory only.
type Ledger struct {
	// DatabaseURLEnv names the environment variable holding the Postgres
	// connection string (postgres://user:pass@host:port/db). Like provider API
	// keys (Invariant #4), the secret-bearing URL is read from the ENVIRONMENT,
	// never written in the config file. When set (and the env var is non-empty)
	// the durable Postgres ledger is enabled: records are batched asynchronously
	// and dropped-with-a-counter under backpressure so accounting never blocks the
	// request path. Empty = in-memory aggregates + structured log only.
	DatabaseURLEnv string `yaml:"database_url_env"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.RequestTimeout <= 0 {
		c.Server.RequestTimeout = 120 * time.Second
	}
	if c.Server.MaxRequestBytes <= 0 {
		c.Server.MaxRequestBytes = 1 << 20 // 1 MiB
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: no providers defined")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("config: no models defined")
	}
	provNames := make(map[string]bool, len(c.Providers))
	for _, p := range c.Providers {
		if p.Name == "" || p.Type == "" {
			return fmt.Errorf("config: provider missing name or type")
		}
		if p.Type != "anthropic" && p.Type != "openai" {
			return fmt.Errorf("config: provider %q has unknown type %q", p.Name, p.Type)
		}
		if provNames[p.Name] {
			return fmt.Errorf("config: duplicate provider name %q", p.Name)
		}
		if p.RateLimitRPM < 0 || p.RateLimitTPM < 0 {
			return fmt.Errorf("config: provider %q rate limits must be >= 0", p.Name)
		}
		provNames[p.Name] = true
	}
	aliases := make(map[string]bool, len(c.Models))
	for _, m := range c.Models {
		if m.ID == "" {
			return fmt.Errorf("config: model missing id")
		}
		if aliases[m.ID] {
			return fmt.Errorf("config: duplicate model id %q", m.ID)
		}
		aliases[m.ID] = true
		if !provNames[m.Provider] {
			return fmt.Errorf("config: model %q references unknown provider %q", m.ID, m.Provider)
		}
	}
	// Fallbacks must reference defined models and not the model itself.
	for _, m := range c.Models {
		for _, fb := range m.Fallbacks {
			if fb == m.ID {
				return fmt.Errorf("config: model %q lists itself as a fallback", m.ID)
			}
			if !aliases[fb] {
				return fmt.Errorf("config: model %q has unknown fallback %q", m.ID, fb)
			}
		}
	}
	if c.Routing.DefaultModel != "" && !aliases[c.Routing.DefaultModel] {
		return fmt.Errorf("config: default_model %q is not a defined model", c.Routing.DefaultModel)
	}
	if err := c.validateClients(); err != nil {
		return err
	}
	for name, pat := range c.Redaction.CustomPatterns {
		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("config: redaction custom_pattern %q is not a valid regexp: %w", name, err)
		}
	}
	if c.Failover.ConsecutiveFailures < 0 {
		return fmt.Errorf("config: failover.consecutive_failures must be >= 0")
	}
	if c.Firewall.MaxUSDPerMin < 0 || c.Firewall.MaxTokensPerMin < 0 ||
		c.Firewall.MaxConcurrent < 0 || c.Firewall.LoopThreshold < 0 ||
		c.Firewall.MaxUSDPerRun < 0 {
		return fmt.Errorf("config: firewall limits must be >= 0")
	}
	if c.Firewall.KillTTL < 0 {
		return fmt.Errorf("config: firewall.kill_ttl must be >= 0")
	}
	return nil
}

func (c *Config) validateClients() error {
	if c.Auth.Enabled && len(c.Clients) == 0 {
		return fmt.Errorf("config: auth.enabled is true but no clients are defined")
	}
	names := make(map[string]bool, len(c.Clients))
	hashes := make(map[string]bool, len(c.Clients))
	for _, cl := range c.Clients {
		if cl.Name == "" {
			return fmt.Errorf("config: client missing name")
		}
		if names[cl.Name] {
			return fmt.Errorf("config: duplicate client name %q", cl.Name)
		}
		names[cl.Name] = true
		if len(cl.KeySHA256) != 64 {
			return fmt.Errorf("config: client %q key_sha256 must be a 64-char hex SHA-256", cl.Name)
		}
		if _, err := hex.DecodeString(cl.KeySHA256); err != nil {
			return fmt.Errorf("config: client %q key_sha256 is not valid hex", cl.Name)
		}
		if hashes[cl.KeySHA256] {
			return fmt.Errorf("config: duplicate client key_sha256 (client %q)", cl.Name)
		}
		hashes[cl.KeySHA256] = true
		if cl.MonthlyBudgetUSD < 0 || cl.RateLimitRPM < 0 {
			return fmt.Errorf("config: client %q has a negative budget or rate limit", cl.Name)
		}
	}
	return nil
}

// APIKey resolves the provider's API key from its named environment variable.
func (p Provider) APIKey() string {
	if p.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(p.APIKeyEnv)
}
