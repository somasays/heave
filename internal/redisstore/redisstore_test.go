package redisstore

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestStore stands up an in-process Redis (miniredis) so these tests are
// hermetic — no external server, no network — while exercising the real go-redis
// code path. Returns the store and the server (for TTL fast-forwarding).
func newTestStore(t *testing.T, ttl time.Duration) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), ttl), mr
}

func mustKill(t *testing.T, s *Store, runKey string) {
	t.Helper()
	if err := s.Kill(runKey); err != nil {
		t.Fatalf("Kill(%q): %v", runKey, err)
	}
}

func TestKillAndKilled(t *testing.T) {
	s, _ := newTestStore(t, time.Hour)
	if s.Killed("run:teamA\x00r1") {
		t.Fatal("fresh run must not be killed")
	}
	mustKill(t, s, "run:teamA\x00r1")
	if !s.Killed("run:teamA\x00r1") {
		t.Fatal("run should be killed after Kill")
	}
	// A different run (different owner-namespaced key) is unaffected.
	if s.Killed("run:teamB\x00r1") {
		t.Fatal("other run must be unaffected (no cross-tenant kill)")
	}
}

func TestKillExpiresAfterTTL(t *testing.T) {
	s, mr := newTestStore(t, time.Minute)
	mustKill(t, s, "run:x")
	if !s.Killed("run:x") {
		t.Fatal("should be killed immediately")
	}
	mr.FastForward(2 * time.Minute) // past the 1-minute TTL
	if s.Killed("run:x") {
		t.Fatal("kill should expire after its TTL")
	}
}

func TestKilledFailsOpenWhenRedisDown(t *testing.T) {
	s, mr := newTestStore(t, time.Hour)
	mustKill(t, s, "run:y")
	mr.Close() // Redis is now unreachable
	// Fail open: a Redis error must not report a run as killed (availability).
	if s.Killed("run:y") {
		t.Fatal("must fail open (return false) when Redis is unreachable")
	}
}

// TestKillReturnsErrorWhenRedisDown is the write-side of fail-open: a failed
// Kill write must SURFACE (non-nil error) so the caller doesn't report a false
// success. This is what lets the kill endpoint return 5xx instead of a lying 200.
func TestKillReturnsErrorWhenRedisDown(t *testing.T) {
	s, mr := newTestStore(t, time.Hour)
	mr.Close()
	if err := s.Kill("run:z"); err == nil {
		t.Fatal("Kill must return an error when the write cannot be persisted")
	}
}

// TestKillIsVisibleAcrossInstances is the whole point of the shared store: a kill
// written by one gateway replica must be seen by another pointed at the same
// Redis. Two independent Store values over one miniredis stand in for two
// replicas.
func TestKillIsVisibleAcrossInstances(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	replicaA := NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), time.Hour)
	replicaB := NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), time.Hour)

	if replicaB.Killed("run:shared\x00r1") {
		t.Fatal("run must not be killed before any Kill")
	}
	mustKill(t, replicaA, "run:shared\x00r1") // killed on A
	if !replicaB.Killed("run:shared\x00r1") { // seen on B
		t.Fatal("a kill on one replica must be visible on another sharing Redis")
	}
}
