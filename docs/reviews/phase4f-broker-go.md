# Provider-quota brokering (Phase 4F, ADR 0003) — Go review

**Scope:** `internal/broker` + server dispatch integration (`runCandidates`,
`writeNoServe`), config/main wiring.
**Verdict:** PASS-WITH-NITS — one must-fix (TPM fail-open on a usage-omitting
backend); lifecycle, nil-safety, 429/503 classification, purity all sound.

## Verified correct
- Lease lifecycle: exactly one of Settle/Release on every path (fallback-then-
  succeed, unary failure→Release, streaming pre-byte→Release, mid-byte→Settle keep,
  all-skipped→no lease→429). Per-iteration leases; idempotent settled/released
  guards; no leak/double-apply.
- Nil-safety (broker never nil; lease methods guarded); RPM→float count is
  integer-exact past any real RPM; broker stays stdlib-pure (own ScopeStore
  interface).

## Must-fix → resolution
| # | Finding | Resolution |
|---|---------|-----------|
| MF | On success `lease.Settle(inTok+outTok)`; a usage-omitting backend reports 0 → `Settle(0)` subtracts the whole reservation → the request books 0 TPM though it consumed the vendor's quota → shared TPM ceiling under-counted / overshootable. | Fail CLOSED to the estimate when reported usage is zero (`actualTok = acct.estTokens`), mirroring `recordSuccess`. Regression: `TestProviderTPMFailClosedOnZeroUsage`. |

## Nits → resolution
- Stray writes to the uncapped dimension (RPM-only still wrote a token delta):
  Lease now carries `rpm`/`tpm` flags and gates Settle/Release per configured
  dimension. `TestBrokerSingleDimensionSkipsOther`.
- `newHoldID` vestigial (no concurrency dim): commented.
- 429 message overstated when a candidate was merely unhealthy: softened to "no
  provider with available quota for this model".
- Duplicate ScopeStore interface: accepted (structural typing keeps broker pure).

## Test gaps → filled
- Streaming mid-fail keeps count (`TestProviderQuotaStreamingMidFailKeepsCount`),
  TPM path e2e (`TestProviderTPMExhaustionFailsOver`), unary-failure→release→retry
  (`TestProviderUnaryFailureReleasesQuota`), zero-usage settle regression, fail-open
  observability (`TestBrokerFailOpenIsObservable`).
