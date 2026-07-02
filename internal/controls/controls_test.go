package controls

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
)

func hashKey(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

func TestAuthDisabledAllowsAll(t *testing.T) {
	g := New(false, nil, nil)
	c, err := g.Admit("")
	if err != nil || c != nil {
		t.Fatalf("disabled auth should allow anonymously, got c=%v err=%v", c, err)
	}
}

func TestUnknownKeyUnauthorized(t *testing.T) {
	g := New(true, []Client{{Name: "a", KeySHA256: hashKey("secret")}}, nil)
	if _, err := g.Admit("wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if _, err := g.Admit(""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty bearer should be unauthorized, got %v", err)
	}
}

func TestKnownKeyAdmitted(t *testing.T) {
	g := New(true, []Client{{Name: "team-a", KeySHA256: hashKey("secret")}}, nil)
	c, err := g.Admit("secret")
	if err != nil || c == nil || c.Name != "team-a" {
		t.Fatalf("want team-a admitted, got c=%v err=%v", c, err)
	}
}

func TestRateLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := New(true, []Client{{Name: "a", KeySHA256: hashKey("k"), RateLimitRPM: 60}}, func() time.Time { return now })
	for i := 0; i < 60; i++ {
		if _, err := g.Admit("k"); err != nil {
			t.Fatalf("req %d unexpectedly limited: %v", i, err)
		}
	}
	_, err := g.Admit("k")
	var rle *RateLimitError
	if !errors.As(err, &rle) || rle.RetryAfterSec < 1 {
		t.Fatalf("want RateLimitError with retry>=1, got %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := g.Admit("k"); err != nil {
		t.Fatalf("should be allowed after refill, got %v", err)
	}
}

func TestRateLimitZeroIsUnlimited(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := New(true, []Client{{Name: "a", KeySHA256: hashKey("k"), RateLimitRPM: 0}}, func() time.Time { return now })
	for i := 0; i < 1000; i++ {
		if _, err := g.Admit("k"); err != nil {
			t.Fatalf("rpm=0 should be unlimited, req %d got %v", i, err)
		}
	}
}

func TestBudgetReserveEnforcedAndMonthlyReset(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	g := New(true, []Client{{Name: "a", KeySHA256: hashKey("k"), MonthlyBudgetUSD: 1.0}}, clock)

	c, _ := g.Admit("k")
	r, err := g.Reserve(c, 1.0)
	if err != nil {
		t.Fatalf("first reserve should pass: %v", err)
	}
	g.Settle(r, 1.0) // spend the whole budget

	if _, err := g.Reserve(c, 0.01); !errors.As(err, new(*BudgetError)) {
		t.Fatalf("want *BudgetError, got %v", err)
	}
	var be *BudgetError
	_, err = g.Reserve(c, 0.01)
	if errors.As(err, &be) && be.RetryAfterSec < 1 {
		t.Fatalf("budget error should carry retry-after seconds, got %d", be.RetryAfterSec)
	}

	now = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) // new month resets
	if _, err := g.Reserve(c, 0.5); err != nil {
		t.Fatalf("budget should reset next month, got %v", err)
	}
}

func TestBudgetZeroIsUnlimitedButTracks(t *testing.T) {
	g := New(true, []Client{{Name: "a", KeySHA256: hashKey("k"), MonthlyBudgetUSD: 0}}, nil)
	c, _ := g.Admit("k")
	for i := 0; i < 100; i++ {
		r, err := g.Reserve(c, 1000.0)
		if err != nil {
			t.Fatalf("budget=0 should never reject, got %v", err)
		}
		g.Settle(r, 1000.0)
	}
	if got := g.byHash[hashKey("k")].spentUSD; got < 99_000 {
		t.Fatalf("unlimited budget should still track spend, got %v", got)
	}
}

func TestReserveBoundsConcurrentOvershoot(t *testing.T) {
	// The key security property: with a $10 budget and $1 reservations, no more
	// than ~10 requests may be admitted concurrently — the reserve hold prevents
	// the unbounded overshoot a naive check-then-add would allow.
	g := New(true, []Client{{Name: "a", KeySHA256: hashKey("k"), MonthlyBudgetUSD: 10.0}}, nil)
	c, _ := g.Admit("k")

	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := g.Reserve(c, 1.0); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
				g.Settle(r, 1.0)
			}
		}()
	}
	wg.Wait()
	if admitted > 10 {
		t.Fatalf("reserve should bound admits to <=10 for a $10 budget, got %d", admitted)
	}
}

func TestMultiClientIsolation(t *testing.T) {
	g := New(true, []Client{
		{Name: "a", KeySHA256: hashKey("ka"), MonthlyBudgetUSD: 1.0},
		{Name: "b", KeySHA256: hashKey("kb"), MonthlyBudgetUSD: 1.0},
	}, nil)
	ca, _ := g.Admit("ka")
	r, _ := g.Reserve(ca, 1.0)
	g.Settle(r, 1.0) // exhaust a's budget

	if _, err := g.Reserve(ca, 0.5); !errors.As(err, new(*BudgetError)) {
		t.Fatalf("a should be over budget")
	}
	cb, _ := g.Admit("kb")
	if _, err := g.Reserve(cb, 0.5); err != nil {
		t.Fatalf("b's budget must be independent of a, got %v", err)
	}
}

func TestConcurrentAdmitSettleRaceClean(t *testing.T) {
	// Exercises st.mu / b.mu under contention so -race can catch a dropped lock.
	g := New(true, []Client{{Name: "a", KeySHA256: hashKey("k")}}, nil)
	c, _ := g.Admit("k")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = g.Admit("k")
			if r, err := g.Reserve(c, 0.001); err == nil {
				g.Settle(r, 0.001)
			}
		}()
	}
	wg.Wait()
}
