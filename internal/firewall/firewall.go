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
// State is in-memory and PER-INSTANCE: with N replicas the effective ceiling is
// N× each cap and a kill on one replica does not stop the run on the others.
// The guarantees hold within one instance; a shared store (Phase 2R) makes them
// hold across replicas. Maps are size-capped and idle-swept so the enforcement
// point cannot be OOM'd by rotating run ids.
package firewall

import (
	"errors"
	"sync"
	"time"
)

const (
	windowSeconds = 60
	// eviction bounds so a caller rotating run ids cannot grow memory unbounded.
	defaultMaxScopes = 50_000
	gcInterval       = 30 * time.Second
	idleEvictTTL     = 2 * windowSeconds * time.Second
	killedTTL        = time.Hour
)

// ErrKilled means the run has been killed (manually or by loop detection).
var ErrKilled = errors.New("run killed")

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
}

// Firewall enforces Limits across keys and runs.
type Firewall struct {
	enabled bool
	limits  Limits
	now     func() time.Time

	mu     sync.Mutex
	scopes map[string]*scopeState
	killed map[string]time.Time // composite run key -> killed-at
	loops  map[string]*loopState
	lastGC time.Time
}

type scopeState struct {
	window    *window
	inflight  int
	lastTouch time.Time
}

type loopState struct {
	recent    []string
	lastTouch time.Time
}

// New builds a Firewall. When enabled is false, Enter returns an inert Ticket
// that admits everything. now may be nil (time.Now); injectable for tests.
func New(enabled bool, limits Limits, now func() time.Time) *Firewall {
	if now == nil {
		now = time.Now
	}
	return &Firewall{
		enabled: enabled,
		limits:  limits,
		now:     now,
		scopes:  map[string]*scopeState{},
		killed:  map[string]time.Time{},
		loops:   map[string]*loopState{},
	}
}

// Enabled reports whether the firewall enforces anything.
func (f *Firewall) Enabled() bool { return f.enabled }

// Ticket is a held reservation. Release MUST be called on every path (defer);
// Settle is called once on a successful vendor response to reconcile the held
// estimate to the actual spend.
type Ticket struct {
	fw        *Firewall
	scopeKeys []string
	estUSD    float64
	estTokens int
	released  bool
	settled   bool
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
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()
	f.gcLocked(now)

	runKey := f.runKey(key, runID)

	if runKey != "" {
		if _, ok := f.killed[runKey]; ok {
			return nil, ErrKilled
		}
		if f.limits.LoopThreshold > 0 && promptHash != "" && f.tripLoopLocked(runKey, promptHash, now) {
			f.killed[runKey] = now
			return nil, ErrKilled
		}
	}

	keys := f.scopeKeys(key, runID)
	// Check every scope first (all-or-nothing), then reserve.
	for _, sk := range keys {
		if verr := f.checkVelocityLocked(sk, estUSD, estTokens, now); verr != nil {
			return nil, verr
		}
	}
	if f.limits.MaxConcurrent > 0 {
		for _, sk := range keys {
			if st := f.scopes[sk.mapKey]; st != nil && st.inflight >= f.limits.MaxConcurrent {
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
	return &Ticket{fw: f, scopeKeys: mapKeys, estUSD: estUSD, estTokens: estTokens}, nil
}

// Settle reconciles the held estimate to the actual spend (delta into the same
// windows). Call once after a successful response.
func (tk *Ticket) Settle(actualUSD float64, actualTokens int) {
	if tk.fw == nil {
		return
	}
	tk.fw.mu.Lock()
	defer tk.fw.mu.Unlock()
	if tk.settled {
		return
	}
	tk.settled = true
	now := tk.fw.now().Unix()
	for _, mk := range tk.scopeKeys {
		if st := tk.fw.scopes[mk]; st != nil {
			st.window.add(now, actualUSD-tk.estUSD, actualTokens-tk.estTokens)
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
}

// Kill hard-stops a run owned by ownerKey. A client can only kill its own runs.
func (f *Firewall) Kill(ownerKey, runID string) {
	if runID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed[f.runKey(ownerKey, runID)] = f.now()
}

// Killed reports whether a run (owned by ownerKey) has been killed.
func (f *Firewall) Killed(ownerKey, runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.killed[f.runKey(ownerKey, runID)]
	return ok
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
func (f *Firewall) runKey(ownerKey, runID string) string {
	if runID == "" {
		return ""
	}
	return "run:" + ownerKey + "\x00" + runID
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
	for k, t := range f.killed {
		if now.Sub(t) > killedTTL {
			delete(f.killed, k)
		}
	}
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
