// Package broker is heave's provider-quota broker (Invariant #9, ADR 0003): it
// reserves a vendor's shared rate limit (RPM / TPM) BEFORE dispatch, so requests
// proactively avoid a 429 and route to a provider that has headroom, instead of
// reacting to a rate-limit error after the fact.
//
// It reuses the atomic cross-replica reserve/settle scope store (ADR 0002): a
// provider scope maps RPM onto the store's "count" rate dimension (reserve 1 per
// request) and TPM onto the token dimension. Brokering is only meaningful with a
// SHARED store — a provider limit is one global number, and per-instance
// brokering across N replicas would enforce N× the real ceiling — so a Broker
// with no store (or no configured limit) is inert (admits everything).
//
// Pure: stdlib only; the store is injected as a stdlib-typed interface.
package broker

import (
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"
)

// windowRetryAfterSec is the hint returned when a provider is at its ceiling; the
// rolling window is one minute, so a retry within ~a minute should find headroom.
const windowRetryAfterSec = 60

// ScopeStore is the shared atomic reserve/settle store (redisstore.Store
// satisfies it). Kept stdlib-typed so this package stays pure. Reserve checks and
// reserves a scope; it FAILS OPEN (admitted=true, non-nil err) on a store error.
type ScopeStore interface {
	Reserve(keys, names []string, maxUSD []float64, maxTokens, maxInflight []int, estUSD float64, estTokens int, holdID string) (admitted bool, deniedName, deniedKind string, err error)
	Settle(keys []string, deltaUSD float64, deltaTokens int) error
	Release(keys []string, holdID string, estUSD float64, estTokens int, settled bool) error
}

// Limit is a provider's known shared quota. 0 disables a dimension.
type Limit struct {
	RPM int // requests per minute
	TPM int // tokens per minute
}

func (l Limit) any() bool { return l.RPM > 0 || l.TPM > 0 }

// Broker reserves provider quota. A nil Broker, a nil store, or a provider
// without a configured limit all admit everything (brokering off).
type Broker struct {
	store  ScopeStore
	limits map[string]Limit
	// degraded counts admits that FAILED OPEN because the shared store was
	// unreachable (brokering silently off for that request) — surfaced via
	// /metrics so an operator can see the wedge control degrade.
	degraded atomic.Uint64
}

// Degraded returns the count of fail-open admits (shared store unreachable).
func (b *Broker) Degraded() uint64 {
	if b == nil {
		return 0
	}
	return b.degraded.Load()
}

// New builds a Broker. store may be nil (brokering disabled); limits maps a
// provider name to its quota.
func New(store ScopeStore, limits map[string]Limit) *Broker {
	return &Broker{store: store, limits: limits}
}

// Active reports whether the broker will enforce a quota for this provider.
func (b *Broker) Active(provider string) bool {
	if b == nil || b.store == nil {
		return false
	}
	return b.limits[provider].any()
}

// Reserve attempts to reserve one request (+ estTokens) of the provider's quota.
// It returns a Lease (nil when brokering is inactive or on fail-open), admitted,
// and — when denied — a Retry-After hint in seconds. A store error FAILS OPEN
// (admitted=true, nil lease) so brokering never blocks traffic on a Redis blip.
func (b *Broker) Reserve(provider string, estTokens int) (lease *Lease, admitted bool, retryAfterSec int) {
	if !b.Active(provider) {
		return nil, true, 0
	}
	l := b.limits[provider]
	key := scopeKey(provider)
	// hold is passed for interface completeness; the broker sets no concurrency
	// dimension (maxInflight 0), so no semaphore hold is actually created.
	hold := newHoldID()
	ok, _, _, err := b.store.Reserve(
		[]string{key}, []string{"provider"},
		[]float64{float64(l.RPM)}, []int{l.TPM}, []int{0},
		1, estTokens, hold,
	)
	if err != nil {
		b.degraded.Add(1)
		return nil, true, 0 // fail open (observable via Degraded)
	}
	if !ok {
		return nil, false, windowRetryAfterSec
	}
	return &Lease{store: b.store, key: key, hold: hold, estTokens: estTokens, rpm: l.RPM > 0, tpm: l.TPM > 0}, true, 0
}

// Lease is a held provider-quota reservation. Settle it on a successful vendor
// response (reconciles reserved tokens to actual, and KEEPS the request counted).
// Release it if the vendor was never successfully called (frees the reservation
// so a skipped/failed request doesn't burn quota). Exactly one of the two
// outcomes should be applied; both are idempotent and nil-safe.
type Lease struct {
	store     ScopeStore
	key, hold string
	estTokens int
	rpm, tpm  bool // which dimensions were actually reserved (avoid writing junk)
	settled   bool
	released  bool
}

// Settle reconciles the reserved TPM to the actual token count. The request count
// (RPM) is exact, so its delta is 0. The reservation stays counted. Skips the
// token dimension entirely when TPM isn't configured.
func (le *Lease) Settle(actualTokens int) {
	if le == nil || le.settled || le.released {
		return
	}
	le.settled = true
	dt := 0
	if le.tpm {
		dt = actualTokens - le.estTokens
	}
	_ = le.store.Settle([]string{le.key}, 0, dt)
}

// Release frees the reservation when the request never billed the provider (a
// candidate skipped after reserve, or a failed dispatch). If already settled it
// is a no-op on the window (the request legitimately counted). Only the
// configured dimensions are unwound.
func (le *Lease) Release() {
	if le == nil || le.released {
		return
	}
	le.released = true
	count := 0.0
	if le.rpm {
		count = 1
	}
	tok := 0
	if le.tpm {
		tok = le.estTokens
	}
	_ = le.store.Release([]string{le.key}, le.hold, count, tok, le.settled)
}

func scopeKey(provider string) string { return "prov:" + provider }

func newHoldID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
