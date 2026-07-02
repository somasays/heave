# Cross-replica velocity/concurrency (ADR 0002) — Go / distributed review

**Scope:** `internal/redisstore/scopestore.go` (Lua reserve/adjust/releaseConc),
firewall integration (`enterShared`, ScopeStore interface, Ticket store-branch),
cross-replica E2E.
**Initial verdict:** FAIL — one correctness must-fix; the atomic reserve itself is
correct.

## Verified correct (no change)
- Two-pass check-then-reserve in one `EVAL` is atomic (Redis serial execution) —
  no TOCTOU, no partial reserve (`TestScopeAllOrNothing`).
- Window bounded (stale fields purged in-script + key EXPIRE).
- Settle/Release idempotent (`settled`/`released` flags set under `f.mu` before the
  post-unlock store call); no Redis call under `f.mu`; `holdID` crypto-random.

## Must-fix → resolution
| # | Finding | Resolution |
|---|---------|-----------|
| MF-1 | On a Reserve error the fail-open ticket kept `store`/`storeKeys`/`holdID`, so its later Settle/Release subtracted a phantom reservation from the shared window (Redis wrote nothing), eroding co-tenants' reservations → silent over-admission. | The fail-open path now falls back to **local** enforcement (`reserveScopesLocal`) and returns a LOCAL-backed ticket (no store binding), so reconciliation hits local state, never the shared window. `TestSharedFailOpenFallsBackToLocalEnforcement` + the strengthened `TestSharedFailOpenCountsDegraded` (asserts store no-op). |

## Nits → resolution
- Reap vs request timeout: `holdTTL` is now derived from `request_timeout + 60s`
  (floored), set via `Store.SetHoldTTL`; `TestSetHoldTTLFloor`.
- Comment inaccuracy (pass 1 also reaps the ZSET): comment corrected.
- Token-velocity settle/release untested: `TestScopeTokenVelocity`.
- Multi-scope mixed velocity+concurrency all-or-nothing: `TestScopeMixedVelocityAndConcurrencyAllOrNothing`.
- Settle/Release store errors now increment `ScopeDegraded` (were swallowed).
- Parallel-slice API kept (preserves package purity; documented).
- Reconcile-current-second + clock assumptions documented in ADR "Residual limits".
