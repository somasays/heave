// Package pgledger is a durable Postgres sink for the spend ledger (Phase 5): it
// persists every billable record to Postgres behind the ledger's Sink interface,
// so spend survives restarts and can be queried by external BI.
//
// Durability is best-effort by design (Invariant #5 says accounting must never
// fail or slow the request path): Write is non-blocking — records go onto a
// bounded buffer and a background goroutine BATCHES them to Postgres. If the
// buffer is full (Postgres down or lagging) records are DROPPED and counted
// rather than blocking the caller; the in-memory ledger + structured log remain
// the always-on record. The drop counter is surfaced on /metrics so loss is
// observable.
//
// The batching/drop/flush machinery is decoupled from Postgres (an injected flush
// func), so it is unit-tested hermetically; the pgx SQL is exercised by an
// integration-tagged test against a real database.
package pgledger

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/somasays/heave/internal/ledger"
)

// Tunables for the async writer.
const (
	defaultBuffer    = 4096
	defaultBatch     = 256
	defaultFlushEach = time.Second
	writeTimeout     = 5 * time.Second
)

// schema is created on startup if absent (a real deployment would use versioned
// migrations; IF NOT EXISTS keeps the single-binary story simple).
const schema = `
CREATE TABLE IF NOT EXISTS spend (
  id                 BIGSERIAL PRIMARY KEY,
  ts                 TIMESTAMPTZ NOT NULL DEFAULT now(),
  request_id         TEXT,
  alias              TEXT,
  provider           TEXT,
  upstream           TEXT,
  client             TEXT,
  run_id             TEXT,
  input_tokens       INT,
  output_tokens      INT,
  cache_read_tokens  INT,
  cache_write_tokens INT,
  cost_usd           DOUBLE PRECISION,
  latency_ms         BIGINT,
  status             TEXT
);
CREATE INDEX IF NOT EXISTS spend_client_ts ON spend (client, ts);
CREATE INDEX IF NOT EXISTS spend_run_ts    ON spend (run_id, ts);`

// entry is a record plus the EVENT time it was enqueued (which is ~request time,
// not the later flush time), so the durable `ts` reflects when spend happened.
type entry struct {
	rec ledger.Record
	ts  time.Time
}

// Store is an async, bounded, batching durable sink. Implements ledger.Sink.
type Store struct {
	ch      chan entry
	flush   func([]entry) error
	dropped atomic.Uint64
	now     func() time.Time
	quit    chan struct{}
	done    chan struct{}
	closed  atomic.Bool // set by Close so Write stops sending (never send on a
	once    sync.Once   // closed channel: the request path must never panic)

	pool *pgxpool.Pool // nil for the hermetic (injected-flush) constructor
}

// New connects to Postgres, ensures the schema, and starts the writer.
func New(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("pgledger connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgledger ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgledger schema: %w", err)
	}
	s := newStore(nil, defaultBuffer)
	s.pool = pool
	s.flush = s.writeBatch
	go s.loop(defaultBatch, defaultFlushEach)
	return s, nil
}

// newStore builds the writer core with an injected flush (unit-testable). The
// caller starts loop() with its chosen batch size and flush interval.
func newStore(flush func([]entry) error, buffer int) *Store {
	return &Store{
		ch:    make(chan entry, buffer),
		flush: flush,
		now:   time.Now,
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// Write enqueues a record for durable persistence. NON-BLOCKING: if the buffer is
// full (Postgres down/lagging) or the store is closing, the record is dropped and
// counted — never blocking, and never sending on a channel Close might close.
func (s *Store) Write(r ledger.Record) {
	if s.closed.Load() {
		s.dropped.Add(1)
		return
	}
	select {
	case s.ch <- entry{rec: r, ts: s.now()}: // stamp event time at enqueue
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports records lost because the buffer was full or a flush failed.
func (s *Store) Dropped() uint64 { return s.dropped.Load() }

// loop batches records and flushes on a full batch or a timer; on Close it drains
// and flushes the remainder.
func (s *Store) loop(batchN int, each time.Duration) {
	t := time.NewTicker(each)
	defer t.Stop()
	batch := make([]entry, 0, batchN)
	// flush MUST be synchronous and must not retain the batch slice: doFlush reuses
	// the backing array (batch[:0]) after it returns. Safe today — entry is all
	// scalar (no aliasing) and writeBatch copies into COPY rows before returning.
	doFlush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.flush(batch); err != nil {
			s.dropped.Add(uint64(len(batch))) // a failed flush is lost spend — count it
		}
		batch = batch[:0]
	}
	for {
		select {
		case r := <-s.ch:
			batch = append(batch, r)
			if len(batch) >= batchN {
				doFlush()
			}
		case <-t.C:
			doFlush()
		case <-s.quit:
			// Close signalled shutdown. Drain whatever is buffered, flush, and stop.
			// New Writes see closed==true and no longer send; a Write already past
			// that check but preempted mid-send is handled by Close's post-drain
			// sweep (which counts it), so shutdown loss stays observable.
			for {
				select {
				case r := <-s.ch:
					batch = append(batch, r)
					if len(batch) >= batchN {
						doFlush()
					}
				default:
					doFlush()
					close(s.done)
					return
				}
			}
		}
	}
}

// Close stops accepting writes, flushes the buffer, and releases the pool. It is
// idempotent and blocks until the final flush completes. It does NOT close the
// send channel (a concurrent request-path Write must never panic); instead it
// sets closed and signals quit, and the loop drains + flushes the remainder.
func (s *Store) Close() error {
	s.once.Do(func() {
		s.closed.Store(true)
		close(s.quit)
		<-s.done // wait for the loop's final flush
		// Sweep any record a racing in-flight Write slipped into the buffer between
		// its closed-check and the loop's exit, counting each as dropped so
		// shutdown loss stays observable (the in-memory ledger + log still hold it).
		for {
			select {
			case <-s.ch:
				s.dropped.Add(1)
			default:
				if s.pool != nil {
					s.pool.Close()
				}
				return
			}
		}
	})
	return nil
}

// cleanText strips NUL bytes, which Postgres TEXT rejects. `user` is client-
// controlled and not charset-validated, so one crafted request could otherwise
// fail (and drop) an entire batch. No-op for the common NUL-free case.
func cleanText(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// writeBatch bulk-inserts a batch via COPY. Used by the Postgres constructor.
func (s *Store) writeBatch(batch []entry) error {
	columns := []string{
		"ts", "request_id", "alias", "provider", "upstream", "client", "run_id",
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
		"cost_usd", "latency_ms", "status",
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	rows := make([][]any, len(batch))
	for i, e := range batch {
		r := e.rec
		// Strip NUL bytes: Postgres TEXT rejects 0x00, and `user` is client-
		// controlled and not charset-validated — one crafted request would
		// otherwise fail (and drop) the whole batch. cleanText is a no-op for the
		// common (NUL-free) case.
		rows[i] = []any{
			e.ts, cleanText(r.RequestID), cleanText(r.Alias), cleanText(r.Provider), cleanText(r.Upstream),
			cleanText(r.User), cleanText(r.RunID),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens,
			r.CostUSD, r.LatencyMS, cleanText(r.Status),
		}
	}
	if _, err := s.pool.CopyFrom(ctx, pgx.Identifier{"spend"}, columns, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("pgledger copy: %w", err)
	}
	return nil
}

// TopSpendSince returns the top-n clients and runs by cost over records since
// `since`, read from the durable store (the historical view the /v1/spend
// endpoint serves — the in-memory ledger only keeps a recent ring).
func (s *Store) TopSpendSince(ctx context.Context, since time.Time, n int) (byClient, byRun []ledger.NamedStat, err error) {
	byClient, err = s.topBy(ctx, "client", since, n)
	if err != nil {
		return nil, nil, err
	}
	byRun, err = s.topBy(ctx, "run_id", since, n)
	if err != nil {
		return nil, nil, err
	}
	return byClient, byRun, nil
}

func (s *Store) topBy(ctx context.Context, dimension string, since time.Time, n int) ([]ledger.NamedStat, error) {
	// dimension is a fixed internal string ("client"|"run_id"), never user input —
	// no injection surface. %s is safe here.
	q := fmt.Sprintf(`SELECT COALESCE(%s,''), count(*), COALESCE(sum(input_tokens+output_tokens+cache_read_tokens+cache_write_tokens),0), COALESCE(sum(cost_usd),0)
		FROM spend WHERE ts >= $1 AND %s <> '' GROUP BY %s ORDER BY sum(cost_usd) DESC LIMIT $2`, dimension, dimension, dimension)
	rows, err := s.pool.Query(ctx, q, since, n)
	if err != nil {
		return nil, fmt.Errorf("pgledger query %s: %w", dimension, err)
	}
	defer rows.Close()
	out := make([]ledger.NamedStat, 0) // non-nil so an empty result marshals [] (like /v1/stats), not null
	for rows.Next() {
		var ns ledger.NamedStat
		if err := rows.Scan(&ns.Name, &ns.Requests, &ns.Tokens, &ns.CostUSD); err != nil {
			return nil, fmt.Errorf("pgledger scan %s: %w", dimension, err)
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}
