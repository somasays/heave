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
}

// KillStore is a shared run-kill store (e.g. Redis). Kill returns an error so a
// failed write can be surfaced (a kill switch must not silently no-op). Killed
// reports whether a run is killed; implementations should fail open (return
// false on error) so a store outage doesn't block all traffic.
type KillStore interface {
	Kill(runKey string) error
	Killed(runKey string) bool
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
	scopeKeys []string
	// runScopeKey is the run scope's map key when a per-run budget is tracked
	// ("" otherwise), so Settle/Release can reconcile the cumulative runUSD.
	runScopeKey string
	estUSD      float64
	estTokens   int
	released    bool
	settled     bool
}

// Enter is the pre-vendor gate: kill/loop checks, then a velocity + concurrency
// check-and-RESERVE for the key and run scopes (both under one lock, so
// concurrent callers see each other's holds). key is the authenticated client
// (may be ""); runID is the agent run (may be "" → run scope skipped); est* is
// the request's upper-bound cost/token estimate, held until Settle.
func (f *Firewall) Enter(key, runID, promptHash string, estUSD float64, estTokens int) (*Ticket, error) {
	if !f.enabled {
		return &Ticket{}, nil
	}
	runKey := f.runKey(key, runID)

	// Kill check is OUTSIDE f.mu: the store may do network I/O (Redis), which
	// must never be held under the global firewall lock.
	if f.isKilled(runKey) {
		return nil, ErrKilled
	}

	ticket, tripped, err := f.enterLocked(key, runID, runKey, promptHash, estUSD, estTokens)
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
func (f *Firewall) enterLocked(key, runID, runKey, promptHash string, estUSD float64, estTokens int) (tk *Ticket, tripped bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.gcLocked(now)

	if runKey != "" && f.limits.LoopThreshold > 0 && promptHash != "" &&
		f.tripLoopLocked(runKey, promptHash, now) {
		return nil, true, nil
	}

	// Per-run cumulative budget: if this request would push the run over its hard
	// $ cap, KILL the run (trip) — this is the backstop for runaways whose prompts
	// keep changing, which loop detection cannot see.
	if runKey != "" && f.limits.MaxUSDPerRun > 0 {
		spent := 0.0
		if st := f.scopes[runKey]; st != nil {
			spent = st.runUSD
		}
		if spent+estUSD > f.limits.MaxUSDPerRun {
			return nil, true, nil
		}
	}

	keys := f.scopeKeys(key, runID)
	// Check every scope first (all-or-nothing), then reserve.
	for _, sk := range keys {
		if verr := f.checkVelocityLocked(sk, estUSD, estTokens, now); verr != nil {
			return nil, false, verr
		}
	}
	if f.limits.MaxConcurrent > 0 {
		for _, sk := range keys {
			if st := f.scopes[sk.mapKey]; st != nil && st.inflight >= f.limits.MaxConcurrent {
				return nil, false, &ConcurrencyError{Scope: sk.name}
			}
		}
	}
	tk = &Ticket{fw: f, scopeKeys: make([]string, 0, len(keys)), estUSD: estUSD, estTokens: estTokens}
	for _, sk := range keys {
		st := f.ensureScopeLocked(sk.mapKey, now)
		st.window.add(now.Unix(), estUSD, estTokens) // reserve the estimate
		st.inflight++
		tk.scopeKeys = append(tk.scopeKeys, sk.mapKey)
		if sk.name == "run" && f.limits.MaxUSDPerRun > 0 {
			st.runUSD += estUSD // reserve against the cumulative per-run budget
			tk.runScopeKey = sk.mapKey
		}
	}
	return tk, false, nil
}

// Settle reconciles the held estimate to the actual spend (delta into the same
// windows). Call once after a successful response.
func (tk *Ticket) Settle(actualUSD float64, actualTokens int) {
	if tk.fw == nil {
		return
	}
	tk.fw.mu.Lock()
	defer tk.fw.mu.Unlock()
	// Settle must run before Release (the handler does; defer Release fires after).
	// Guard against a released-then-settled ordering, which would double-unwind the
	// reservation and drive runUSD negative.
	if tk.settled || tk.released {
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
}

// Release frees the concurrency slots and, if the request never settled (it
// failed), removes the reserved estimate from the windows.
func (tk *Ticket) Release() {
	if tk.fw == nil {
		return
	}
	tk.fw.mu.Lock()
	defer tk.fw.mu.Unlock()
	if tk.released {
		return
	}
	tk.released = true
	now := tk.fw.now().Unix()
	for _, mk := range tk.scopeKeys {
		st := tk.fw.scopes[mk]
		if st == nil {
			continue
		}
		if st.inflight > 0 {
			st.inflight--
		}
		if !tk.settled {
			st.window.add(now, -tk.estUSD, -tk.estTokens) // release the hold
		}
	}
	// A failed (unsettled) request releases its cumulative per-run reservation too,
	// so a run isn't charged for spend that never happened.
	if !tk.settled && tk.runScopeKey != "" {
		if st := tk.fw.scopes[tk.runScopeKey]; st != nil {
			st.runUSD -= tk.estUSD
			if st.runUSD < 0 {
				st.runUSD = 0
			}
		}
	}
}

// Kill hard-stops a run owned by ownerKey. A client can only kill its own runs.
// The returned error is the shared store's write error (nil when there is no
// shared store): a non-nil error means the kill is in effect on this replica but
// may not have propagated to others, so the caller MUST surface it rather than
// report success.
func (f *Firewall) Kill(ownerKey, runID string) error {
	if runID == "" {
		return nil
	}
	return f.doKill(f.runKey(ownerKey, runID))
}

// Killed reports whether a run (owned by ownerKey) has been killed.
func (f *Firewall) Killed(ownerKey, runID string) bool {
	if runID == "" {
		return false
	}
	return f.isKilled(f.runKey(ownerKey, runID))
}

type scopeKey struct {
	name   string // "key" or "run"
	mapKey string
}

func (f *Firewall) scopeKeys(key, runID string) []scopeKey {
	var s []scopeKey
	if key != "" {
		s = append(s, scopeKey{"key", "key:" + key})
	}
	if runID != "" {
		s = append(s, scopeKey{"run", f.runKey(key, runID)})
	}
	return s
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
	if f.limits.MaxUSDPerMin <= 0 && f.limits.MaxTokensPerMin <= 0 {
		return nil
	}
	st := f.scopes[sk.mapKey]
	if st == nil {
		return nil
	}
	usd, toks := st.window.sum(now.Unix())
	if f.limits.MaxUSDPerMin > 0 && usd+estUSD > f.limits.MaxUSDPerMin {
		return &VelocityError{Scope: sk.name, RetryAfterSec: windowSeconds}
	}
	if f.limits.MaxTokensPerMin > 0 && toks+estTokens > f.limits.MaxTokensPerMin {
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
