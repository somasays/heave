// Package cache tracks, per conversation, which model's prefix cache is
// currently warm — the state the cache-aware router needs to keep a conversation
// on its warm model and only re-route once the cache TTL lapses (the project's
// wedge; docs/INVARIANTS.md, Invariant #3).
//
// It is pure (stdlib only) with an injectable clock. State is in-memory and
// per-instance for now — the SPIKE deliberately proves the routing thesis on a
// single instance before the shared (Redis) store that org-scale needs. See
// docs/BENCHMARK.md.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Store maps a conversation key to the model whose cache was last warmed and
// when. A model is "warm" for a conversation if it was used within the TTL.
type Store struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]entry
}

type entry struct {
	model    string
	lastSeen time.Time
}

// New builds a Store with the given prefix-cache TTL. now may be nil (time.Now)
// and is injectable for deterministic tests/benchmarks.
func New(ttl time.Duration, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{ttl: ttl, now: now, m: map[string]entry{}}
}

// Warm returns the model pinned to a conversation if its cache is still warm
// (used within the TTL). When cold or unknown, ok is false and the router is
// free to (re-)select a model.
func (s *Store) Warm(convKey string) (model string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, found := s.m[convKey]
	if !found {
		return "", false
	}
	if s.now().Sub(e.lastSeen) > s.ttl {
		return "", false // TTL lapsed: cache is cold, safe to re-route
	}
	return e.model, true
}

// Touch records that a conversation was just served by model, refreshing its
// warmth window.
func (s *Store) Touch(convKey, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[convKey] = entry{model: model, lastSeen: s.now()}
}

// ConversationKey derives a stable identity for a conversation from the parts of
// the prompt that persist across turns: the system prompt and every message
// except the latest user turn. Because the provider's prefix cache keys on the
// stable leading prefix, hashing that same prefix groups a conversation's turns
// together without needing a client-supplied id.
func ConversationKey(system string, priorMessages []string) string {
	h := sha256.New()
	h.Write([]byte(system))
	h.Write([]byte{0})
	for _, m := range priorMessages {
		h.Write([]byte(m))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
