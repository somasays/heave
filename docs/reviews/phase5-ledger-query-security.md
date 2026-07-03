# Durable ledger query API + `/v1/spend` — security review

**Scope:** `TopSpendSince`/`topBy` SQL, `handleSpend` authZ/DoS/error handling,
dashboard durable-history panel.
**Verdict:** PASS-WITH-NITS, no must-fix.

## Verified clean
- **No SQL injection.** `topBy` is unexported; the only caller passes the fixed
  literals `"client"`/`"run_id"` — `dimension` is never request-derived. `since`
  and `limit` are bound params (`$1`/`$2`); `n` is a hardcoded 20. The `%s`
  interpolation is a fixed internal constant.
- **AuthZ consistent.** `/v1/spend` uses the same `requireAdmin` gate as
  `/v1/stats` (401 / 403 / open-when-auth-off); uses `Authenticate` (no rate-bucket
  burn). Same cross-tenant exposure profile as the already-shipped stats endpoint.
- **No info leak on error.** A query failure returns the generic "durable spend
  query failed"; the pgx error is logged server-side only, and the DSN is never in
  the wrapped error (connect errors are separately redacted in main).
- **XSS closed.** The durable-history panel renders `top_users` names via the same
  `esc()` (`&<>"'`) as every other table; the persisted, client-controlled `user`
  cannot inject.

## Nit → resolution
- Unbounded `?since` → an admin could force a full-table aggregate on a large
  `spend` table. Now capped at **90 days** (`> 90d` → 400); `TestSpendEndpoint`
  covers it.

## Nits (left, low-severity)
- Server-side error log may include pgx table/column context (no client exposure).
- The built-in panel is fixed at `?since=24h` though the endpoint accepts any
  window ≤ 90d (cosmetic).
