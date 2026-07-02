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
	f.Kill("teamA", "run1")
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
