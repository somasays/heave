// Package firewall is heave's runtime spend & quota firewall: real-time,
// PRE-vendor enforcement for agentic traffic (docs/INVARIANTS.md, Invariant #9).
// It generalizes the reserve/settle idea (Invariant #7) from a monthly budget to
// short time constants and run scope:
//   - $/min and tokens/min velocity caps over a rolling 60s window, per key AND
//     per run, RESERVED at admit and reconciled at settle so concurrent requests
//     cannot overshoot the cap,
//   - per-run kill switches (manual, or auto-tripped by loop detection),
//   - repeated-prompt detection over a sliding window (a run resending the same
//     prompt is a runaway; note: exact-hash, so a per-turn nonce defeats it —
//     it is a heuristic, not a security control),
//   - in-flight concurrency caps.
//
// Run scope is namespaced by the AUTHENTICATED key, so a client can only affect
// its own runs (a spoofed X-Heave-Run-Id cannot kill or poison another caller's
// run). Pure (stdlib only), injectable clock.
//
// Kill state is LAYERED: an always-on local store plus an optional shared store
// (Redis, via WithKillStore). A kill on one replica propagates to all, and a
// locally-issued kill still takes effect if the shared store is unreachable.
// Velocity and concurrency state, by contrast, remain in-memory and PER-INSTANCE:
// with N replicas the effective ceiling is N× each cap. Maps are idle-swept and
// size-capped so the enforcement point cannot be OOM'd by rotating run ids; at
// the kill-map cap a new kill is REFUSED (surfaced as an error) rather than
// evicting a live kill, since dropping one would silently resurrect a runaway.
package firewall

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	windowSeconds = 60
	// eviction bounds so a caller rotating run ids cannot grow memory unbounded.
	defaultMaxScopes = 50_000
	gcInterval       = 30 * time.Second
	idleEvictTTL     = 2 * windowSeconds * time.Second
	// DefaultKillTTL bounds how long a kill is remembered; the timestamp is
	// refreshed each time the run is checked, so an ACTIVE killed run stays dead
	// while an abandoned/rotated run id is eventually forgotten. Overridable via
	// Limits.KillTTL.
	DefaultKillTTL = 24 * time.Hour
	// maxKills caps the local kill map so a caller spraying run ids at the kill
	// endpoint cannot OOM the enforcement point. When the cap is reached (after
	// sweeping expired entries) a NEW kill is REFUSED with an error rather than
	// evicting a still-live kill — dropping a live kill would silently resurrect
	// a runaway, so we fail loud instead (the endpoint surfaces it as 5xx).
	maxKills = 100_000
)

// ErrKilled means the run has been killed (manually or by loop detection).
var ErrKilled = errors.New("run killed")

// ErrBadChain means an EnterChain chain is malformed: a scope with an empty Key,
// an over-long Key, or more than one scope named "run". The firewall fails CLOSED
// on these rather than silently enforce a chain where kill/budget bind to the
// wrong (or no) run key, or where distinct runs collide on one empty counter key.
var ErrBadChain = errors.New("invalid scope chain")

// maxScopeKeyLen bounds a caller-supplied scope key so a direct EnterChain caller
// cannot spray or oversize the reserve keyspace. Policy ids are ≤128 chars and a
// run key is ~"run:app:<128>\x00<128>", well under this.
const maxScopeKeyLen = 512

// ErrKillStoreFull means the local kill map is at capacity with live kills, so a
// new kill could not be recorded without evicting a live one. It surfaces so the
// caller reports failure (and can retry) rather than silently under-enforcing.
var ErrKillStoreFull = errors.New("kill store full")

// VelocityError means a rolling-window rate cap ($/min or tokens/min) would be
// exceeded. RetryAfterSec hints when the window will have drained.
type VelocityError struct {
	Scope         string // "key" or "run"
	RetryAfterSec int
}

func (e *VelocityError) Error() string { return "velocity limit exceeded (" + e.Scope + ")" }

// ConcurrencyError means too many in-flight requests for the key/run.
type ConcurrencyError struct{ Scope string }

func (e *ConcurrencyError) Error() string { return "concurrency limit exceeded (" + e.Scope + ")" }

// Limits are the firewall thresholds (0 = unlimited / disabled per field).
type Limits struct {
	MaxUSDPerMin    float64
	MaxTokensPerMin int
	MaxConcurrent   int
	// LoopThreshold auto-kills a run after the same prompt hash recurs this many
	// times within a recent-history window. 0 disables it.
	LoopThreshold int
	// MaxUSDPerRun caps a single run's ESTIMATED spend over its ACTIVE lifetime:
	// once a run's reserved+settled spend would exceed the cap, the run is
	// auto-KILLED. It is the backstop for a changing-prompt runaway that loop
	// detection cannot see. Three caveats keep the claim honest (it is NOT an
	// absolute hard cap):
	//   - Enforced on the reserved ESTIMATE. Output is bounded by the request's
	//     max_tokens (set max_output_tokens per model), but the input estimate is
	//     a chars/4 heuristic, NOT a strict upper bound — adversarial input can
	//     tokenize higher, so a single admitted call may bill above its estimate.
	//     A run can thus exceed the cap by ~one call's estimation error, or more
	//     if MaxConcurrent admits several in-flight at once — pair the two, and a
	//     tokenizer-accurate estimate tightens it.
	//   - ACTIVE lifetime: a run's accounting is reclaimed when its scope goes
	//     idle (gcLocked), so a run idle beyond the eviction window restarts at
	//     zero. The per-client MONTHLY budget (Invariant #7) is the absolute,
	//     non-evictable backstop across idle gaps and run-id rotation.
	//   - Requires a run id (per-run scope); 0 disables it.
	MaxUSDPerRun float64
	// KillTTL bounds how long a kill is remembered (local + shared). 0 = default.
	KillTTL time.Duration
}

// Scope is one enforceable level of a request's chain: a stable reserve-store /
// map key plus THAT level's own caps. EnterChain enforces velocity + concurrency
// independently per scope (all-or-nothing across the chain) and the per-run $
// budget on the scope named "run". It mirrors policy.Scope; the server translates
// a resolved policy chain into these so the firewall stays pure (it never imports
// policy). Loop detection and kill remain run-scoped and use the run scope's Key.
type Scope struct {
	Name string // "org" | "team" | "app" | "run" | "key"
	// Key is the stable reserve-store / in-memory counter key. The firewall trusts
	// it VERBATIM (it is also the kill key for a "run" scope), so the caller MUST
	// supply non-empty, delimiter-safe, type-namespaced, cross-tenant-unique keys —
	// policy's validated keys ("org:<id>", "run:<owner>\x00<id>", …) satisfy this.
	Key    string
	Limits Limits // this level's own caps (only the velocity/concurrency/run fields apply)
}

// Firewall enforces Limits across keys and runs.
type Firewall struct {
	enabled bool
	limits  Limits
	now     func() time.Time

	// Kill state is LAYERED: a local in-memory store is always consulted (so a
	// kill issued on this replica takes effect immediately and survives a Redis
	// outage), plus an optional shared store (Redis) that propagates kills across
	// replicas. A run is killed if EITHER says so.
	localKills  *memKillStore
	sharedKills KillStore
	// sharedKillErrs counts shared-store (Redis) write failures across BOTH the
	// manual and loop-trip kill paths, so a silent propagation failure on the
	// auto path is still observable via /metrics.
	sharedKillErrs atomic.Uint64

	// scopeStore, when set, moves velocity + concurrency enforcement OFF local
	// memory and into a shared (Redis) store so the caps hold across replicas
	// (ADR 0002). Loop detection and the per-run $ budget stay local.
	scopeStore ScopeStore
	// scopeDegraded counts admits that failed OPEN because the shared scope store
	// was unreachable (enforcement degraded to unenforced) — observable via Stats.
	scopeDegraded atomic.Uint64

	mu     sync.Mutex
	scopes map[string]*scopeState
	loops  map[string]*loopState
	lastGC time.Time
}

// Stats is a point-in-time snapshot of firewall kill-state health, surfaced on
// /metrics so operators can see the DoS backstop and propagation failures.
type Stats struct {
	// LocalKills is the number of live entries in the local kill map.
	LocalKills int
	// KillRejections counts kills refused because the local map was full of live
	// kills (a nonzero value means the enforcement point is under kill pressure).
	KillRejections uint64
	// SharedKillErrors counts shared-store write failures (kills that may not have
	// propagated to other replicas).
	SharedKillErrors uint64
	// ScopeDegraded counts admits that failed OPEN because the shared velocity/
	// concurrency store was unreachable (caps degraded to unenforced).
	ScopeDegraded uint64
}

// KillStore is a shared run-kill store (e.g. Redis). Kill returns an error so a
// failed write can be surfaced (a kill switch must not silently no-op). Killed
// reports whether a run is killed; implementations should fail open (return
// false on error) so a store outage doesn't block all traffic.
type KillStore interface {
	Kill(runKey string) error
	Killed(runKey string) bool
}

// ScopeStore is a shared, atomic velocity + concurrency reserve/settle store
// (e.g. Redis), keeping the firewall pure via stdlib-only signatures. Reserve
// checks and reserves ALL scopes atomically (all-or-nothing); on a cap breach it
// returns admitted=false with the breached scope name + kind ("velocity" |
// "concurrency"). It FAILS OPEN: on a store error it returns admitted=true and a
// non-nil error so the caller can record the degradation. Settle reconciles a
// reservation to actual spend; Release frees the concurrency hold (holdID) and,
// if unsettled, the reserved estimate. Scopes are parallel slices (keys[i] has
// caps maxUSD[i]/maxTokens[i]/maxInflight[i], display name names[i]).
type ScopeStore interface {
	Reserve(keys, names []string, maxUSD []float64, maxTokens, maxInflight []int, estUSD float64, estTokens int, holdID string) (admitted bool, deniedName, deniedKind string, err error)
	Settle(keys []string, deltaUSD float64, deltaTokens int) error
	Release(keys []string, holdID string, estUSD float64, estTokens int, settled bool) error
}

// newHoldID returns a globally-unique concurrency-hold id (crypto-random, so it
// cannot collide across replicas sharing one store).
func newHoldID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type scopeState struct {
	window    *window
	inflight  int
	lastTouch time.Time
	// runUSD is the CUMULATIVE reserved+settled spend for a run scope (never
	// windowed); used only when MaxUSDPerRun is set. It is reclaimed with the
	// scope when the run goes idle, so it caps a run's ACTIVE lifetime spend.
	runUSD float64
}

type loopState struct {
	recent    []string
	lastTouch time.Time
}

// New builds a Firewall. When enabled is false, Enter returns an inert Ticket
// that admits everything. now may be nil (time.Now); injectable for tests. Kill
// state defaults to in-memory; use WithKillStore to share it across replicas.
func New(enabled bool, limits Limits, now func() time.Time) *Firewall {
	if now == nil {
		now = time.Now
	}
	ttl := limits.KillTTL
	if ttl <= 0 {
		ttl = DefaultKillTTL
	}
	return &Firewall{
		enabled:    enabled,
		limits:     limits,
		now:        now,
		localKills: newMemKillStore(now, ttl),
		scopes:     map[string]*scopeState{},
		loops:      map[string]*loopState{},
	}
}

// WithKillStore adds a shared kill store (e.g. Redis) alongside the always-on
// local store and returns f. Kills then propagate across replicas, while a
// locally-issued kill still takes effect if the shared store is unreachable.
// Only the composition root (cmd) should call this, once, before serving.
func (f *Firewall) WithKillStore(ks KillStore) *Firewall {
	if ks != nil {
		f.sharedKills = ks
	}
	return f
}

// WithScopeStore moves velocity + concurrency enforcement into a shared store
// (e.g. Redis) so the caps hold across replicas, and returns f. Only the
// composition root (cmd) should call this, once, before serving.
func (f *Firewall) WithScopeStore(ss ScopeStore) *Firewall {
	if ss != nil {
		f.scopeStore = ss
	}
	return f
}

// isKilled reports whether a run is killed per EITHER the local or shared store.
// Callers must not hold f.mu: the shared store may do network I/O.
func (f *Firewall) isKilled(runKey string) bool {
	if runKey == "" {
		return false
	}
	if f.localKills.Killed(runKey) {
		return true
	}
	return f.sharedKills != nil && f.sharedKills.Killed(runKey)
}

// doKill records a kill in the local store and the shared store (if configured).
// A local failure (map full of live kills) is returned immediately — the kill did
// NOT record here, so the caller must not report success. A shared failure is
// counted (observability for the swallowed auto-kill path) and returned: the kill
// is in effect on THIS replica but may not have propagated.
func (f *Firewall) doKill(runKey string) error {
	if err := f.localKills.Kill(runKey); err != nil {
		return err
	}
	if f.sharedKills != nil {
		if err := f.sharedKills.Kill(runKey); err != nil {
			f.sharedKillErrs.Add(1)
			return err
		}
	}
	return nil
}

// Stats snapshots kill-state health for /metrics.
func (f *Firewall) Stats() Stats {
	local, rejects := f.localKills.stats()
	return Stats{
		LocalKills:       local,
		KillRejections:   rejects,
		SharedKillErrors: f.sharedKillErrs.Load(),
		ScopeDegraded:    f.scopeDegraded.Load(),
	}
}

// memKillStore is the default in-memory KillStore: a map with self-expiring kills.
// It is bounded by cap: at the cap it REFUSES new kills (never drops a live one).
type memKillStore struct {
	mu       sync.Mutex
	m        map[string]time.Time
	now      func() time.Time
	ttl      time.Duration
	cap      int
	rejected uint64 // kills refused because the map was full of live kills
}

func newMemKillStore(now func() time.Time, ttl time.Duration) *memKillStore {
	return &memKillStore{m: map[string]time.Time{}, now: now, ttl: ttl, cap: maxKills}
}

func (s *memKillStore) Kill(runKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// Re-killing an already-tracked run is always allowed (refreshes, no growth).
	// A NEW run at the cap: sweep expired first; if the map is still full it is
	// full of LIVE kills, so refuse rather than resurrect a runaway by eviction.
	if _, exists := s.m[runKey]; !exists && len(s.m) >= s.cap {
		s.gcLocked(now)
		if len(s.m) >= s.cap {
			s.rejected++
			return ErrKillStoreFull
		}
	}
	s.m[runKey] = now
	return nil
}

func (s *memKillStore) Killed(runKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[runKey]
	if !ok {
		return false
	}
	now := s.now()
	if now.Sub(t) > s.ttl {
		delete(s.m, runKey)
		return false
	}
	// Refresh on hit so an ACTIVE killed run (still being checked on Enter) never
	// ages out and resurrects; an abandoned kill stops being checked and expires.
	s.m[runKey] = now
	return true
}

// gc actively sweeps expired kills. It is called periodically (from the
// firewall GC) so abandoned run ids are reclaimed even if never read again.
func (s *memKillStore) gc(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
}

func (s *memKillStore) gcLocked(now time.Time) {
	for k, t := range s.m {
		if now.Sub(t) > s.ttl {
			delete(s.m, k)
		}
	}
}

func (s *memKillStore) stats() (size int, rejected uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m), s.rejected
}

// Enabled reports whether the firewall enforces anything.
func (f *Firewall) Enabled() bool { return f.enabled }

// Ticket is a held reservation. Release MUST be called on every path (defer);
// Settle is called once on a successful vendor response to reconcile the held
// estimate to the actual spend.
type Ticket struct {
	fw        *Firewall
	scopeKeys []string // LOCAL-mode velocity/concurrency scopes (empty in shared mode)
	// runScopeKey is the run scope's map key when a per-run budget is tracked
	// ("" otherwise), so Settle/Release can reconcile the cumulative runUSD.
	runScopeKey string
	// store/storeKeys/holdID are set in SHARED mode: velocity/concurrency
	// reserve/settle/release go to the cross-replica store keyed by holdID.
	store     ScopeStore
	storeKeys []string
	holdID    string
	estUSD    float64
	estTokens int
	released  bool
	settled   bool
}

// Enter is the pre-vendor gate for the built-in {key, run} scopes, both enforced
// against the firewall's single configured Limits. key is the authenticated
// client (may be ""); runID is the agent run (may be "" → run scope skipped);
// est* is the request's upper-bound cost/token estimate, held until Settle. It is
// a thin wrapper over EnterChain: it builds the two-scope chain and delegates.
func (f *Firewall) Enter(key, runID, promptHash string, estUSD float64, estTokens int) (*Ticket, error) {
	if !f.enabled {
		return &Ticket{}, nil
	}
	var scopes []Scope
	if key != "" {
		scopes = append(scopes, Scope{Name: "key", Key: "key:" + key, Limits: f.limits})
	}
	runKey := f.runKey(key, runID)
	if runKey != "" {
		scopes = append(scopes, Scope{Name: "run", Key: runKey, Limits: f.limits})
	}
	return f.enterChain(scopes, runKey, promptHash, f.limits.MaxUSDPerRun, estUSD, estTokens)
}

// EnterChain is the pre-vendor gate for a RESOLVED policy chain: an ordered set of
// scopes (org▸team▸app▸run), each carrying its OWN caps. Velocity + concurrency
// are enforced independently per scope (all-or-nothing across the chain); the
// per-run $ budget, loop detection, and kill are enforced on the scope named
// "run" (its Key is the run's unique kill/budget key). This is the generalization
// of Enter from one global Limits to per-scope caps (docs/adr/0006); Enter is the
// two-scope special case.
//
// Preconditions the caller (the server, translating a policy.Chain) MUST honor —
// the firewall trusts the keys verbatim and cannot re-derive them:
//   - Every scope Key is non-empty, ≤maxScopeKeyLen, delimiter-safe, and
//     collision-free across tenants (policy's validated, type-namespaced keys
//     satisfy this). A violation returns ErrBadChain — fail closed.
//   - At most ONE scope is named "run" (else ErrBadChain). A chain may have no run
//     scope (no run id); then run-level budget/loop/kill simply don't apply.
//   - To later KILL a run entered here, call KillRun with the SAME run scope Key
//     (not Kill(ownerKey,runID), which derives a different key).
//   - NODE-level kills (policy Chain.KilledBy for a killed org/team/app) are NOT
//     visible to the firewall; the caller must deny those BEFORE calling here.
//   - Loop detection uses the firewall's global LoopThreshold (not a per-scope
//     value); it is inert if the firewall was built with LoopThreshold == 0.
func (f *Firewall) EnterChain(scopes []Scope, promptHash string, estUSD float64, estTokens int) (*Ticket, error) {
	if !f.enabled {
		return &Ticket{}, nil
	}
	var runKey string
	var runMaxUSD float64
	runScopes := 0
	for _, sc := range scopes {
		if sc.Key == "" || len(sc.Key) > maxScopeKeyLen {
			return nil, ErrBadChain // fail closed: empty/oversized keys collide or spray
		}
		if sc.Name == "run" {
			runScopes++
			runKey = sc.Key
			runMaxUSD = sc.Limits.MaxUSDPerRun
		}
	}
	if runScopes > 1 {
		return nil, ErrBadChain // ambiguous: which run drives kill/budget?
	}
	return f.enterChain(scopes, runKey, promptHash, runMaxUSD, estUSD, estTokens)
}

// enterChain is the shared admit core: kill check, then per-scope velocity +
// concurrency reserve plus the per-run budget. runKey is the run's kill/budget key
// ("" if the chain has no run scope); runMaxUSD is the per-run $ cap to enforce on
// it. Assumes f.enabled.
func (f *Firewall) enterChain(scopes []Scope, runKey, promptHash string, runMaxUSD, estUSD float64, estTokens int) (*Ticket, error) {
	// A negative estimate must never DEFLATE a counter (a buggy/hostile caller
	// could otherwise reserve "negative spend" and under-enforce). Floor at 0; the
	// real spend is reconciled at Settle.
	if estUSD < 0 {
		estUSD = 0
	}
	if estTokens < 0 {
		estTokens = 0
	}
	// Kill check is OUTSIDE f.mu: the store may do network I/O (Redis), which
	// must never be held under the global firewall lock.
	if f.isKilled(runKey) {
		return nil, ErrKilled
	}

	// Shared mode: velocity + concurrency go to the cross-replica store; loop
	// detection and the per-run $ budget stay local.
	if f.scopeStore != nil {
		return f.enterShared(scopes, runKey, promptHash, runMaxUSD, estUSD, estTokens)
	}

	ticket, tripped, err := f.enterLocked(scopes, runKey, promptHash, runMaxUSD, estUSD, estTokens)
	if tripped {
		// An auto-kill trip (loop detection OR per-run budget): kill the run
		// (local + shared) outside the lock, since the shared store may do network
		// I/O. Unlike the manual Kill endpoint (which surfaces a write failure as
		// 5xx), the auto paths intentionally swallow doKill's error — the local
		// kill still records, and if it couldn't (map full) the next request
		// re-trips and retries; KillRejections/SharedKillErrors stay observable via
		// Stats.
		_ = f.doKill(runKey)
		return nil, ErrKilled
	}
	return ticket, err
}

// enterLocked runs the check-and-reserve under f.mu. It returns tripped=true
// (with a nil ticket) when loop detection fires, so the caller can issue the
// kill after unlocking. The defer keeps the lock panic-safe.
func (f *Firewall) enterLocked(scopes []Scope, runKey, promptHash string, runMaxUSD, estUSD float64, estTokens int) (tk *Ticket, tripped bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.gcLocked(now)

	tripped, budgetKey := f.checkLoopBudgetLocked(runKey, promptHash, estUSD, runMaxUSD, now)
	if tripped {
		return nil, true, nil
	}
	mapKeys, verr := f.checkReserveScopesLocked(toScopeKeys(scopes), estUSD, estTokens, now)
	if verr != nil {
		f.unreserveBudgetLocked(budgetKey, estUSD) // roll back the budget reserved above
		return nil, false, verr
	}
	return &Ticket{fw: f, scopeKeys: mapKeys, runScopeKey: budgetKey, estUSD: estUSD, estTokens: estTokens}, false, nil
}

// checkLoopBudgetLocked runs the LOCAL loop-detection + per-run-budget checks and,
// if the budget applies, RESERVES the estimate against it. runMaxUSD is the run
// scope's own per-run $ cap (the tightest ancestor value, pre-resolved). Returns
// tripped=true when either fires (caller then kills), and the budget scope key
// (for reconcile/rollback). Assumes f.mu is held.
func (f *Firewall) checkLoopBudgetLocked(runKey, promptHash string, estUSD, runMaxUSD float64, now time.Time) (tripped bool, budgetKey string) {
	if runKey != "" && f.limits.LoopThreshold > 0 && promptHash != "" &&
		f.tripLoopLocked(runKey, promptHash, now) {
		return true, ""
	}
	// Per-run cumulative budget: if this request would push the run over its hard
	// $ cap, KILL the run (trip) — the backstop for runaways whose prompts keep
	// changing, which loop detection cannot see.
	if runKey != "" && runMaxUSD > 0 {
		spent := 0.0
		if st := f.scopes[runKey]; st != nil {
			spent = st.runUSD
		}
		if spent+estUSD > runMaxUSD {
			return true, ""
		}
		st := f.ensureScopeLocked(runKey, now)
		st.runUSD += estUSD
		return false, runKey
	}
	return false, ""
}

// checkReserveScopesLocked does the local velocity + concurrency check-all (all-or
// -nothing) then reserve-all for the given scopes, returning the reserved map
// keys. Assumes f.mu is held. This is the local enforcement shared by the normal
// local path AND the shared-mode fallback when Redis is unreachable.
func (f *Firewall) checkReserveScopesLocked(keys []scopeKey, estUSD float64, estTokens int, now time.Time) ([]string, error) {
	for _, sk := range keys {
		if verr := f.checkVelocityLocked(sk, estUSD, estTokens, now); verr != nil {
			return nil, verr
		}
	}
	for _, sk := range keys {
		if sk.limits.MaxConcurrent > 0 {
			if st := f.scopes[sk.mapKey]; st != nil && st.inflight >= sk.limits.MaxConcurrent {
				return nil, &ConcurrencyError{Scope: sk.name}
			}
		}
	}
	mapKeys := make([]string, 0, len(keys))
	for _, sk := range keys {
		st := f.ensureScopeLocked(sk.mapKey, now)
		st.window.add(now.Unix(), estUSD, estTokens) // reserve the estimate
		st.inflight++
		mapKeys = append(mapKeys, sk.mapKey)
	}
	return mapKeys, nil
}

// unreserveBudgetLocked rolls back a per-run budget reservation. Assumes f.mu held.
func (f *Firewall) unreserveBudgetLocked(budgetKey string, estUSD float64) {
	if budgetKey == "" {
		return
	}
	if st := f.scopes[budgetKey]; st != nil {
		st.runUSD -= estUSD
		if st.runUSD < 0 {
			st.runUSD = 0
		}
	}
}

// enterShared is the admit path when a shared ScopeStore is configured: the local
// loop + per-run-budget checks run under f.mu, then velocity + concurrency
// check-and-reserve go to the shared store (one atomic op across replicas). On a
// store denial the local budget reservation is rolled back so no partial state
// leaks.
func (f *Firewall) enterShared(scopes []Scope, runKey, promptHash string, runMaxUSD, estUSD float64, estTokens int) (*Ticket, error) {
	tripped, budgetKey := f.reserveLocalLocked(runKey, promptHash, runMaxUSD, estUSD)
	if tripped {
		_ = f.doKill(runKey)
		return nil, ErrKilled
	}

	keys, names, maxUSD, maxTok, maxConc := sharedCaps(scopes)
	if len(keys) == 0 {
		// No velocity/concurrency caps on any scope: only the (local) budget applies.
		return &Ticket{fw: f, runScopeKey: budgetKey, estUSD: estUSD, estTokens: estTokens}, nil
	}
	holdID := newHoldID()
	admitted, dname, dkind, err := f.scopeStore.Reserve(keys, names, maxUSD, maxTok, maxConc, estUSD, estTokens, holdID)
	if err != nil {
		// Redis unreachable: DON'T admit blind (that would drop the caps fleet-wide,
		// worse than the no-Redis baseline). Fall back to LOCAL per-instance
		// enforcement for this request — still bounded (N×), and the resulting ticket
		// reconciles against LOCAL state (not the store), so it can't corrupt the
		// shared window. Observable via Stats.ScopeDegraded.
		f.scopeDegraded.Add(1)
		return f.reserveScopesLocal(scopes, budgetKey, estUSD, estTokens)
	}
	if !admitted {
		f.releaseBudgetLocked(budgetKey, estUSD) // roll back the local budget reserve
		if dkind == "concurrency" {
			return nil, &ConcurrencyError{Scope: dname}
		}
		return nil, &VelocityError{Scope: dname, RetryAfterSec: windowSeconds}
	}
	return &Ticket{
		fw: f, store: f.scopeStore, storeKeys: keys, holdID: holdID,
		runScopeKey: budgetKey, estUSD: estUSD, estTokens: estTokens,
	}, nil
}

// reserveLocalLocked runs the LOCAL parts of admit (loop detection + per-run
// budget) under f.mu. It returns tripped=true when either fires (the caller then
// kills), and the run-scope key if a budget reservation was held (for rollback /
// settle).
func (f *Firewall) reserveLocalLocked(runKey, promptHash string, runMaxUSD, estUSD float64) (tripped bool, budgetKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.gcLocked(now)
	return f.checkLoopBudgetLocked(runKey, promptHash, estUSD, runMaxUSD, now)
}

// reserveScopesLocal is the shared-mode fallback when Redis is unreachable: it
// runs the LOCAL velocity/concurrency check-and-reserve (loop + budget already
// done by reserveLocalLocked) and returns a LOCAL-backed ticket. On a local cap
// breach it rolls back the budget reservation.
func (f *Firewall) reserveScopesLocal(scopes []Scope, budgetKey string, estUSD float64, estTokens int) (*Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	mapKeys, err := f.checkReserveScopesLocked(toScopeKeys(scopes), estUSD, estTokens, now)
	if err != nil {
		f.unreserveBudgetLocked(budgetKey, estUSD)
		return nil, err
	}
	return &Ticket{fw: f, scopeKeys: mapKeys, runScopeKey: budgetKey, estUSD: estUSD, estTokens: estTokens}, nil
}

// releaseBudgetLocked unwinds a per-run budget reservation (used when the shared
// store denies after the local budget was reserved).
func (f *Firewall) releaseBudgetLocked(budgetKey string, estUSD float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreserveBudgetLocked(budgetKey, estUSD)
}

// sharedCaps flattens a chain into the parallel slices the ScopeStore expects,
// INCLUDING only scopes that actually carry a velocity or concurrency cap (a
// run scope carrying only MaxUSDPerRun is enforced locally, not in the store).
func sharedCaps(scopes []Scope) (keys, names []string, maxUSD []float64, maxTok, maxConc []int) {
	for _, sc := range scopes {
		if sc.Limits.MaxUSDPerMin <= 0 && sc.Limits.MaxTokensPerMin <= 0 && sc.Limits.MaxConcurrent <= 0 {
			continue
		}
		keys = append(keys, sc.Key)
		names = append(names, sc.Name)
		maxUSD = append(maxUSD, sc.Limits.MaxUSDPerMin)
		maxTok = append(maxTok, sc.Limits.MaxTokensPerMin)
		maxConc = append(maxConc, sc.Limits.MaxConcurrent)
	}
	return keys, names, maxUSD, maxTok, maxConc
}

// Settle reconciles the held estimate to the actual spend (delta into the same
// windows). Call once after a successful response.
func (tk *Ticket) Settle(actualUSD float64, actualTokens int) {
	if tk.fw == nil {
		return
	}
	tk.fw.mu.Lock()
	// Settle must run before Release (the handler does; defer Release fires after).
	// Guard against a released-then-settled ordering, which would double-unwind the
	// reservation and drive runUSD negative.
	if tk.settled || tk.released {
		tk.fw.mu.Unlock()
		return
	}
	tk.settled = true
	now := tk.fw.now().Unix()
	for _, mk := range tk.scopeKeys {
		if st := tk.fw.scopes[mk]; st != nil {
			st.window.add(now, actualUSD-tk.estUSD, actualTokens-tk.estTokens)
		}
	}
	// Reconcile the cumulative per-run budget from the reserved estimate to actual.
	if tk.runScopeKey != "" {
		if st := tk.fw.scopes[tk.runScopeKey]; st != nil {
			st.runUSD += actualUSD - tk.estUSD
			if st.runUSD < 0 {
				st.runUSD = 0
			}
		}
	}
	tk.fw.mu.Unlock()
	// Shared velocity reconcile is done AFTER unlocking (network I/O must not hold
	// f.mu); the settled flag above makes it idempotent. A failed reconcile is
	// counted so a silent shared-window drift is observable.
	if tk.store != nil {
		if err := tk.store.Settle(tk.storeKeys, actualUSD-tk.estUSD, actualTokens-tk.estTokens); err != nil {
			tk.fw.scopeDegraded.Add(1)
		}
	}
}

// Release frees the concurrency slots and, if the request never settled (it
// failed), removes the reserved estimate from the windows.
func (tk *Ticket) Release() {
	if tk.fw == nil {
		return
	}
	tk.fw.mu.Lock()
	if tk.released {
		tk.fw.mu.Unlock()
		return
	}
	tk.released = true
	settled := tk.settled
	now := tk.fw.now().Unix()
	for _, mk := range tk.scopeKeys {
		st := tk.fw.scopes[mk]
		if st == nil {
			continue
		}
		if st.inflight > 0 {
			st.inflight--
		}
		if !settled {
			st.window.add(now, -tk.estUSD, -tk.estTokens) // release the hold
		}
	}
	// A failed (unsettled) request releases its cumulative per-run reservation too,
	// so a run isn't charged for spend that never happened.
	if !settled && tk.runScopeKey != "" {
		if st := tk.fw.scopes[tk.runScopeKey]; st != nil {
			st.runUSD -= tk.estUSD
			if st.runUSD < 0 {
				st.runUSD = 0
			}
		}
	}
	tk.fw.mu.Unlock()
	// Shared mode: free the concurrency hold (and, if unsettled, the velocity
	// reservation) after unlocking — network I/O must not hold f.mu. A failed
	// release (a possibly-leaked hold, reaped later by TTL) is counted.
	if tk.store != nil {
		if err := tk.store.Release(tk.storeKeys, tk.holdID, tk.estUSD, tk.estTokens, settled); err != nil {
			tk.fw.scopeDegraded.Add(1)
		}
	}
}

// KillRun hard-stops the run identified by its EXACT run scope key — the same key
// EnterChain enforced kill/budget under (for a policy chain, the "run" scope's
// Key). This single-sources admit and kill: the manual kill path and the admit
// path key off the same string, so a chain-entered run is actually killable.
// runKey=="" is a no-op. The returned error is the shared store's write error
// (nil when there is no shared store): a non-nil error means the kill is in effect
// on this replica but may not have propagated, so the caller MUST surface it.
func (f *Firewall) KillRun(runKey string) error {
	if runKey == "" {
		return nil
	}
	return f.doKill(runKey)
}

// RunKilled reports whether the run with this exact run scope key is killed.
func (f *Firewall) RunKilled(runKey string) bool { return f.isKilled(runKey) }

// Kill hard-stops a run owned by ownerKey, using the built-in Enter scoping. A
// client can only kill its own runs. For a run entered via EnterChain, use KillRun
// with the run scope's Key instead — this derivation is Enter-specific.
func (f *Firewall) Kill(ownerKey, runID string) error {
	if runID == "" {
		return nil
	}
	return f.KillRun(f.runKey(ownerKey, runID))
}

// Killed reports whether a run (owned by ownerKey) has been killed. For a run
// entered via EnterChain, use RunKilled with the run scope's Key instead.
func (f *Firewall) Killed(ownerKey, runID string) bool {
	if runID == "" {
		return false
	}
	return f.RunKilled(f.runKey(ownerKey, runID))
}

type scopeKey struct {
	name   string // "key" | "run" | "org" | "team" | "app"
	mapKey string
	limits Limits // this scope's own velocity/concurrency caps
}

// toScopeKeys projects a chain onto the internal (name, mapKey, limits) triples
// the local velocity/concurrency reserve operates on.
func toScopeKeys(scopes []Scope) []scopeKey {
	sk := make([]scopeKey, len(scopes))
	for i, sc := range scopes {
		sk[i] = scopeKey{name: sc.Name, mapKey: sc.Key, limits: sc.Limits}
	}
	return sk
}

// runKey namespaces a run by its owner so cross-tenant interference is impossible.
// The owner is length-prefixed so the owner/run boundary is unambiguous even if
// the owner (an operator-set client name) contains the separator byte — a raw
// delimiter alone could be spoofed into a cross-owner collision.
func (f *Firewall) runKey(ownerKey, runID string) string {
	if runID == "" {
		return ""
	}
	return "run:" + strconv.Itoa(len(ownerKey)) + ":" + ownerKey + "\x00" + runID
}

func (f *Firewall) ensureScopeLocked(mapKey string, now time.Time) *scopeState {
	st := f.scopes[mapKey]
	if st == nil {
		st = &scopeState{window: newWindow(windowSeconds)}
		f.scopes[mapKey] = st
	}
	st.lastTouch = now
	return st
}

func (f *Firewall) checkVelocityLocked(sk scopeKey, estUSD float64, estTokens int, now time.Time) error {
	if sk.limits.MaxUSDPerMin <= 0 && sk.limits.MaxTokensPerMin <= 0 {
		return nil
	}
	st := f.scopes[sk.mapKey]
	if st == nil {
		return nil
	}
	usd, toks := st.window.sum(now.Unix())
	if sk.limits.MaxUSDPerMin > 0 && usd+estUSD > sk.limits.MaxUSDPerMin {
		return &VelocityError{Scope: sk.name, RetryAfterSec: windowSeconds}
	}
	if sk.limits.MaxTokensPerMin > 0 && toks+estTokens > sk.limits.MaxTokensPerMin {
		return &VelocityError{Scope: sk.name, RetryAfterSec: windowSeconds}
	}
	return nil
}

// tripLoopLocked records promptHash in the run's recent history and reports
// whether it now recurs >= LoopThreshold times within that window (so an
// A,B,A,B alternation trips, not only consecutive repeats).
func (f *Firewall) tripLoopLocked(runKey, promptHash string, now time.Time) bool {
	ls := f.loops[runKey]
	if ls == nil {
		ls = &loopState{}
		f.loops[runKey] = ls
	}
	ls.lastTouch = now
	histLen := f.limits.LoopThreshold * 2
	if histLen < 8 {
		histLen = 8
	}
	ls.recent = append(ls.recent, promptHash)
	if len(ls.recent) > histLen {
		ls.recent = ls.recent[len(ls.recent)-histLen:]
	}
	count := 0
	for _, h := range ls.recent {
		if h == promptHash {
			count++
		}
	}
	return count >= f.limits.LoopThreshold
}

// gcLocked evicts idle scopes/loops and expired kills, bounded so it's cheap:
// it runs at most every gcInterval, or immediately if the scope map is oversized.
func (f *Firewall) gcLocked(now time.Time) {
	if len(f.scopes) < defaultMaxScopes && now.Sub(f.lastGC) < gcInterval {
		return
	}
	f.lastGC = now
	for k, st := range f.scopes {
		usd, toks := st.window.sum(now.Unix())
		if st.inflight == 0 && usd == 0 && toks == 0 && now.Sub(st.lastTouch) > idleEvictTTL {
			delete(f.scopes, k)
		}
	}
	for k, ls := range f.loops {
		if now.Sub(ls.lastTouch) > idleEvictTTL {
			delete(f.loops, k)
		}
	}
	// Actively sweep expired local kills so abandoned run ids are reclaimed even
	// if never read again. Shared (Redis) kills self-expire via TTL.
	f.localKills.gc(now)
}

// window is a rolling per-second ring covering windowSeconds of spend.
type window struct {
	usd      []float64
	toks     []int
	size     int
	lastSlot int64
	primed   bool
}

func newWindow(seconds int) *window {
	return &window{usd: make([]float64, seconds), toks: make([]int, seconds), size: seconds}
}

// advance clears buckets for seconds elapsed since the last write. nowSec is
// clamped to be non-decreasing so a backward clock step cannot fold spend into a
// live slot.
func (w *window) advance(nowSec int64) int64 {
	if !w.primed {
		w.primed = true
		w.lastSlot = nowSec
		return nowSec
	}
	if nowSec <= w.lastSlot {
		return w.lastSlot // clamp: ignore backward time
	}
	gap := nowSec - w.lastSlot
	if gap >= int64(w.size) {
		for i := range w.usd {
			w.usd[i] = 0
			w.toks[i] = 0
		}
	} else {
		for s := w.lastSlot + 1; s <= nowSec; s++ {
			idx := int(s % int64(w.size))
			w.usd[idx] = 0
			w.toks[idx] = 0
		}
	}
	w.lastSlot = nowSec
	return nowSec
}

func (w *window) add(nowSec int64, usd float64, tok int) {
	sec := w.advance(nowSec)
	idx := int(sec % int64(w.size))
	w.usd[idx] += usd
	w.toks[idx] += tok
	if w.usd[idx] < 0 {
		w.usd[idx] = 0
	}
	if w.toks[idx] < 0 {
		w.toks[idx] = 0
	}
}

func (w *window) sum(nowSec int64) (float64, int) {
	w.advance(nowSec)
	var u float64
	var t int
	for i := range w.usd {
		u += w.usd[i]
		t += w.toks[i]
	}
	if u < 0 {
		u = 0
	}
	if t < 0 {
		t = 0
	}
	return u, t
}
