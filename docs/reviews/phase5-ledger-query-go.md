# Durable ledger query API + event-time — Go review

**Scope:** `internal/pgledger` (`entry` event-time refactor, `TopSpendSince`/`topBy`)
+ `internal/server` (`LedgerReader`, `handleSpend`) + dashboard panel.
**Verdict:** PASS-WITH-NITS, no must-fix.

## Verified
- `entry` batch reuse safe (scalars + immutable `time.Time`; writeBatch copies
  before returning); `ts` is enqueue (event) time via `s.now`, not flush time.
- `topBy`: `rows.Close` deferred, `rows.Err` returned, Scan types match PG
  (bigint→int64, double→float64, COALESCE non-null), `<> ''` excludes NULL/empty.
- `handleSpend` status mapping correct (501 nil-reader / 400 bad-since / 502
  error); pgxpool concurrency-safe; query-after-Close ordered safely (Shutdown
  drains before the LIFO `defer sink.Close()`).
- depguard clean (pgledger: stdlib + pgx + ledger only).

## Nits → resolution
- Event-time only integration-covered → added `TestWriteStampsEnqueueTime`
  (injected clock asserts `entry.ts`).
- `null` vs `[]` shape inconsistency (`/v1/spend` marshalled null on empty) →
  `topBy` now returns a non-nil empty slice, matching `/v1/stats`.
- No query deadline → `handleSpend` wraps the read in a 5s `context.WithTimeout`
  so a slow aggregate can't pin a pool connection.
- Weak endpoint assertion → `TestSpendEndpoint` now decodes the JSON and asserts
  `top_users[0]` values + non-nil `top_runs`.
- (Cosmetic) `s.now()` evaluated on the drop branch of the send `select` — a
  harmless wasted `time.Now()`; left as-is.

Integration test (real PG14) covers column mapping + `TopSpendSince` aggregation +
time filter.
