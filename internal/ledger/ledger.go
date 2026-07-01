// Package ledger records per-request spend. Every dispatched request MUST be
// recorded here (docs/INVARIANTS.md, Invariant #5) so cost is always
// attributable. Phase 0 emits a structured JSON log line and keeps an in-memory
// running total; Phase 3 adds a Postgres-backed durable ledger behind the same
// Record call.
package ledger

import (
	"log/slog"
	"sync"
)

// Cache token price multipliers, relative to the model's input price. Anthropic
// bills cache reads at ~0.1x and 5-minute cache writes at ~1.25x of input.
const (
	cacheReadMultiplier  = 0.1
	cacheWriteMultiplier = 1.25
)

// Record is a single billable event.
type Record struct {
	RequestID        string
	Alias            string
	Provider         string
	Upstream         string
	User             string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64
	LatencyMS        int64
	Status           string
}

// Ledger accumulates spend and emits structured records.
type Ledger struct {
	log *slog.Logger

	mu        sync.Mutex
	totalUSD  float64
	requests  int64
	totalToks int64
}

// New builds a Ledger that logs through the given slog.Logger.
func New(log *slog.Logger) *Ledger {
	return &Ledger{log: log}
}

// Record persists one billable event. It never returns an error: accounting is
// best-effort and must never fail the request path.
func (l *Ledger) Record(r Record) {
	l.mu.Lock()
	l.totalUSD += r.CostUSD
	l.requests++
	l.totalToks += int64(r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens)
	l.mu.Unlock()

	l.log.Info("request",
		"request_id", r.RequestID,
		"alias", r.Alias,
		"provider", r.Provider,
		"upstream", r.Upstream,
		"user", r.User,
		"input_tokens", r.InputTokens,
		"output_tokens", r.OutputTokens,
		"cache_read_tokens", r.CacheReadTokens,
		"cache_write_tokens", r.CacheWriteTokens,
		"cost_usd", r.CostUSD,
		"latency_ms", r.LatencyMS,
		"status", r.Status,
	)
}

// Totals returns the running aggregates, used by the /metrics endpoint.
func (l *Ledger) Totals() (requests int64, tokens int64, costUSD float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests, l.totalToks, l.totalUSD
}

// Cost computes USD spend for a request from token counts and a price table.
// Cache reads and writes are priced relative to the input rate; uncached input
// tokens are the vendor's already-cache-excluded remainder.
func Cost(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int, inputPerMTok, outputPerMTok float64) float64 {
	return float64(inputTokens)/1e6*inputPerMTok +
		float64(outputTokens)/1e6*outputPerMTok +
		float64(cacheReadTokens)/1e6*inputPerMTok*cacheReadMultiplier +
		float64(cacheWriteTokens)/1e6*inputPerMTok*cacheWriteMultiplier
}
