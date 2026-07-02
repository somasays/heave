# Cross-replica velocity/concurrency (ADR 0002) — security review

**Scope:** the shared scope store as a security control (fail-open abuse,
cross-tenant, cap evasion, clock skew, Redis DoS, honesty).
**Verdict:** PASS-WITH-NITS. Happy-path claim holds (one atomic `EVAL`,
reserve-at-admit, distributed semaphore, E2E). Cross-tenant/key-forging is
genuinely closed: owner is the authenticated client (never `user`), `runKey` is
length-prefixed + charset-validated, `holdID` is 128-bit crypto-random.

## Must-fix → resolution
| # | Finding | Resolution |
|---|---------|-----------|
| M1 | Fail-open ticket reconciles against Redis → corrupts the shared counter after a blip (same as Go MF-1). | Fail-open now falls back to local enforcement with a local-backed ticket; no phantom store mutation. |
| M2 | `holdTTL` hardcoded 300s, decoupled from `request_timeout` → a long stream's hold reaped while live → concurrency over-admits. | `holdTTL` derived from `request_timeout + 60s`, floored at 300s; operator cannot lower it below the floor (`SetHoldTTL`). |
| M3 | Inducible fail-open (push Redis latency past the 500ms op timeout) degraded caps to **fully unenforced fleet-wide** — worse than the no-Redis N× baseline. | On store error the firewall now falls back to **local per-instance** enforcement (still bounded), not blind admit; degradation counted (`firewall_scope_degraded`, now also on settle/release failures). ADR degradation policy updated. |

## Nits (documented in ADR "Residual limits")
- No hard Redis key-cardinality cap (relies on per-key TTL; bounded by per-key
  rate limit; a `maxKills`-equivalent is a follow-up).
- Wall-clock-per-replica window (NTP assumed; `redis.call('TIME')` a future
  tightening).
- Shared-velocity estimate slack + untagged-traffic per-key-only scope: same
  honest caveat as the per-run budget, now stated for shared velocity too.
- Settle-to-current-second reconcile (bounded by request lifetime).

**Bottom line:** ships as a shared, pre-vendor velocity/concurrency cap that
degrades to bounded local enforcement (never unenforced) on a Redis outage — with
the estimate-slack and clock-sync limits disclosed.
