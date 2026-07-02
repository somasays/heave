# ADR 0003 — Provider-quota brokering (proactive, pre-vendor, shared)

**Status:** accepted (2026-07)
**Context:** the last lever of Invariant #9. A vendor account has a *shared* rate
limit (RPM / TPM) across every team routed through the gateway. Today heave reacts
to a 429 by failing over (Phase 1). Brokering is the proactive version: know the
provider's quota, RESERVE against it before dispatch, and route to a provider that
has headroom — preventing the 429 (and the retry storm / tail-latency it causes)
instead of cleaning up after it. This is the multi-team quota-fight pain no
proxy-with-budgets solves: LiteLLM/Portkey load-balance and retry, they do not
arbitrate a known shared ceiling pre-vendor.

## Decision

A `Broker` reserves provider quota during candidate selection, reusing the SAME
atomic reserve/settle scope store built for cross-replica velocity (ADR 0002). A
provider scope maps onto the store's two rate dimensions:

- **RPM** → the "count" dimension: reserve `1` per request, cap = configured RPM.
- **TPM** → the token dimension: reserve the token estimate, cap = configured TPM,
  reconciled to actual on Settle.

Integration is **quota-aware failover**: in the dispatch loop, before calling a
candidate provider, the broker tries to reserve its quota. If the provider is at
its ceiling, that candidate is SKIPPED (like an unhealthy one) and the next
candidate is tried. If every candidate is quota-exhausted and none was dispatched,
the client gets **429 with `Retry-After`** (a truthful "the shared quota is full",
not a vendor error). On dispatch: settle actual tokens on success, release the
reservation on failure (so a request that never reached the vendor doesn't burn
its quota).

## Requires the shared store (deliberate)

Brokering is **only active when `firewall.redis_url` is set**. A provider limit is
a single global number; the gateway cannot know how many replicas share it, so
per-instance brokering would silently enforce N× the real limit — worse than
honest failover-after-429. So without Redis, brokering is OFF and the existing
reactive failover (Phase 1) handles rate limits. A single-node local fast path is
a possible follow-up, but "off unless it can be correct" is the right default for
a control whose whole value is protecting a shared ceiling.

## Scope / non-goals (this increment)

- **No queuing / no priority tiers yet.** Reserve-or-skip-or-429; we do not hold a
  request waiting for quota, nor weight teams. Bounded admission queuing and
  fair-share/priority are tracked follow-ups.
- **No concurrency dimension** for providers yet (RPM + TPM only).
- Fail OPEN on a Redis error (consistent with the rest): a store outage disables
  brokering for that request rather than blocking it; failover still applies.

## Consequences

Requests proactively avoid a provider's 429 and spread across providers by
available headroom, cluster-wide. Cost: one extra scope-store `EVAL` per candidate
tried. The RPM-onto-count-dimension reuse keeps it to a thin broker over the
already-reviewed store.

## Residual limits (owned; surfaced by the Go + security reviews)

- **Reduces, does not eliminate, vendor 429s.** TPM reserves the token *estimate*
  (`estInput` is chars/4, not a strict upper bound), reconciled to actual only on
  Settle — so byte-heavy input, or many concurrent requests each under-reserving,
  can still exceed the vendor's real TPM. RPM (an exact count) is not affected.
- **Fail-open is observable but real.** A Redis error admits (brokering off for
  that request) and increments `broker_scope_degraded` on `/metrics`. An attacker
  who pushes Redis latency past the 500ms op timeout can disable brokering
  fleet-wide (falling back to Phase-1 failover-after-429) — the metric is the
  signal to alert on.
- **Clock skew (inherited from ADR 0002).** RPM/TPM ride the same rolling-window
  sum whose GC uses the reserving replica's wall clock; skew beyond the 60s window
  can delete a peer's live reservation → under-count → admit past the vendor
  ceiling (which surfaces as an actual vendor 429). NTP-synced replicas assumed.
- **Provider-global fairness.** One tenant bursting to a provider's RPM starves
  others on that provider (no queuing / no per-tenant weight — a tracked
  follow-up). The per-client velocity/RPM caps run BEFORE the broker reserve, so a
  tenant is bounded by its own cap first — but only if those per-client caps are
  configured.
- **No Retry-After jitter.** The fixed 60s hint can synchronize retries; a small
  jitter is a cheap follow-up.
