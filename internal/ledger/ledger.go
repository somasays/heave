// Package ledger records per-request spend. Every dispatched request MUST be
// recorded here (docs/INVARIANTS.md, Invariant #5) so cost is always
// attributable. It emits a structured JSON log line and keeps in-memory
// aggregates — grand totals plus spend attributed by client and by agent run —
// and a bounded ring of recent events for the built-in dashboard. A Postgres
// durable sink behind the same Record call is a tracked follow-up.
package ledger

import (
	"log/slog"
	"sort"
	"sync"
)

// Cache token price multipliers, relative to the model's input price. Anthropic
// bills cache reads at ~0.1x and 5-minute cache writes at ~1.25x of input.
const (
	cacheReadMultiplier  = 0.1
	cacheWriteMultiplier = 1.25
)

const (
	// maxTracked bounds the by-client and by-run maps so a caller rotating run ids
	// cannot OOM the ledger; spend for keys past the cap folds into an overflow
	// bucket so the per-dimension sums still reconcile to the grand total.
	maxTracked = 20_000
	// Sentinel bucket keys are NUL-prefixed so a client literally named "(other)"
	// or "(anonymous)" cannot collide with them (a NUL cannot appear in a config
	// client name and is vanishingly unlikely in a request `user` field).
	overflowKey  = "\x00(other)"
	anonymousKey = "\x00(anonymous)"
	// recentRing is how many recent events the dashboard can show.
	recentRing = 200
)

// Record is a single billable event.
type Record struct {
	RequestID        string
	Alias            string
	Provider         string
	Upstream         string
	User             string
	RunID            string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64
	LatencyMS        int64
	Status           string
}

// Stat is an aggregate over some dimension.
type Stat struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

func (s *Stat) add(tokens int64, cost float64) {
	s.Requests++
	s.Tokens += tokens
	s.CostUSD += cost
}

// Event is a compact recent-activity entry for the dashboard.
type Event struct {
	RequestID string  `json:"request_id"`
	Alias     string  `json:"alias"`
	Provider  string  `json:"provider"`
	User      string  `json:"user"`
	RunID     string  `json:"run_id,omitempty"`
	Tokens    int     `json:"tokens"`
	CostUSD   float64 `json:"cost_usd"`
	LatencyMS int64   `json:"latency_ms"`
	Status    string  `json:"status"`
}

// NamedStat is an aggregate labelled by its dimension key (client or run id).
type NamedStat struct {
	Name string `json:"name"`
	Stat
}

// Snapshot is a read-only view for the dashboard / attribution endpoint.
type Snapshot struct {
	Total    Stat        `json:"total"`
	TopUsers []NamedStat `json:"top_users"`
	TopRuns  []NamedStat `json:"top_runs"`
	Recent   []Event     `json:"recent"`
}

// Sink durably persists records (e.g. Postgres). Write MUST be non-blocking and
// best-effort — accounting must never fail or slow the request path, so a sink
// that can't keep up drops (and counts) rather than blocking here.
type Sink interface {
	Write(Record)
}

// Ledger accumulates spend and emits structured records.
type Ledger struct {
	log  *slog.Logger
	sink Sink

	mu     sync.Mutex
	total  Stat
	byUser map[string]*Stat
	byRun  map[string]*Stat
	recent [recentRing]Event
	// recentN is the total number of events seen (unsigned so the modular ring
	// index can never go negative — a signed wrap would panic on the write path).
	recentN uint64
}

// New builds a Ledger that logs through the given slog.Logger.
func New(log *slog.Logger) *Ledger {
	return &Ledger{log: log, byUser: map[string]*Stat{}, byRun: map[string]*Stat{}}
}

// WithSink attaches a durable sink (e.g. Postgres) that every record is also
// written to. Only the composition root should call this, once, before serving.
func (l *Ledger) WithSink(s Sink) *Ledger {
	l.sink = s
	return l
}

// Record persists one billable event. It never returns an error: accounting is
// best-effort and must never fail the request path.
func (l *Ledger) Record(r Record) {
	tokens := int64(r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens)

	l.mu.Lock()
	l.total.add(tokens, r.CostUSD)
	trackLocked(l.byUser, keyOr(r.User, anonymousKey), tokens, r.CostUSD)
	if r.RunID != "" {
		trackLocked(l.byRun, r.RunID, tokens, r.CostUSD)
	}
	l.recent[l.recentN%recentRing] = Event{
		RequestID: r.RequestID, Alias: r.Alias, Provider: r.Provider, User: r.User,
		RunID: r.RunID, Tokens: int(tokens), CostUSD: r.CostUSD, LatencyMS: r.LatencyMS, Status: r.Status,
	}
	l.recentN++
	l.mu.Unlock()

	if l.sink != nil {
		l.sink.Write(r) // non-blocking durable persist (best-effort)
	}

	l.log.Info("request",
		"request_id", r.RequestID,
		"alias", r.Alias,
		"provider", r.Provider,
		"upstream", r.Upstream,
		"user", r.User,
		"run_id", r.RunID,
		"input_tokens", r.InputTokens,
		"output_tokens", r.OutputTokens,
		"cache_read_tokens", r.CacheReadTokens,
		"cache_write_tokens", r.CacheWriteTokens,
		"cost_usd", r.CostUSD,
		"latency_ms", r.LatencyMS,
		"status", r.Status,
	)
}

// trackLocked adds to m[key], folding overflow into a shared bucket once the map
// is at capacity so the map stays bounded and the per-dimension sums still
// reconcile to the grand total. Assumes the ledger lock is held.
func trackLocked(m map[string]*Stat, key string, tokens int64, cost float64) {
	if _, ok := m[key]; !ok && len(m) >= maxTracked {
		key = overflowKey
	}
	st := m[key]
	if st == nil {
		st = &Stat{}
		m[key] = st
	}
	st.add(tokens, cost)
}

func keyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// droppable is implemented by durable sinks that shed records under backpressure.
type droppable interface{ Dropped() uint64 }

// SinkDropped reports records the durable sink lost to backpressure/outage (0 if
// no sink, or the sink doesn't track drops). Surfaced on /metrics so durability
// loss is observable.
func (l *Ledger) SinkDropped() uint64 {
	if d, ok := l.sink.(droppable); ok {
		return d.Dropped()
	}
	return 0
}

// Totals returns the running aggregates, used by the /metrics endpoint.
func (l *Ledger) Totals() (requests int64, tokens int64, costUSD float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total.Requests, l.total.Tokens, l.total.CostUSD
}

// Snapshot returns a bounded, read-only view for the dashboard: grand totals, the
// top-n spenders by client and by run, and the most recent events (newest first).
// The map/ring copies happen under the lock, but the (potentially large) sorts run
// AFTER releasing it, so the billing hot path is never blocked on sorting.
func (l *Ledger) Snapshot(topN int) Snapshot {
	l.mu.Lock()
	total := l.total
	users := collectLocked(l.byUser)
	runs := collectLocked(l.byRun)
	recent := l.recentLocked()
	l.mu.Unlock()
	return Snapshot{
		Total:    total,
		TopUsers: topN_(users, topN),
		TopRuns:  topN_(runs, topN),
		Recent:   recent,
	}
}

// collectLocked copies m into a slice of value stats. Assumes the lock is held.
func collectLocked(m map[string]*Stat) []NamedStat {
	all := make([]NamedStat, 0, len(m))
	for k, st := range m {
		all = append(all, NamedStat{Name: displayName(k), Stat: *st})
	}
	return all
}

// topN_ sorts entries by descending cost and truncates to n. Lock-free.
func topN_(all []NamedStat, n int) []NamedStat {
	sort.Slice(all, func(i, j int) bool {
		if all[i].CostUSD != all[j].CostUSD {
			return all[i].CostUSD > all[j].CostUSD
		}
		return all[i].Name < all[j].Name // stable tiebreak
	})
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	return all
}

// displayName strips the NUL prefix from sentinel bucket keys for presentation.
func displayName(k string) string {
	if len(k) > 0 && k[0] == 0 {
		return k[1:]
	}
	return k
}

// recentLocked returns the recent ring, newest first. Assumes the lock is held.
func (l *Ledger) recentLocked() []Event {
	n := l.recentN
	if n > recentRing {
		n = recentRing
	}
	out := make([]Event, 0, n)
	for i := uint64(0); i < n; i++ {
		idx := (l.recentN - 1 - i) % recentRing
		out = append(out, l.recent[idx])
	}
	return out
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
