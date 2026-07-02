// Package health is a per-provider circuit breaker used to steer failover away
// from a provider that is currently failing. It is pure (stdlib only) with an
// injectable clock, so failover decisions are testable without real time.
//
// The breaker is simple: N consecutive failures opens it for a cooldown, during
// which the provider is considered unhealthy and skipped. After the cooldown it
// is healthy again (the next attempt is the probe); a success closes it, a
// failure re-opens it. State is in-memory and per-instance.
package health

import (
	"sync"
	"time"
)

// Tracker records per-provider success/failure and reports health.
type Tracker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu    sync.Mutex
	state map[string]*endpoint
}

type endpoint struct {
	consecutiveFails int
	openUntil        time.Time
}

// New builds a Tracker. threshold is the consecutive-failure count that opens
// the breaker (min 1); cooldown is how long it stays open. now may be nil.
func New(threshold int, cooldown time.Duration, now func() time.Time) *Tracker {
	if threshold < 1 {
		threshold = 1
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &Tracker{threshold: threshold, cooldown: cooldown, now: now, state: map[string]*endpoint{}}
}

// Healthy reports whether the provider may be tried now (breaker not open).
func (t *Tracker) Healthy(provider string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ep := t.state[provider]
	if ep == nil {
		return true
	}
	return !t.now().Before(ep.openUntil)
}

// RecordSuccess closes the breaker for the provider.
func (t *Tracker) RecordSuccess(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ep := t.state[provider]; ep != nil {
		ep.consecutiveFails = 0
		ep.openUntil = time.Time{}
	}
}

// RecordFailure increments the failure count and opens the breaker once it
// reaches the threshold.
func (t *Tracker) RecordFailure(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ep := t.state[provider]
	if ep == nil {
		ep = &endpoint{}
		t.state[provider] = ep
	}
	ep.consecutiveFails++
	if ep.consecutiveFails >= t.threshold {
		ep.openUntil = t.now().Add(t.cooldown)
	}
}
