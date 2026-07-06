// Package enforcer is the adapter that binds the org POLICY model (internal/policy)
// to the runtime FIREWALL (internal/firewall): it resolves a request's key+run to
// a scope chain and translates that chain into the per-scope form the firewall's
// EnterChain enforces. It exists as its own package because policy and firewall
// are each pure (neither may import the other); the adapter is the one place that
// depends on both, keeping the model and the enforcement engine decoupled.
//
// Only the velocity, concurrency, and per-run-$ caps are carried to the firewall —
// the calendar Day/Month budgets in a policy node are NOT enforced here (they need
// the durable ledger; a later increment). Loop detection and kill TTL are
// firewall-global, not per-node, so they are left zero on translated scopes.
package enforcer

import (
	"errors"

	"github.com/somasays/heave/internal/firewall"
	"github.com/somasays/heave/internal/policy"
)

// Translate projects a resolved policy chain onto the firewall's per-scope form.
// Keys and per-node caps carry over verbatim; the fields the firewall does not
// enforce (Day/Month budgets, loop/kill TTL) are dropped.
//
// OPERATOR CONSEQUENCE of dropping Day/Month: a policy node whose ONLY cap is a
// calendar budget (MaxUSDPerDay/Month, nothing per-min/tokens/concurrency/per-run)
// translates to an all-zero firewall.Scope. The firewall reads 0 as "unlimited",
// so that scope enforces NOTHING in real time and its requests pass the pre-vendor
// gate — the durable ledger is the only backstop until calendar enforcement lands
// (a later increment). A node meant to be capped in real time must carry a
// per-min / per-run cap, not only a daily budget.
func Translate(ch policy.Chain) []firewall.Scope {
	scopes := make([]firewall.Scope, len(ch.Scopes))
	for i, sc := range ch.Scopes {
		scopes[i] = firewall.Scope{
			Name: sc.Name,
			Key:  sc.Key,
			Limits: firewall.Limits{
				MaxUSDPerMin:    sc.Limits.MaxUSDPerMin,
				MaxTokensPerMin: sc.Limits.MaxTokensPerMin,
				MaxConcurrent:   sc.Limits.MaxConcurrent,
				MaxUSDPerRun:    sc.Limits.MaxUSDPerRun,
			},
		}
	}
	return scopes
}

// Resolver resolves a request (bearer key sha + run id) to the firewall scope
// chain it must satisfy. It wraps a policy.Store; the composition root injects one
// into the server, which calls Resolve on the request hot path.
type Resolver struct {
	store *policy.Store
}

// NewResolver wraps a policy store. store must be non-nil.
func NewResolver(store *policy.Store) *Resolver { return &Resolver{store: store} }

// Resolve maps a request to its firewall scope chain. The contract is
// fail-CLOSED, and it distinguishes "not governed" from "error":
//   - governed=false, err=nil  → this key maps to no policy node; the caller
//     should apply its default (flat) enforcement. This is the ONLY fall-through.
//   - governed=false, err!=nil → a resolution FAILURE (unresolvable run id, a
//     broken ancestry that would drop an ancestor's budget). The caller MUST deny
//     — never silently fall back to laxer enforcement.
//   - governed=true, err=nil   → scopes + killedBy are valid; enforce them. A
//     non-empty killedBy names a killed node in the chain (deny before EnterChain).
func (r *Resolver) Resolve(keySHA256, runID string) (scopes []firewall.Scope, killedBy string, governed bool, err error) {
	ch, rerr := r.store.Resolve(keySHA256, runID)
	if rerr != nil {
		if errors.Is(rerr, policy.ErrUnknownKey) {
			return nil, "", false, nil // not policy-governed: caller uses its default
		}
		return nil, "", false, rerr // integrity/input failure: caller must fail closed
	}
	return Translate(ch), ch.KilledBy, true, nil
}
