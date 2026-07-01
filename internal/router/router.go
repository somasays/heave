// Package router resolves a client-facing model alias into a concrete routing
// decision: which provider to call, which upstream model id to send, the price
// used for cost accounting, and per-model policy (max output, sampling support).
//
// Phase 0 routing is static (alias -> configured model). The project's wedge is
// cache-aware routing (docs/INVARIANTS.md, Invariant #3): once implemented, a
// conversation stays on its warm-cache model and only becomes eligible to
// re-route when the prefix cache TTL lapses. That logic slots in behind this
// same Route call so callers do not change.
package router

import "fmt"

// Price is the per-million-token cost used to compute spend, in USD.
type Price struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Decision is the resolved routing target for a request.
type Decision struct {
	// Provider is the adapter name to dispatch to.
	Provider string
	// Upstream is the vendor model id to send.
	Upstream string
	// Alias is the client-facing model name, echoed back in the response.
	Alias string
	Price Price
	// MaxOutputTokens is the default max_tokens when the client omits it (0
	// means "let the adapter choose its own default").
	MaxOutputTokens int
	// AcceptsSampling is false for models that reject temperature/top_p (e.g.
	// Claude Opus 4.8 / Sonnet 5). The server strips those params when false so
	// a common client setting does not turn into an upstream 400.
	AcceptsSampling bool
}

// ModelConfig is one routable model, sourced from config.
type ModelConfig struct {
	Alias           string
	Provider        string
	Upstream        string
	Price           Price
	MaxOutputTokens int
	AcceptsSampling bool
}

// Router maps aliases to routing decisions.
type Router struct {
	models       map[string]ModelConfig
	defaultAlias string
}

// New builds a Router from the configured models and an optional default alias.
func New(models []ModelConfig, defaultAlias string) *Router {
	m := make(map[string]ModelConfig, len(models))
	for _, mc := range models {
		m[mc.Alias] = mc
	}
	return &Router{models: m, defaultAlias: defaultAlias}
}

// Route resolves the requested alias. An empty alias falls back to the
// configured default. Unknown aliases are an error surfaced to the caller.
func (r *Router) Route(alias string) (Decision, error) {
	if alias == "" {
		alias = r.defaultAlias
	}
	mc, ok := r.models[alias]
	if !ok {
		return Decision{}, fmt.Errorf("unknown model %q", alias)
	}
	return Decision{
		Provider:        mc.Provider,
		Upstream:        mc.Upstream,
		Alias:           mc.Alias,
		Price:           mc.Price,
		MaxOutputTokens: mc.MaxOutputTokens,
		AcceptsSampling: mc.AcceptsSampling,
	}, nil
}
