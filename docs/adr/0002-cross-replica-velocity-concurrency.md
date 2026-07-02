# ADR 0002 — Cross-replica velocity & concurrency via an atomic Redis scope store

**Status:** accepted (2026-07)
**Context:** closes the headline caveat of Invariant #9 — "velocity and
concurrency caps remain per-instance; N replicas allow N× each cap." A mid/large
org runs N replicas behind a load balancer, so a per-instance $/min cap is really
N× the intended ceiling. Fable's E2E review named this as the sharpest buyer
objection. Kills are already shared (ADR-adjacent, Phase 2R); this shares the
windowed velocity and the concurrency caps too.

## Decision

Add a **`ScopeStore`** abstraction that the firewall optionally delegates its
velocity ($/min, tokens/min) and concurrency check-and-reserve to. Default is the
existing in-memory path (single node, hermetic tests); with `firewall.redis_url`
set, a Redis-backed store makes the caps hold across replicas.

The reserve/settle/release contract is preserved exactly (Invariant #7): reserve
an estimate at admit, reconcile to actual at settle, release on failure. The
cross-replica version implements it with:

- **Velocity** — one Redis HASH per scope, fields = unix-second → reserved+settled
  $ (and a parallel hash for tokens). The rolling 60s sum is computed in a Lua
  script that purges fields older than the window. Reserve/settle/release adjust
  the *current* second's field (mirrors the local ring, which also always writes
  the current slot). Buckets floor at 0.
- **Concurrency** — a Redis ZSET per scope as a **distributed semaphore**: each
  in-flight request is a member scored by its expiry (`now + holdTTL`). Reserve
  purges expired members (crash-safety: a replica that dies without releasing has
  its holds reaped after `holdTTL`), counts, and admits if `< max`. Release
  ZREMs the member.
- **Atomicity** — the whole multi-scope check-and-reserve (key scope AND run
  scope, velocity AND concurrency, all-or-nothing) is ONE Lua `EVAL`, so
  concurrent admits across replicas cannot TOCTOU past the cap. This is the same
  guarantee the local reserve-under-mutex gives, extended cluster-wide.

## Degradation policy

A Redis error on the admit path must not block all traffic, but it must not drop
the caps either. So on a store error the firewall **falls back to LOCAL
per-instance enforcement** for that request (velocity/concurrency via the local
ring/semaphore) — still bounded (N×), never unenforced — and the resulting ticket
reconciles against LOCAL state, so a fail-open admit can never corrupt the shared
window. The degradation is counted in `/metrics` as `firewall_scope_degraded`
(reserve, settle, and release failures all increment it). The per-client
**monthly budget** (Invariant #7, local+authoritative) remains the absolute
ceiling. (An earlier draft admitted blind on error; review flagged that it dropped
the caps fleet-wide and could be induced by pushing Redis latency over the 500ms
op timeout — hence the local fallback.)

## Scope / non-goals

- Loop detection stays **per-instance** (a local heuristic; each replica re-trips
  independently — acceptable, already documented).
- The per-run `MaxUSDPerRun` budget stays **per-instance** for now; because kills
  are shared, the first replica to trip it stops the run everywhere, but the
  aggregate ceiling before any single replica trips is still ~N×. Sharing it uses
  the same cumulative-counter mechanism and is a tracked follow-up.
- No Redis cluster/hash-tag work: scopes are independent keys; a single-shard or
  proxy-clustered Redis is assumed (documented).

## Residual limits (owned; surfaced by the Go + security reviews)

- **Estimate slack (inherited).** The shared velocity cap reserves the same
  chars/4 input estimate as the local path — not a strict upper bound — so a
  `$/min` cap can be overshot by ~one call's estimation error per admitting
  replica. Untagged traffic (no `X-Heave-Run-Id`) is enforced at the per-key
  scope only. Same honesty caveat as the per-run budget (Invariant #9).
- **Hold TTL vs request lifetime.** A concurrency hold is reaped after
  `holdTTL` seconds as a crashed-replica leak. `holdTTL` is set from
  `request_timeout + 60s` (floored at 300s) so a live request's hold is never
  reaped early; an operator cannot lower it below the floor.
- **Clock assumption.** Each replica stamps the window/semaphore with its own
  wall clock. Skew < the 60s window self-corrects; skew beyond it could purge a
  peer's live reservations early. NTP-synced replicas assumed (not
  attacker-controlled). A future `redis.call('TIME')` inside the Lua would make
  the window authoritative.
- **Reconcile bucket.** Settle/Release write to the current second (mirrors the
  local ring); a request spanning a minute boundary leaves its reserve in the old
  bucket (ages out) — bounded by request lifetime.
- **No hard Redis key cap.** Unlike the local kill map's hard cap, scope keys rely
  on per-key TTL (61s/holdTTL+1s) to bound cardinality; run-id spray is bounded by
  the per-key rate limit + TTL, not refused. A `maxKills`-equivalent is a
  follow-up.

## Consequences

Velocity and concurrency now hold across replicas (the N× caveat is closed for
these two). Cost: a Redis round-trip on the admit hot path (one `EVAL`), and a
dependency on Redis availability for enforcement (fail-open). Hermetic tests use
miniredis (Lua `EVAL` verified in a spike); a cross-instance test asserts two
firewall instances sharing one Redis honor a single shared cap.
