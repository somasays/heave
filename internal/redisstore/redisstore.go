// Package redisstore is a Redis-backed shared state store for heave. Its first
// primitive is a shared run-kill store: a run killed on ANY gateway replica is
// honored by all of them (docs/INVARIANTS.md, Invariant #9 — removes the
// per-instance limitation for kills). It implements firewall.KillStore
// structurally, so the firewall package stays pure (no Redis import).
//
// Kills self-expire via Redis key TTL. Reads FAIL OPEN (a Redis error means "we
// can't confirm a kill", so the request is allowed) — availability over a
// false-positive block; the enforcement point never blocks all traffic because
// Redis blipped.
package redisstore

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const opTimeout = 500 * time.Millisecond

// fallbackTTL guards against a caller passing ttl <= 0, which Redis would treat
// as "never expire" — a leaked kill key that outlives its run forever.
const fallbackTTL = 24 * time.Hour

// Store is a Redis-backed shared store: run-kill state (KillStore) plus
// cross-replica velocity/concurrency reserve/settle (see scopestore.go).
type Store struct {
	rdb *redis.Client
	ttl time.Duration
	// now overrides the scope-store clock (unix seconds) for tests; nil = wall.
	now func() int64
	// holdTTL bounds a concurrency hold's lifetime (seconds) before it is reaped
	// as a crashed-replica leak; MUST exceed the longest request so a live hold is
	// never reaped. Set from request_timeout by the composition root.
	holdTTL int
	// Scope-store Lua scripts, compiled once (EVALSHA-cached by go-redis).
	reserveScript     *redis.Script
	adjustScript      *redis.Script
	releaseConcScript *redis.Script
}

// SetHoldTTL sets the concurrency-hold lifetime (seconds). Values below the
// default floor are ignored, so a live request's hold is never reaped early.
func (s *Store) SetHoldTTL(seconds int) {
	if seconds > holdTTLSecs {
		s.holdTTL = seconds
	}
}

// SetClock injects the scope-store clock (unix seconds). Test-only; the wall
// clock is used when unset.
func (s *Store) SetClock(now func() int64) { s.now = now }

// New connects to Redis from a URL (redis://host:port/db) and returns a Store
// whose kills expire after ttl.
func New(url string, ttl time.Duration) (*Store, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	return NewClient(redis.NewClient(opt), ttl), nil
}

// NewClient wraps an existing client (used by tests with an in-process server).
func NewClient(rdb *redis.Client, ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = fallbackTTL // never persist kill keys forever
	}
	return &Store{
		rdb: rdb, ttl: ttl, holdTTL: holdTTLSecs,
		reserveScript:     redis.NewScript(reserveSrc),
		adjustScript:      redis.NewScript(adjustSrc),
		releaseConcScript: redis.NewScript(releaseConcSrc),
	}
}

func killKey(runKey string) string { return "heave:kill:" + runKey }

// Kill marks a run killed for ttl. It returns the write error so the caller can
// surface a failed kill: a kill switch that silently no-ops would report success
// while the run keeps spending. (The firewall's local store still records the
// kill for this replica, so enforcement here holds regardless.)
func (s *Store) Kill(runKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := s.rdb.Set(ctx, killKey(runKey), "1", s.ttl).Err(); err != nil {
		return fmt.Errorf("redis set kill %q: %w", runKey, err)
	}
	return nil
}

// Killed reports whether a run is killed. FAIL OPEN: on a Redis error it returns
// false so a transient outage never blocks all traffic.
func (s *Store) Killed(runKey string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	n, err := s.rdb.Exists(ctx, killKey(runKey)).Result()
	return err == nil && n > 0
}

// Ping verifies connectivity (used at startup to fail fast on misconfiguration).
func (s *Store) Ping(ctx context.Context) error { return s.rdb.Ping(ctx).Err() }

// Close releases the underlying client.
func (s *Store) Close() error { return s.rdb.Close() }
