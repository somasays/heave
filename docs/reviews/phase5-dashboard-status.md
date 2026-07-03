# Phase 5 (attribution + built-in dashboard) — review status

**IMPORTANT — the two adversarial expert reviews for this increment did NOT run.**
Both subagents (Go reviewer, security reviewer) terminated on a provider session
limit before producing findings. Per the phase-gate DoD (docs/INVARIANTS.md), a
phase is done only after BOTH an LLM-apps/security review and a Go review pass.
So **Phase 5 is code-complete but its adversarial reviews are DEFERRED** and must
be run before it is marked done. This file records the interim self-review so the
gap is explicit, not hidden.

## Self-review (author, not a substitute for the two adversarial passes)

Scope: `internal/ledger` (attribution aggregation + recent ring + Snapshot),
`internal/server` (handleStats/handleDashboard + admin gate + runID threading),
`internal/server/dashboard.html`.

### Addressed proactively
- **Cross-tenant exposure (the sharp risk).** `/v1/stats` returns every tenant's
  spend/attribution + run ids. Gated behind an **admin** key when auth is enabled
  (`requireAdmin`): no key → 401, non-admin key → 403, admin → 200. Auth-off (dev)
  is open (startup already warns loudly). The `/dashboard` **shell** stays open
  (contains no data); the page prompts for an admin key and sends it as a bearer
  on the `/v1/stats` fetch (sessionStorage, not URL/localStorage). Test:
  `TestStatsRequiresAdminWhenAuthEnabled`.
- **Stored XSS.** `user` (request field, NOT charset-validated) and `run_id` flow
  into the recent-activity table; the page escapes every interpolated value via
  `esc()` (`&<>"`) before `innerHTML`.
- **Concurrency.** Record (writes `*Stat`) and Snapshot (reads `*Stat` into a value
  copy) both hold `l.mu`; verified race-free under `-race`
  (`TestSnapshotConcurrentWithRecord`).
- **Bounds.** by-user/by-run maps capped at `maxTracked` with an overflow bucket
  (grand total always reconciles); recent is a fixed 200-slot ring.
  `TestOverflowBucketBoundsMapAndReconciles`, `TestRecentRingNewestFirstAndBounded`.
- Attribution threaded through all three spend-recording paths (success,
  candidate-error, streaming-aborted).

### Known open items for the deferred adversarial reviews to weigh
- `/v1/stats` sorts up to `maxTracked` entries per poll (3s) — now admin-gated, so
  not an unauthenticated amplification vector, but a brief snapshot cache could be
  considered.
- Run-id enumeration by an admin is inherent to the feature (admin is trusted).
- Whether `/metrics` (aggregate only, no per-tenant names) should also be gated.

## Follow-ups (tracked)
- Durable Postgres ledger behind the same `Record` call (the "attribution" half of
  Phase 5 not yet built — this increment is in-memory only).
- Near-limit-run / quota-headroom panels (needs firewall/broker to expose live
  scope snapshots).
