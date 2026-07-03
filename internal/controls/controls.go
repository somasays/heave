// Package controls enforces pre-vendor policy: authentication, per-client rate
// limiting, and per-client budget caps. Every rejection here happens BEFORE any
// request reaches a provider (docs/INVARIANTS.md, Invariant #7) — the whole
// point is to spend the gateway's own CPU rejecting abuse, never a vendor's
// billed tokens.
//
// Budget enforcement uses a reserve/settle model: Reserve holds an upper-bound
// cost estimate atomically before dispatch, so concurrent requests cannot all
// pass the same pre-spend check and overshoot the cap (the TOCTOU the naive
// check-then-add had). Settle reconciles the hold to the actual cost afterward.
// Overshoot is therefore bounded to at most one request's estimate error.
//
// State is IN-MEMORY and PER-INSTANCE. Two consequences operators must know:
//   - Limits are per-process: N replicas allow N× the configured RPM and budget
//     until the shared store (Redis/Postgres) lands in a later phase.
//   - The monthly budget window resets on process restart.
//
// Both are documented on the config fields and in INVARIANTS #7.
package controls

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"sync"
	"time"
)

// ErrUnauthorized means the bearer token was missing or unknown.
var ErrUnauthorized = errors.New("unauthorized")

// RateLimitError means the client exceeded its request rate. RetryAfterSec is a
// hint for the Retry-After header.
type RateLimitError struct{ RetryAfterSec int }

func (e *RateLimitError) Error() string { return "rate limit exceeded" }

// BudgetError means the client is over its monthly budget. RetryAfterSec is the
// seconds until the budget resets (the 1st of next UTC month), so clients get a
// hard "come back later" instead of a bare 429 that invites a month-long retry
// storm.
type BudgetError struct{ RetryAfterSec int }

func (e *BudgetError) Error() string { return "budget exceeded" }

// Client is one authenticated caller's policy, sourced from config.
type Client struct {
	Name string
	// KeySHA256 is the hex SHA-256 of the client's bearer token. The plaintext
	// key is never stored (Invariant #4): operators configure the hash. Keys
	// MUST be high-entropy random (≥256-bit; e.g. `openssl rand -hex 32`) — an
	// unsalted hash of a low-entropy secret is offline-brute-forceable.
	KeySHA256 string
	// MonthlyBudgetUSD caps spend per calendar month; 0 means unlimited.
	MonthlyBudgetUSD float64
	// RateLimitRPM caps requests per minute; 0 means unlimited. The bucket
	// starts full, so a client may burst up to RPM immediately, then sustains
	// RPM/min thereafter.
	RateLimitRPM int
	// Admin grants access to cross-tenant observability endpoints.
	Admin bool
}

// Guard authenticates callers and enforces their limits.
type Guard struct {
	authEnabled bool
	now         func() time.Time
	byHash      map[string]*clientState
}

type clientState struct {
	cfg    Client
	bucket *bucket

	mu       sync.Mutex
	monthKey string
	spentUSD float64
}

// Reservation is a held budget amount, returned by Reserve and reconciled by
// Settle. A nil Reservation (auth disabled / unknown client) is a valid no-op.
type Reservation struct {
	st     *clientState
	amount float64
}

// New builds a Guard. When authEnabled is false, Admit allows every request and
// applies no limits (local-dev mode). now may be nil (defaults to time.Now) and
// is injectable for deterministic tests.
func New(authEnabled bool, clients []Client, now func() time.Time) *Guard {
	if now == nil {
		now = time.Now
	}
	g := &Guard{authEnabled: authEnabled, now: now, byHash: make(map[string]*clientState, len(clients))}
	for _, c := range clients {
		var b *bucket
		if c.RateLimitRPM > 0 {
			b = newBucket(float64(c.RateLimitRPM), float64(c.RateLimitRPM)/60.0)
		}
		g.byHash[strings.ToLower(c.KeySHA256)] = &clientState{cfg: c, bucket: b}
	}
	return g
}

// Admit authenticates the bearer token and checks the rate limit. It returns the
// resolved Client (nil when auth is disabled) or ErrUnauthorized / *RateLimitError.
// Budget is enforced separately by Reserve, once the request's cost is known.
func (g *Guard) Admit(bearer string) (*Client, error) {
	if !g.authEnabled {
		return nil, nil
	}
	st := g.lookup(bearer)
	if st == nil {
		return nil, ErrUnauthorized
	}
	if st.bucket != nil {
		if ok, retry := st.bucket.allow(g.now()); !ok {
			return nil, &RateLimitError{RetryAfterSec: retry}
		}
	}
	c := st.cfg
	return &c, nil
}

// Authenticate verifies a bearer WITHOUT consuming the client's rate-limit bucket
// — for non-chat control-plane reads (e.g. the observability endpoints) that must
// not eat the key's chat RPM. Returns (nil, nil) when auth is disabled.
func (g *Guard) Authenticate(bearer string) (*Client, error) {
	if !g.authEnabled {
		return nil, nil
	}
	st := g.lookup(bearer)
	if st == nil {
		return nil, ErrUnauthorized
	}
	c := st.cfg
	return &c, nil
}

// Reserve holds estUSD (an upper-bound cost estimate) against the client's
// monthly budget, atomically. It rejects with *BudgetError if the hold would
// exceed the cap. A nil client (auth disabled) or a 0 budget reserves nothing
// but still tracks spend for reporting. Always pair with Settle.
func (g *Guard) Reserve(client *Client, estUSD float64) (*Reservation, error) {
	if client == nil {
		return nil, nil
	}
	st := g.byHash[strings.ToLower(client.KeySHA256)]
	if st == nil {
		return nil, nil
	}
	now := g.now()
	st.mu.Lock()
	st.rollMonthLocked(now)
	if st.cfg.MonthlyBudgetUSD > 0 && st.spentUSD+estUSD > st.cfg.MonthlyBudgetUSD {
		st.mu.Unlock()
		return nil, &BudgetError{RetryAfterSec: secondsUntilNextMonthUTC(now)}
	}
	st.spentUSD += estUSD // hold the estimate until Settle reconciles it
	st.mu.Unlock()
	return &Reservation{st: st, amount: estUSD}, nil
}

// Settle reconciles a reservation to the actual cost (release the held estimate,
// book the real amount). Best-effort: never fails the request path. Pass 0 for a
// request that produced no billable spend (e.g. a pre-dispatch failure).
func (g *Guard) Settle(r *Reservation, actualUSD float64) {
	if r == nil {
		return
	}
	r.st.mu.Lock()
	r.st.spentUSD += actualUSD - r.amount
	if r.st.spentUSD < 0 {
		r.st.spentUSD = 0
	}
	r.st.mu.Unlock()
}

func (g *Guard) lookup(bearer string) *clientState {
	if bearer == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(bearer))
	// The map is keyed by the lowercase hex hash and looked up by the same, so a
	// hit is itself the comparison. The stored value is a hash of the caller's
	// input (not a server secret), so there is no timing channel worth guarding.
	return g.byHash[hex.EncodeToString(sum[:])]
}

// rollMonthLocked resets accumulated spend when the calendar month changes.
func (st *clientState) rollMonthLocked(now time.Time) {
	key := now.UTC().Format("2006-01")
	if st.monthKey != key {
		st.monthKey = key
		st.spentUSD = 0
	}
}

// secondsUntilNextMonthUTC returns whole seconds from now to 00:00 on the 1st of
// next UTC month (at least 1).
func secondsUntilNextMonthUTC(now time.Time) int {
	u := now.UTC()
	next := time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	s := int(next.Sub(u).Seconds())
	if s < 1 {
		s = 1
	}
	return s
}

// bucket is a token bucket with an injectable clock (via allow's now arg).
type bucket struct {
	mu           sync.Mutex
	capacity     float64
	tokens       float64
	refillPerSec float64
	last         time.Time
}

func newBucket(capacity, refillPerSec float64) *bucket {
	return &bucket{capacity: capacity, tokens: capacity, refillPerSec: refillPerSec}
}

// allow consumes one token if available, returning ok and, when not ok, the
// whole seconds to wait before a token is available.
func (b *bucket) allow(now time.Time) (bool, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last = now
	}
	// elapsed<0 only via a non-monotonic injected clock (tests); real time.Now
	// is monotonic. Guard against it so a backward clock never adds tokens.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillPerSec)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	retry := int(math.Ceil((1 - b.tokens) / b.refillPerSec))
	if retry < 1 {
		retry = 1
	}
	return false, retry
}
