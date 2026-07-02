# Provider-quota brokering (Phase 4F, ADR 0003) — security review

**Scope:** the broker as a control (evasion, fail-open abuse, estimate slack, 429
correctness, cross-replica correctness, honesty).
**Verdict:** PASS-WITH-NITS. The core guarantee holds within its disclosed
envelope; accounting is conservative (over-counts / blocks more, never silently
under-counts) except via the two disclosed vectors (fail-open, ADR-0002 skew).

## Confirmed correct
- Scope key `"prov:<name>"` is router-derived — a client cannot forge or evade
  another provider's quota. "Off unless correct" enforced in code (`main.go`),
  loud warn when limits set but Redis absent. `broker.Active` guards nil/empty.
- Mid-stream fail-closed keeps the count (no under-count); skip/never-billed
  releases; per-iteration idempotent leases (no double-settle/leak).
- Release-in-wrong-second floors at 0 → over-count, never under-count (safe for a
  ceiling). 429 requires `quotaBlocked && !attempted` (server-determined, not
  client-gameable).

## Must-fix → resolution
| # | Finding | Resolution |
|---|---------|-----------|
| 1 | Broker fail-open was **silent** (no metric/log) — an attacker inducing Redis latency past the 500ms op timeout disables brokering fleet-wide with no operator signal, violating the project's own observability precedent (`firewall_scope_degraded`). | Added a `degraded` counter (`Broker.Degraded()`), surfaced as `broker_scope_degraded` on `/metrics`; `TestBrokerFailOpenIsObservable`. |

## Nits → resolution (documented in ADR 0003 "Residual limits")
- Clock-skew (inherited) now weakens a *vendor-limit* guarantee (exceeding it =
  real vendor 429s) — restated in ADR 0003 + Invariant #9.
- TPM estimate slack: "reduces, does not eliminate, vendor 429s" — Invariant #9
  and ADR wording softened from "instead of provoking the vendor's 429".
- Provider-global fairness (one tenant can starve others; per-client caps run
  first but only if configured) — noted.
- Fixed 60s Retry-After (no jitter → possible synchronized retry) — noted as a
  cheap follow-up.

**Bottom line:** ships as a control that *reduces* vendor 429s pre-vendor and
holds a provider's shared ceiling cluster-wide, degrading observably to Phase-1
failover on a Redis outage — with the estimate-slack, skew, and fairness limits
disclosed.
