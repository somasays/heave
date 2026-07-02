package redisstore

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// scopeEnv builds a store over a fresh miniredis with a fixed injectable clock.
func scopeEnv(t *testing.T, nowSec int64) (*Store, *miniredis.Miniredis, *int64) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	s := NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), 0)
	clk := nowSec
	s.SetClock(func() int64 { return clk })
	return s, mr, &clk
}

// reserveUSD is a one-scope velocity helper.
func reserveUSD(s *Store, scope string, cap, est float64, hold string) (bool, string) {
	adm, name, _, _ := s.Reserve([]string{scope}, []string{"key"}, []float64{cap}, []int{0}, []int{0}, est, 0, hold)
	return adm, name
}

func TestScopeVelocityReservesAndCaps(t *testing.T) {
	s, _, _ := scopeEnv(t, 1000)
	if adm, _ := reserveUSD(s, "A", 1.0, 0.4, "h1"); !adm {
		t.Fatal("first 0.4 must admit")
	}
	if adm, _ := reserveUSD(s, "A", 1.0, 0.4, "h2"); !adm {
		t.Fatal("second 0.4 must admit (0.8 <= 1.0)")
	}
	if adm, _ := reserveUSD(s, "A", 1.0, 0.4, "h3"); adm {
		t.Fatal("third 0.4 must be denied (1.2 > 1.0)")
	}
}

func TestScopeSettleReconciles(t *testing.T) {
	s, _, _ := scopeEnv(t, 1000)
	reserveUSD(s, "A", 1.0, 0.9, "h1") // reserve 0.9
	if err := s.Settle([]string{"A"}, -0.7, 0); err != nil {
		t.Fatal(err) // actual was only 0.2 → window should hold 0.2
	}
	if adm, _ := reserveUSD(s, "A", 1.0, 0.7, "h2"); !adm {
		t.Fatal("after settling down to 0.2, a 0.7 reserve must admit (0.9 <= 1.0)")
	}
}

func TestScopeReleaseFreesUnsettledReservation(t *testing.T) {
	s, _, _ := scopeEnv(t, 1000)
	reserveUSD(s, "A", 1.0, 0.9, "h1")
	if err := s.Release([]string{"A"}, "h1", 0.9, 0, false); err != nil {
		t.Fatal(err) // failed request → releases the 0.9 hold
	}
	if adm, _ := reserveUSD(s, "A", 1.0, 0.9, "h2"); !adm {
		t.Fatal("released reservation must free the window")
	}
}

func TestScopeConcurrencyCapAndRelease(t *testing.T) {
	s, _, _ := scopeEnv(t, 1000)
	res := func(hold string) bool {
		adm, _, _, _ := s.Reserve([]string{"A"}, []string{"key"}, []float64{0}, []int{0}, []int{2}, 0, 0, hold)
		return adm
	}
	if !res("h1") || !res("h2") {
		t.Fatal("two concurrent holds must fit under cap 2")
	}
	if res("h3") {
		t.Fatal("third concurrent hold must be denied")
	}
	if err := s.Release([]string{"A"}, "h1", 0, 0, true); err != nil {
		t.Fatal(err)
	}
	if !res("h4") {
		t.Fatal("after releasing one hold, a new one must fit")
	}
}

func TestScopeConcurrencyHoldExpiresOnCrash(t *testing.T) {
	s, _, clk := scopeEnv(t, 1000)
	res := func(hold string) bool {
		adm, _, _, _ := s.Reserve([]string{"A"}, []string{"key"}, []float64{0}, []int{0}, []int{1}, 0, 0, hold)
		return adm
	}
	if !res("h1") {
		t.Fatal("first hold must admit")
	}
	if res("h2") {
		t.Fatal("second must be denied at cap 1")
	}
	// Simulate the replica holding h1 crashing without releasing: time passes the
	// hold TTL, and the next reserve reaps it.
	*clk += holdTTLSecs + 1
	if !res("h3") {
		t.Fatal("an expired (crashed) hold must be reaped so a new request admits")
	}
}

func TestScopeVelocityWindowDrains(t *testing.T) {
	s, _, clk := scopeEnv(t, 1000)
	reserveUSD(s, "A", 1.0, 0.6, "h1")
	if adm, _ := reserveUSD(s, "A", 1.0, 0.6, "h2"); adm {
		t.Fatal("1.2 must be denied within the window")
	}
	*clk += windowSecs + 1 // the earlier reservation ages out of the window
	if adm, _ := reserveUSD(s, "A", 1.0, 0.6, "h3"); !adm {
		t.Fatal("after the window drains, a reserve must admit again")
	}
}

// TestScopeAllOrNothing: if ANY scope is over cap, NOTHING is reserved.
func TestScopeAllOrNothing(t *testing.T) {
	s, _, _ := scopeEnv(t, 1000)
	// Fill scope A to 0.8 (cap 1.0); B is empty (cap 10).
	reserveUSD(s, "A", 1.0, 0.8, "h1")
	adm, name, _, _ := s.Reserve([]string{"A", "B"}, []string{"key", "run"},
		[]float64{1.0, 10.0}, []int{0, 0}, []int{0, 0}, 0.4, 0, "h2")
	if adm || name != "key" {
		t.Fatalf("must be denied on scope A (key), got admitted=%v name=%q", adm, name)
	}
	// B must NOT have been reserved by the denied call: a big reserve on B alone
	// still admits.
	if adm2, _ := reserveUSD(s, "B", 10.0, 9.9, "h3"); !adm2 {
		t.Fatal("scope B must be untouched by the denied all-or-nothing reserve")
	}
}

// TestScopeVelocityIsSharedAcrossInstances is the whole point (ADR 0002): two
// Store instances (two replicas) over one Redis honor a SINGLE shared cap.
func TestScopeVelocityIsSharedAcrossInstances(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	mk := func() *Store {
		s := NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), 0)
		s.SetClock(func() int64 { return 1000 })
		return s
	}
	replicaA, replicaB := mk(), mk()
	if adm, _ := reserveUSD(replicaA, "team", 1.0, 0.8, "a1"); !adm {
		t.Fatal("replica A reserve must admit")
	}
	// Replica B sees A's reservation: 0.8 + 0.4 = 1.2 > 1.0 → denied.
	if adm, _ := reserveUSD(replicaB, "team", 1.0, 0.4, "b1"); adm {
		t.Fatal("replica B must see replica A's reservation (shared cap, not N×)")
	}
}

func TestScopeTokenVelocity(t *testing.T) {
	s, _, _ := scopeEnv(t, 1000)
	res := func(estTok int, hold string) bool {
		adm, _, _, _ := s.Reserve([]string{"A"}, []string{"key"}, []float64{0}, []int{10}, []int{0}, 0, estTok, hold)
		return adm
	}
	if !res(4, "h1") || !res(4, "h2") {
		t.Fatal("8 tokens must fit under cap 10")
	}
	if res(4, "h3") {
		t.Fatal("12 tokens must be denied (> 10)")
	}
	if err := s.Settle([]string{"A"}, 0, -2); err != nil { // actual fewer tokens
		t.Fatal(err)
	}
	if !res(4, "h4") { // 6 + 4 = 10 <= 10
		t.Fatal("after settling tokens down, a reserve must admit")
	}
}

// TestScopeMixedVelocityAndConcurrencyAllOrNothing: a mixed reserve where one
// scope breaches VELOCITY and another CONCURRENCY is all-or-nothing.
func TestScopeMixedVelocityAndConcurrencyAllOrNothing(t *testing.T) {
	s, _, _ := scopeEnv(t, 1000)
	reserveUSD(s, "A", 1.0, 0.8, "a1") // A near its velocity cap
	// B at its concurrency cap (1).
	if _, _, _, err := s.Reserve([]string{"B"}, []string{"run"}, []float64{0}, []int{0}, []int{1}, 0, 0, "b1"); err != nil {
		t.Fatal(err)
	}
	// Reserve on [A(vel ok), B(conc full)] est 0.1 → deny on B's concurrency.
	adm, name, kind, _ := s.Reserve([]string{"A", "B"}, []string{"key", "run"},
		[]float64{1.0, 0}, []int{0, 0}, []int{0, 1}, 0.1, 0, "x1")
	if adm || name != "run" || kind != "concurrency" {
		t.Fatalf("must deny on B concurrency, got adm=%v name=%q kind=%q", adm, name, kind)
	}
	// A must be untouched by the denied reserve.
	if a, _ := reserveUSD(s, "A", 1.0, 0.15, "a2"); !a {
		t.Fatal("scope A must be untouched by the denied mixed reserve (0.8+0.15 <= 1.0)")
	}
}

func TestSetHoldTTLFloor(t *testing.T) {
	s, _, _ := scopeEnv(t, 1000)
	s.SetHoldTTL(60) // below the default floor → ignored (never reap a live hold early)
	if s.holdTTL != holdTTLSecs {
		t.Fatalf("below-floor holdTTL must be ignored, got %d", s.holdTTL)
	}
	s.SetHoldTTL(600)
	if s.holdTTL != 600 {
		t.Fatalf("above-floor holdTTL must apply, got %d", s.holdTTL)
	}
}

// TestScopeReserveFailsOpen: a Redis error must admit (availability) and surface.
func TestScopeReserveFailsOpen(t *testing.T) {
	s, mr, _ := scopeEnv(t, 1000)
	mr.Close()
	adm, _, _, err := s.Reserve([]string{"A"}, []string{"key"}, []float64{0.01}, []int{0}, []int{0}, 999.0, 0, "h1")
	if !adm || err == nil {
		t.Fatalf("a Redis outage must fail OPEN (admit) and surface the error; adm=%v err=%v", adm, err)
	}
}
