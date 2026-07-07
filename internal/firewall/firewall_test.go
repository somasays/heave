package firewall

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func clockAt(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestDisabledAdmitsAll(t *testing.T) {
	f := New(false, Limits{MaxUSDPerMin: 0.01}, nil)
	tk, err := f.Enter("k", "r", "h", 100, 100)
	if err != nil {
		t.Fatalf("disabled firewall must admit, got %v", err)
	}
	tk.Settle(1, 1)
	tk.Release()
}

func TestManualKillIsOwnerScoped(t *testing.T) {
	f := New(true, Limits{}, nil)
	if err := f.Kill("teamA", "run1"); err != nil {
		t.Fatalf("local-only kill must not error, got %v", err)
	}
	if !f.Killed("teamA", "run1") {
		t.Fatal("owner's run should be killed")
	}
	if _, err := f.Enter("teamA", "run1", "", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("killed run must be rejected, got %v", err)
	}
	// A DIFFERENT owner with the same run id string is unaffected (no cross-tenant kill).
	if tk, err := f.Enter("teamB", "run1", "", 0, 0); err != nil {
		t.Fatalf("another owner's same-named run must be admitted, got %v", err)
	} else {
		tk.Release()
	}
}

func TestLoopAutoKillIncludingAlternation(t *testing.T) {
	f := New(true, Limits{LoopThreshold: 3}, nil)
	var err error
	for i := 0; i < 3; i++ {
		var tk *Ticket
		tk, err = f.Enter("k", "run", "samehash", 0, 0)
		if tk != nil {
			tk.Release()
		}
	}
	if !errors.Is(err, ErrKilled) {
		t.Fatalf("3 identical prefixes should auto-kill, got %v", err)
	}

	// Alternation A,B,A,B,A must also trip (A recurs 3× within the window).
	f2 := New(true, Limits{LoopThreshold: 3}, nil)
	seq := []string{"a", "b", "a", "b", "a"}
	for i, h := range seq {
		tk, e := f2.Enter("k", "r", h, 0, 0)
		if tk != nil {
			tk.Release()
		}
		if i == len(seq)-1 {
			if !errors.Is(e, ErrKilled) {
				t.Fatalf("A/B alternation should trip loop detection, got %v", e)
			}
		}
	}
}

func TestVelocityReservedAndDrains(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := New(true, Limits{MaxUSDPerMin: 1.0}, clockAt(&now))

	// Reserve $0.90 by ENTERING (not settling) — the hold must count immediately.
	tk, err := f.Enter("teamA", "", "", 0.90, 0)
	if err != nil {
		t.Fatalf("first request should pass: %v", err)
	}
	// A concurrent request estimated at $0.20 sees the $0.90 hold → $1.10 > $1.00.
	if _, err := f.Enter("teamA", "", "", 0.20, 0); !errors.As(err, new(*VelocityError)) {
		t.Fatalf("hold must be visible to concurrent callers (reserve, not check-only), got %v", err)
	}
	tk.Settle(0.90, 0)
	tk.Release()

	// After the window drains (>60s), allowed again.
	now = now.Add(61 * time.Second)
	if tk, err := f.Enter("teamA", "", "", 0.20, 0); err != nil {
		t.Fatalf("window should have drained, got %v", err)
	} else {
		tk.Settle(0.20, 0)
		tk.Release()
	}
}

func TestFailedRequestReleasesHold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := New(true, Limits{MaxUSDPerMin: 1.0}, clockAt(&now))
	tk, _ := f.Enter("k", "", "", 0.90, 0)
	tk.Release() // request failed → hold released, no Settle
	// The window should now be ~empty, so a $0.90 request fits.
	if tk2, err := f.Enter("k", "", "", 0.90, 0); err != nil {
		t.Fatalf("released hold should free the window, got %v", err)
	} else {
		tk2.Release()
	}
}

func TestTokenVelocityIsPreventive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := New(true, Limits{MaxTokensPerMin: 1000}, clockAt(&now))
	tk, _ := f.Enter("k", "", "", 0, 900)
	tk.Settle(0, 900)
	tk.Release()
	// Estimate 200 tokens → 900+200 > 1000, must be blocked BEFORE the vendor.
	if _, err := f.Enter("k", "", "", 0, 200); !errors.As(err, new(*VelocityError)) {
		t.Fatalf("token cap must be preventive (estimate-inclusive), got %v", err)
	}
}

func TestConcurrencyCapAndReleaseIdempotent(t *testing.T) {
	f := New(true, Limits{MaxConcurrent: 2}, nil)
	t1, err := f.Enter("k", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := f.Enter("k", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Enter("k", "", "", 0, 0); !errors.As(err, new(*ConcurrencyError)) {
		t.Fatalf("3rd concurrent should be rejected, got %v", err)
	}
	t1.Release()
	t1.Release() // idempotent — must not free a second slot
	if _, err := f.Enter("k", "", "", 0, 0); err != nil {
		t.Fatalf("exactly one slot should have freed, got %v", err)
	}
	// Only one slot freed by the double release: a further Enter must be rejected.
	if _, err := f.Enter("k", "", "", 0, 0); !errors.As(err, new(*ConcurrencyError)) {
		t.Fatalf("double release must not free two slots, got %v", err)
	}
	t2.Release()
}

func TestConcurrentEntersRaceClean(t *testing.T) {
	f := New(true, Limits{MaxUSDPerMin: 1000, MaxConcurrent: 1000, LoopThreshold: 5}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tk, err := f.Enter("k", "run", "h", 0.001, 1); err == nil {
				tk.Settle(0.001, 1)
				tk.Release()
			}
		}()
	}
	wg.Wait()
}

// TestKillExpiresAfterTTL: a kill is forgotten once its TTL passes, so a rotated
// run id does not linger in memory forever.
func TestKillExpiresAfterTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := New(true, Limits{KillTTL: time.Minute}, clockAt(&now))
	if err := f.Kill("owner", "run1"); err != nil {
		t.Fatal(err)
	}
	if !f.Killed("owner", "run1") {
		t.Fatal("run should be killed immediately")
	}
	now = now.Add(2 * time.Minute)
	if f.Killed("owner", "run1") {
		t.Fatal("kill should be forgotten after its TTL")
	}
}

// TestMemKillStoreActivelySweeps: abandoned kills are reclaimed by the periodic
// GC even when they are never read again (the leak the review flagged).
func TestMemKillStoreActivelySweeps(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newMemKillStore(clockAt(&now), time.Minute)
	if err := s.Kill("run:abandoned"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // past TTL, but nobody reads it
	s.gc(now)
	s.mu.Lock()
	n := len(s.m)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired kill should have been swept, %d remain", n)
	}
}

// fakeKillStore is a controllable shared KillStore for the layered-store tests.
type fakeKillStore struct {
	mu       sync.Mutex
	killed   map[string]bool
	failNext bool // Kill returns an error (simulate a shared-store outage)
	down     bool // Killed returns false (fail-open) as if unreachable
}

func newFakeKillStore() *fakeKillStore { return &fakeKillStore{killed: map[string]bool{}} }

func (s *fakeKillStore) Kill(runKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext {
		return errors.New("shared store down")
	}
	s.killed[runKey] = true
	return nil
}

func (s *fakeKillStore) Killed(runKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return false // fail open
	}
	return s.killed[runKey]
}

// TestSharedKillPropagates: with a shared store attached, Kill writes through to
// it (so other replicas see the kill) and a kill only present in the shared store
// still blocks Enter here.
func TestSharedKillPropagates(t *testing.T) {
	shared := newFakeKillStore()
	f := New(true, Limits{}, nil).WithKillStore(shared)

	if err := f.Kill("owner", "run1"); err != nil {
		t.Fatalf("kill should succeed, got %v", err)
	}
	shared.mu.Lock()
	_, wrote := shared.killed[f.runKey("owner", "run1")]
	shared.mu.Unlock()
	if !wrote {
		t.Fatal("kill must propagate to the shared store")
	}

	// A kill that exists ONLY in the shared store (as if issued on another
	// replica) must still block Enter on this instance.
	other := New(true, Limits{}, nil).WithKillStore(shared)
	if _, err := other.Enter("owner", "run1", "", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("a shared-store kill must block Enter on another replica, got %v", err)
	}
}

// TestKillErrorSurfacesButLocalStillHolds: when the shared write fails, Kill
// returns the error (so the endpoint can report failure) YET the run is still
// killed locally — a partition must not silently let a runaway keep spending.
func TestKillErrorSurfacesButLocalStillHolds(t *testing.T) {
	shared := newFakeKillStore()
	shared.failNext = true
	f := New(true, Limits{}, nil).WithKillStore(shared)

	if err := f.Kill("owner", "run1"); err == nil {
		t.Fatal("a failed shared write must surface as an error")
	}
	if _, err := f.Enter("owner", "run1", "", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("local kill must hold even when the shared write failed, got %v", err)
	}
}

// TestLocalKillSurvivesSharedOutage: a kill issued locally still blocks Enter
// when the shared store later goes unreachable (fail-open reads there must not
// resurrect the run on the replica that killed it).
func TestLocalKillSurvivesSharedOutage(t *testing.T) {
	shared := newFakeKillStore()
	f := New(true, Limits{}, nil).WithKillStore(shared)
	if err := f.Kill("owner", "run1"); err != nil {
		t.Fatal(err)
	}
	shared.down = true // shared store now unreachable (fail-open reads)
	if _, err := f.Enter("owner", "run1", "", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("locally-issued kill must survive a shared-store outage, got %v", err)
	}
}

// TestKillStoreRefusesWhenFullNeverEvictsLiveKill is the MF-1 regression: at the
// cap, a NEW kill is refused (surfaced) rather than evicting a still-live kill —
// so an established kill can never be silently dropped and resurrect a runaway.
func TestKillStoreRefusesWhenFullNeverEvictsLiveKill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newMemKillStore(clockAt(&now), time.Hour)
	s.cap = 2
	if err := s.Kill("run:a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Kill("run:b"); err != nil {
		t.Fatal(err)
	}
	// A new distinct run at the cap (all entries live) must be REFUSED, not admitted
	// by evicting a live kill.
	if err := s.Kill("run:c"); !errors.Is(err, ErrKillStoreFull) {
		t.Fatalf("full store must refuse a new kill, got %v", err)
	}
	// The established live kills are preserved (not resurrected).
	if !s.Killed("run:a") || !s.Killed("run:b") {
		t.Fatal("live kills must survive a refused insert")
	}
	if s.Killed("run:c") {
		t.Fatal("a refused kill must not be recorded")
	}
	// Re-killing an already-tracked run is allowed (refresh, no growth).
	if err := s.Kill("run:a"); err != nil {
		t.Fatalf("re-killing an existing run must not be refused, got %v", err)
	}
	if _, rejected := s.stats(); rejected != 1 {
		t.Fatalf("rejection must be observable, got %d", rejected)
	}
	// Once an entry expires, its slot frees and a new kill is admitted again.
	now = now.Add(2 * time.Hour)
	if err := s.Kill("run:d"); err != nil {
		t.Fatalf("after expiry a new kill must be admitted, got %v", err)
	}
}

// TestKilledRefreshesTTL: an ACTIVE killed run (still being checked) stays dead
// past the original TTL, while an abandoned kill eventually expires.
func TestKilledRefreshesTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newMemKillStore(clockAt(&now), time.Minute)
	if err := s.Kill("r"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(40 * time.Second)
	if !s.Killed("r") { // 40s < 60s: alive, and refreshes the timestamp
		t.Fatal("kill should be live at 40s")
	}
	now = now.Add(40 * time.Second) // 80s since kill, 40s since last check
	if !s.Killed("r") {
		t.Fatal("refresh-on-read must keep an actively-checked kill alive past the original TTL")
	}
	now = now.Add(2 * time.Minute) // now abandoned (not checked) past TTL
	if s.Killed("r") {
		t.Fatal("an abandoned kill must eventually expire")
	}
}

// TestLoopTripSharedErrorIsObservable is the MF/Go regression for the swallowed
// auto-kill path: when loop detection trips and the shared write fails, the run
// is still killed locally AND the propagation failure is counted (not silent).
func TestLoopTripSharedErrorIsObservable(t *testing.T) {
	shared := newFakeKillStore()
	shared.failNext = true
	f := New(true, Limits{LoopThreshold: 2}, nil).WithKillStore(shared)

	if _, err := f.Enter("k", "run", "h", 0, 0); err != nil {
		t.Fatalf("first prompt should admit, got %v", err)
	}
	if _, err := f.Enter("k", "run", "h", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("second identical prompt should trip loop auto-kill, got %v", err)
	}
	// Local auto-kill holds even though the shared propagation failed.
	if _, err := f.Enter("k", "run", "other", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("auto-killed run must stay dead locally, got %v", err)
	}
	if got := f.Stats().SharedKillErrors; got == 0 {
		t.Fatal("a failed shared write on the auto-kill path must be observable")
	}
}

// fakeScopeStore is a controllable in-memory ScopeStore for the shared-mode tests.
type fakeScopeStore struct {
	mu        sync.Mutex
	reserved  map[string]float64 // scope key -> reserved USD
	holds     map[string]int     // scope key -> live holds
	denyKind  string             // if set, Reserve denies with this kind
	failErr   error              // if set, Reserve fails (fail-open path)
	settleSum float64
	releases  int
}

func newFakeScopeStore() *fakeScopeStore {
	return &fakeScopeStore{reserved: map[string]float64{}, holds: map[string]int{}}
}

func (s *fakeScopeStore) Reserve(keys, names []string, _ []float64, _, _ []int, estUSD float64, _ int, _ string) (bool, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr != nil {
		return true, "", "", s.failErr
	}
	if s.denyKind != "" {
		return false, names[0], s.denyKind, nil
	}
	for _, k := range keys {
		s.reserved[k] += estUSD
		s.holds[k]++
	}
	return true, "", "", nil
}

func (s *fakeScopeStore) Settle(_ []string, deltaUSD float64, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleSum += deltaUSD
	return nil
}

func (s *fakeScopeStore) Release(keys []string, _ string, estUSD float64, _ int, settled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	for _, k := range keys {
		if s.holds[k] > 0 {
			s.holds[k]--
		}
		if !settled {
			s.reserved[k] -= estUSD
		}
	}
	return nil
}

// TestSharedModeDelegatesToScopeStore: with a ScopeStore, velocity/concurrency go
// to the store; Settle/Release propagate to it.
func TestSharedModeDelegatesToScopeStore(t *testing.T) {
	store := newFakeScopeStore()
	f := New(true, Limits{MaxUSDPerMin: 100}, nil).WithScopeStore(store)
	tk, err := f.Enter("k", "r", "h", 0.5, 0)
	if err != nil {
		t.Fatalf("store admits, Enter must succeed: %v", err)
	}
	if store.reserved["key:k"] != 0.5 || store.holds["key:k"] != 1 {
		t.Fatalf("Enter must reserve in the shared store, got %+v", store.reserved)
	}
	tk.Settle(0.3, 0)
	tk.Release()
	if store.settleSum != 0.3-0.5 { // reconcile delta actual-est
		t.Fatalf("Settle must reconcile to the store, got %v", store.settleSum)
	}
	if store.releases != 1 {
		t.Fatalf("Release must free the store hold, got %d", store.releases)
	}
}

// TestSharedVelocityDenialRollsBackBudget: a store velocity denial must roll back
// the local per-run budget reservation (no partial state).
func TestSharedVelocityDenialRollsBackBudget(t *testing.T) {
	store := newFakeScopeStore()
	f := New(true, Limits{MaxUSDPerMin: 100, MaxUSDPerRun: 1.0}, nil).WithScopeStore(store)
	store.denyKind = "velocity"
	if _, err := f.Enter("k", "r", "h1", 0.4, 0); !errors.As(err, new(*VelocityError)) {
		t.Fatalf("store velocity denial must surface as VelocityError, got %v", err)
	}
	// The budget reserve was rolled back: a fresh 0.9 (which would trip if 0.4 had
	// stuck) must still admit.
	store.denyKind = ""
	if tk, err := f.Enter("k", "r", "h2", 0.9, 0); err != nil {
		t.Fatalf("denied reserve must not consume the per-run budget, got %v", err)
	} else {
		tk.Release()
	}
}

// TestSharedConcurrencyDenial maps a store concurrency breach to ConcurrencyError.
func TestSharedConcurrencyDenial(t *testing.T) {
	store := newFakeScopeStore()
	store.denyKind = "concurrency"
	f := New(true, Limits{MaxConcurrent: 1}, nil).WithScopeStore(store)
	if _, err := f.Enter("k", "r", "h", 0, 0); !errors.As(err, new(*ConcurrencyError)) {
		t.Fatalf("store concurrency denial must surface as ConcurrencyError, got %v", err)
	}
}

// TestSharedFailOpenCountsDegraded: a store error admits (availability) and is
// counted so the degradation is observable.
func TestSharedFailOpenCountsDegraded(t *testing.T) {
	store := newFakeScopeStore()
	store.failErr = errors.New("redis down")
	f := New(true, Limits{MaxUSDPerMin: 0.0001}, nil).WithScopeStore(store)
	tk, err := f.Enter("k", "r", "h", 999.0, 0) // would exceed if enforced
	if err != nil {
		t.Fatalf("a store outage must fail OPEN, got %v", err)
	}
	tk.Settle(999.0, 0)
	tk.Release()
	if f.Stats().ScopeDegraded == 0 {
		t.Fatal("a failed-open admit must be observable via Stats")
	}
	// MF-1 regression: nothing was written to the store on a fail-open admit, so
	// Settle/Release must NOT touch it — otherwise they'd subtract a phantom
	// reservation and erode co-tenants' real reservations.
	if store.releases != 0 || store.settleSum != 0 {
		t.Fatalf("fail-open ticket must be a store no-op, got releases=%d settle=%v", store.releases, store.settleSum)
	}
}

// TestSharedFailOpenFallsBackToLocalEnforcement (review M3): when the shared
// store errors, the firewall enforces LOCALLY (bounded per-instance) rather than
// admitting everything — degrading to N×, never to unenforced.
func TestSharedFailOpenFallsBackToLocalEnforcement(t *testing.T) {
	store := newFakeScopeStore()
	store.failErr = errors.New("redis down")
	f := New(true, Limits{MaxUSDPerMin: 0.01}, nil).WithScopeStore(store)
	admitted := 0
	for i := 0; i < 5; i++ {
		if tk, err := f.Enter("k", "", "", 0.006, 0); err == nil {
			admitted++
			tk.Settle(0.006, 0)
			tk.Release()
		}
	}
	if admitted == 5 {
		t.Fatal("fail-open must fall back to LOCAL enforcement, not admit everything")
	}
	if admitted == 0 {
		t.Fatal("local fallback must still admit within the local cap")
	}
	if f.Stats().ScopeDegraded == 0 {
		t.Fatal("fail-open must be observable via Stats")
	}
}

// TestSharedModeLoopAndKillStayLocal: loop detection and kills still work in
// shared mode (they are local, not delegated).
func TestSharedModeLoopAndKillStayLocal(t *testing.T) {
	store := newFakeScopeStore()
	f := New(true, Limits{MaxUSDPerMin: 100, LoopThreshold: 2}, nil).WithScopeStore(store)
	if tk, err := f.Enter("k", "run", "same", 0, 0); err == nil {
		tk.Release()
	}
	if _, err := f.Enter("k", "run", "same", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("loop detection must still trip in shared mode, got %v", err)
	}
	// And a manual kill still blocks.
	f2 := New(true, Limits{MaxUSDPerMin: 100}, nil).WithScopeStore(newFakeScopeStore())
	if err := f2.Kill("k", "run2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Enter("k", "run2", "h", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("manual kill must still block in shared mode, got %v", err)
	}
}

// TestPerRunBudgetAutoKills: a hard cumulative per-run $ cap trips a kill once the
// run would exceed it — the backstop for changing-prompt runaways loop detection
// can't catch. The run never bills past the cap, and stays killed afterward.
func TestPerRunBudgetAutoKills(t *testing.T) {
	f := New(true, Limits{MaxUSDPerRun: 1.0}, nil)
	// Distinct prompt hashes each time (loop detection would NOT fire) — only the
	// cumulative budget catches this.
	for i, est := range []float64{0.4, 0.4} {
		tk, err := f.Enter("k", "run", "h"+string(rune('a'+i)), est, 0)
		if err != nil {
			t.Fatalf("call %d under budget must admit, got %v", i, err)
		}
		tk.Settle(est, 0)
		tk.Release()
	}
	// Cumulative is 0.8; a third 0.4 would reach 1.2 > 1.0 → kill.
	if _, err := f.Enter("k", "run", "hc", 0.4, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("exceeding the per-run budget must auto-kill, got %v", err)
	}
	// The run is now hard-killed: even a tiny request is refused.
	if _, err := f.Enter("k", "run", "hd", 0.001, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("a budget-killed run must stay killed, got %v", err)
	}
	// A different run is unaffected (per-run scope).
	if tk, err := f.Enter("k", "other", "he", 0.4, 0); err != nil {
		t.Fatalf("an unrelated run must be admitted, got %v", err)
	} else {
		tk.Release()
	}
}

// TestPerRunBudgetReleaseFreesReservation: a failed (unsettled) request must not
// count toward the run's cumulative budget.
func TestPerRunBudgetReleaseFreesReservation(t *testing.T) {
	f := New(true, Limits{MaxUSDPerRun: 1.0}, nil)
	tk, err := f.Enter("k", "run", "h1", 0.9, 0)
	if err != nil {
		t.Fatal(err)
	}
	tk.Release() // failed request, never settled → releases the 0.9 hold
	// The budget is free again: another 0.9 request must admit.
	if tk2, err := f.Enter("k", "run", "h2", 0.9, 0); err != nil {
		t.Fatalf("released reservation must free the per-run budget, got %v", err)
	} else {
		tk2.Release()
	}
}

// TestPerRunBudgetSettlesToActual: the cumulative budget reconciles the reserved
// estimate down to the actual spend, so an over-estimate doesn't strand budget.
func TestPerRunBudgetSettlesToActual(t *testing.T) {
	f := New(true, Limits{MaxUSDPerRun: 1.0}, nil)
	tk, err := f.Enter("k", "run", "h1", 0.9, 0) // reserve 0.9
	if err != nil {
		t.Fatal(err)
	}
	tk.Settle(0.2, 0) // actual only 0.2 → cumulative reconciles to 0.2
	tk.Release()
	// 0.8 of budget remains: a 0.7 request admits.
	if tk2, err := f.Enter("k", "run", "h2", 0.7, 0); err != nil {
		t.Fatalf("settle-to-actual must free the over-estimate, got %v", err)
	} else {
		tk2.Settle(0.7, 0)
		tk2.Release()
	}
	// Now 0.9 cumulative; a 0.2 request would exceed → kill.
	if _, err := f.Enter("k", "run", "h3", 0.2, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("cumulative should track settled actuals, got %v", err)
	}
}

// TestPerRunBudgetNeedsRunID: without a run id there is no per-run scope, so the
// budget cannot apply (documented prerequisite).
func TestPerRunBudgetNeedsRunID(t *testing.T) {
	f := New(true, Limits{MaxUSDPerRun: 0.1}, nil)
	for i := 0; i < 5; i++ {
		tk, err := f.Enter("k", "", "", 1.0, 0) // no run id, est way over the cap
		if err != nil {
			t.Fatalf("no-run-id traffic is not subject to the per-run budget, got %v", err)
		}
		tk.Settle(1.0, 0)
		tk.Release()
	}
}

// TestPerRunBudgetOvershootBoundedThenTrips is the honest limit (review MF-2): the
// cap is on the ESTIMATE, so a call that bills MORE than estimated can push a run
// over the cap — but settle keeps the accumulator accurate, so the overshoot is
// bounded to that one call and the NEXT request trips.
func TestPerRunBudgetOvershootBoundedThenTrips(t *testing.T) {
	f := New(true, Limits{MaxUSDPerRun: 1.0}, nil)
	tk, err := f.Enter("k", "run", "h1", 0.4, 0) // reserve only 0.4 …
	if err != nil {
		t.Fatal(err)
	}
	tk.Settle(1.4, 0) // … but it actually cost 1.4 (underestimate): billed over the cap
	tk.Release()
	// The overshoot does not compound: the run is now over budget, so the next
	// request is refused.
	if _, err := f.Enter("k", "run", "h2", 0.01, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("an underestimated call over the cap must trip the next request, got %v", err)
	}
}

// TestPerRunBudgetResetsAfterIdleEviction PINS the documented active-lifetime
// behavior (review MF-1): a run's cumulative resets once its scope is idle-evicted
// — this is intentional (bounded memory), and the monthly budget is the absolute
// backstop.
func TestPerRunBudgetResetsAfterIdleEviction(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := New(true, Limits{MaxUSDPerRun: 1.0}, clockAt(&now))
	tk, err := f.Enter("k", "r", "h1", 0.9, 0) // spend near the cap
	if err != nil {
		t.Fatal(err)
	}
	tk.Settle(0.9, 0)
	tk.Release()
	// Idle past the eviction window (and past gcInterval) with no activity on "r".
	now = now.Add(idleEvictTTL + gcInterval + time.Second)
	tk2, err := f.Enter("k", "other", "h2", 0.1, 0) // any Enter triggers gcLocked
	if err != nil {
		t.Fatal(err)
	}
	tk2.Release()
	// "r"'s scope was reclaimed, so its budget restarts: a fresh 0.9 admits again
	// (it would be killed if the prior 0.9 still counted).
	tk3, err := f.Enter("k", "r", "h3", 0.9, 0)
	if err != nil {
		t.Fatalf("an idle-evicted run must reset its per-run budget and re-admit, got %v", err)
	}
	tk3.Release()
}

// TestBudgetKilledRunStaysKilledAfterScopeEvicts: the KILL outlives the evictable
// scope — a budget-killed run stays dead even after its runUSD accounting is
// reclaimed (kill lives in the kill store with its own TTL).
func TestBudgetKilledRunStaysKilledAfterScopeEvicts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := New(true, Limits{MaxUSDPerRun: 1.0, KillTTL: 24 * time.Hour}, clockAt(&now))
	tk, err := f.Enter("k", "r", "h1", 0.9, 0)
	if err != nil {
		t.Fatal(err)
	}
	tk.Settle(0.9, 0)
	tk.Release()
	if _, err := f.Enter("k", "r", "h2", 0.2, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("run should be budget-killed, got %v", err)
	}
	now = now.Add(idleEvictTTL + gcInterval + time.Second)
	if tk, err := f.Enter("k", "other", "h3", 0.1, 0); err == nil { // trigger gc
		tk.Release()
	}
	if _, err := f.Enter("k", "r", "h4", 0.001, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("a budget-killed run must stay killed after its scope evicts, got %v", err)
	}
}

// TestVelocityRejectDoesNotChargeRunBudget: a request rejected by a velocity cap
// returns before the reserve loop, so it must not consume the per-run budget.
func TestVelocityRejectDoesNotChargeRunBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := New(true, Limits{MaxUSDPerMin: 0.5, MaxUSDPerRun: 1.0}, clockAt(&now))
	tk, err := f.Enter("k", "r", "h1", 0.4, 0) // within velocity + budget
	if err != nil {
		t.Fatal(err)
	}
	tk.Settle(0.4, 0)
	tk.Release()
	// Exceeds the $/min velocity cap (0.4+0.4 > 0.5) → rejected before any reserve.
	if _, err := f.Enter("k", "r", "h2", 0.4, 0); !errors.As(err, new(*VelocityError)) {
		t.Fatalf("want velocity rejection, got %v", err)
	}
	// Drain the velocity window (60s) but keep the run scope alive (< 120s idle).
	// The run budget should reflect ONLY the one settled call (0.4), so a 0.5
	// request still admits (0.9 ≤ 1.0). If the rejected call had been charged
	// (0.8), this would trip.
	now = now.Add(90 * time.Second)
	tk3, err := f.Enter("k", "r", "h3", 0.5, 0)
	if err != nil {
		t.Fatalf("a velocity-rejected call must not charge the per-run budget, got %v", err)
	}
	tk3.Release()
}

// --- EnterChain: per-scope caps across a resolved policy chain (ADR 0006) ---

// chain builds an org▸team▸app▸run chain with the given per-node caps. Distinct
// keys per scope so each has its own window/counter.
func chain(org, team, app, run Limits) []Scope {
	return []Scope{
		{Name: "org", Key: "org:acme", Limits: org},
		{Name: "team", Key: "team:eng", Limits: team},
		{Name: "app", Key: "app:bot", Limits: app},
		{Name: "run", Key: "run:app:bot\x00r1", Limits: run},
	}
}

func TestEnterChainVelocityBindsAtTightestScope(t *testing.T) {
	f := New(true, Limits{}, nil) // no global limits: caps come from the chain
	// The org caps $1/min; the app is loose ($100/min). A request that fits the
	// app must still be denied by the tighter ORG cap, and name it.
	c := chain(Limits{MaxUSDPerMin: 1.0}, Limits{}, Limits{MaxUSDPerMin: 100}, Limits{})
	tk, err := f.EnterChain(c, "h", 0.9, 0)
	if err != nil {
		t.Fatalf("0.9 must fit under the $1 org cap, got %v", err)
	}
	tk.Settle(0.9, 0)
	tk.Release()
	var ve *VelocityError
	_, err = f.EnterChain(c, "h", 0.2, 0) // 0.9 + 0.2 > 1.0 at the org
	if !errors.As(err, &ve) {
		t.Fatalf("want VelocityError, got %v", err)
	}
	if ve.Scope != "org" {
		t.Fatalf("the binding node must be the org, got %q", ve.Scope)
	}
}

func TestEnterChainConcurrencyBindsAtScope(t *testing.T) {
	f := New(true, Limits{}, nil)
	// The team allows only one in-flight; the app is unbounded. A second
	// concurrent request must be denied by the TEAM scope.
	c := chain(Limits{}, Limits{MaxConcurrent: 1}, Limits{MaxConcurrent: 100}, Limits{})
	t1, err := f.EnterChain(c, "h", 0, 0)
	if err != nil {
		t.Fatalf("first must admit, got %v", err)
	}
	var ce *ConcurrencyError
	if _, err := f.EnterChain(c, "h", 0, 0); !errors.As(err, &ce) {
		t.Fatalf("second concurrent must be denied, got %v", err)
	} else if ce.Scope != "team" {
		t.Fatalf("binding node must be the team, got %q", ce.Scope)
	}
	t1.Release() // free the hold
	if t2, err := f.EnterChain(c, "h", 0, 0); err != nil {
		t.Fatalf("after release the slot must free, got %v", err)
	} else {
		t2.Release()
	}
}

func TestEnterChainPerRunBudgetKillsRun(t *testing.T) {
	f := New(true, Limits{LoopThreshold: 0}, nil)
	// Only the run scope carries a per-run $ cap (as policy resolves the tightest
	// ancestor value onto the run). Exceeding it auto-kills the run.
	c := chain(Limits{}, Limits{}, Limits{}, Limits{MaxUSDPerRun: 0.01})
	tk, err := f.EnterChain(c, "", 0.006, 0)
	if err != nil {
		t.Fatalf("first under-budget call must admit, got %v", err)
	}
	tk.Settle(0.006, 0)
	tk.Release()
	if _, err := f.EnterChain(c, "", 0.006, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("exceeding the per-run cap must kill the run, got %v", err)
	}
	// The run stays killed for subsequent calls.
	if _, err := f.EnterChain(c, "", 0.001, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("a killed run stays dead, got %v", err)
	}
}

func TestEnterChainSettleReconcilesEveryScope(t *testing.T) {
	f := New(true, Limits{}, nil)
	// The org caps $1/min. Reserve 0.9 but settle to a real 0.1 — the org window
	// must reflect the actual, so a following 0.85 fits.
	c := chain(Limits{MaxUSDPerMin: 1.0}, Limits{}, Limits{}, Limits{})
	tk, err := f.EnterChain(c, "h", 0.9, 0)
	if err != nil {
		t.Fatalf("0.9 must admit, got %v", err)
	}
	tk.Settle(0.1, 0) // actual far below the estimate
	tk.Release()
	tk2, err := f.EnterChain(c, "h", 0.85, 0) // 0.1 + 0.85 < 1.0
	if err != nil {
		t.Fatalf("settle must have reconciled the org window down; got %v", err)
	}
	tk2.Release()
}

func TestEnterChainDisabledAdmitsAll(t *testing.T) {
	f := New(false, Limits{}, nil)
	c := chain(Limits{MaxUSDPerMin: 0.01}, Limits{}, Limits{}, Limits{MaxUSDPerRun: 0.01})
	tk, err := f.EnterChain(c, "h", 100, 100)
	if err != nil {
		t.Fatalf("disabled firewall must admit any chain, got %v", err)
	}
	tk.Release()
}

func TestEnterChainKillIsAddressableByRunKey(t *testing.T) {
	f := New(true, Limits{}, nil)
	c := chain(Limits{}, Limits{}, Limits{}, Limits{})
	runKey := c[3].Key // the "run" scope's Key — what EnterChain enforces kill under
	tk, err := f.EnterChain(c, "h", 0, 0)
	if err != nil {
		t.Fatalf("first must admit, got %v", err)
	}
	tk.Release()
	// Killing via the run scope key (as the server will, from the resolved chain)
	// must actually stop the run — the regression the review caught (Kill/EnterChain
	// keyed off different strings).
	if err := f.KillRun(runKey); err != nil {
		t.Fatalf("KillRun must not error, got %v", err)
	}
	if !f.RunKilled(runKey) {
		t.Fatal("RunKilled must see the kill")
	}
	if _, err := f.EnterChain(c, "h", 0, 0); !errors.Is(err, ErrKilled) {
		t.Fatalf("a run killed by its scope key must be rejected on re-entry, got %v", err)
	}
}

func TestEnterChainFailsClosedOnMalformedChain(t *testing.T) {
	f := New(true, Limits{}, nil)
	// Empty-keyed scope: would collide distinct runs on a "" counter and disable
	// kill/budget — must fail closed, not admit.
	bad := []Scope{{Name: "org", Key: "", Limits: Limits{}}}
	if _, err := f.EnterChain(bad, "h", 0, 0); !errors.Is(err, ErrBadChain) {
		t.Fatalf("empty-keyed scope must be rejected, got %v", err)
	}
	// Two "run" scopes: ambiguous which drives kill/budget — reject.
	twoRuns := []Scope{
		{Name: "run", Key: "run:a\x001", Limits: Limits{}},
		{Name: "run", Key: "run:a\x002", Limits: Limits{}},
	}
	if _, err := f.EnterChain(twoRuns, "h", 0, 0); !errors.Is(err, ErrBadChain) {
		t.Fatalf("multiple run scopes must be rejected, got %v", err)
	}
}

func TestEnterChainNegativeEstimateCannotDeflateCounter(t *testing.T) {
	f := New(true, Limits{}, nil)
	c := chain(Limits{MaxUSDPerMin: 1.0}, Limits{}, Limits{}, Limits{})
	t1, err := f.EnterChain(c, "h", 0.9, 0) // hold 0.9 in the org window
	if err != nil {
		t.Fatalf("0.9 must admit, got %v", err)
	}
	// A negative estimate must NOT subtract from the live 0.9 (that would be spend
	// evasion). Clamped to 0, so the window stays at 0.9.
	neg, err := f.EnterChain(c, "h", -0.5, 0)
	if err != nil {
		t.Fatalf("a zero-clamped estimate still admits, got %v", err)
	}
	neg.Release()
	// If the negative had deflated the counter to 0.4, this 0.2 would pass; with the
	// clamp it must be denied (0.9 + 0.2 > 1.0).
	if _, err := f.EnterChain(c, "h", 0.2, 0); !errors.As(err, new(*VelocityError)) {
		t.Fatalf("negative estimate must not have deflated the org counter, got %v", err)
	}
	t1.Release()
}

// --- Reservation: settle/release a held reserve WITHOUT the original ticket (ADR 0007) ---

func TestReservationSettleReconcilesStatelessly(t *testing.T) {
	f := New(true, Limits{}, nil)
	c := chain(Limits{MaxUSDPerMin: 1.0}, Limits{}, Limits{}, Limits{})
	tk, err := f.EnterChain(c, "h", 0.9, 0) // hold 0.9 in the org window
	if err != nil {
		t.Fatalf("0.9 must admit, got %v", err)
	}
	resv := tk.Reservation() // capture reconcile state; do NOT release inline
	// A different request sees the 0.9 hold: 0.9 + 0.2 > 1.0 → denied.
	if _, err := f.EnterChain(c, "h", 0.2, 0); !errors.As(err, new(*VelocityError)) {
		t.Fatalf("the held reserve must bind other requests, got %v", err)
	}
	// Settle the reservation to a real 0.1 (no original ticket used) → window drops.
	f.SettleReservation(resv, 0.1, 0)
	if tk2, err := f.EnterChain(c, "h", 0.85, 0); err != nil { // 0.1 + 0.85 < 1.0
		t.Fatalf("after stateless settle the window must reflect the actual 0.1, got %v", err)
	} else {
		tk2.Release()
	}
}

func TestReservationReleaseFreesHold(t *testing.T) {
	f := New(true, Limits{}, nil)
	c := chain(Limits{MaxUSDPerMin: 1.0}, Limits{}, Limits{}, Limits{})
	tk, _ := f.EnterChain(c, "h", 0.9, 0)
	resv := tk.Reservation()
	f.ReleaseReservation(resv)                                 // the call never billed → fully unwind the hold
	if tk2, err := f.EnterChain(c, "h", 0.95, 0); err != nil { // 0 + 0.95 < 1.0
		t.Fatalf("release must free the whole hold, got %v", err)
	} else {
		tk2.Release()
	}
}
