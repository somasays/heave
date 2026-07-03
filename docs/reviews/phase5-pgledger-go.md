# Durable Postgres ledger (Phase 5 increment 2) — Go / concurrency review

**Scope:** `internal/pgledger` (async batched writer + pgx CopyFrom), `ledger.Sink`
seam, main wiring, integration test.
**Verdict:** PASS-WITH-NITS. Core is sound — the send channel is never closed (so
Write can't panic), Close is idempotent (`sync.Once`), the quit-drain flushes the
partial batch, CopyFrom column/value order is aligned, `dropped` is atomic.

## Must-fix → resolution
| # | Finding | Resolution |
|---|---------|-----------|
| MF | Shutdown race: a Write past the `closed` check but preempted could land in the buffer AFTER the drain loop exited — lost AND uncounted, breaking "loss is observable". | Close now sweeps the buffer after the loop's final flush, counting each straggler as dropped; comment corrected. `TestConcurrentWriteAndClose` (‑race). |

## Nits → resolution
- Ticker flush path was untested (all tests used `time.Hour` to disable it) →
  `TestTickerFlushesPartialBatch` asserts a partial batch flushes on the timer.
- Integration test only checked `count(*)` (a column transposition would pass) →
  now asserts `client/input_tokens/output_tokens/cost_usd/status` values.
- `TestDropsWhenBufferFull` asserted `>0` → tightened to exact `==2`.
- `batch[:0]` reuse is safe only for a synchronous, non-retaining flush + scalar
  Record → documented as the flush contract.
- `ts` defaults to flush-time (Record has no event-time field) — documented
  limitation. Single flush-error drops the batch with no retry — best-effort per
  Invariant #5, documented. `int`→INT4 overflow at 2^31 tokens — unreachable.

Integration test PASSES against real Postgres 14 (schema, batched COPY,
flush-on-close, column-value assertion).
