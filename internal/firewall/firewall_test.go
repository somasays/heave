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
