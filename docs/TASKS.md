# Task tracker (durable, spans sessions)

This committed file is the source of truth for project state across sessions.
The harness task list mirrors it within a session; this file survives. Keep it
current: when work moves, edit the status here in the same change.

Status: ⬜ todo · 🟡 in progress · ✅ done. Each phase is done only after both
adversarial reviews (LLM-apps + Go) are logged in `docs/reviews/` (see
Invariant: Phase gate).

## Foundation
- ✅ **F1 — Invariants + commit-time enforcement.** `docs/INVARIANTS.md`,
  `CLAUDE.md`, `.githooks/*`, `scripts/check*.sh` (fail-cheap, `-race`),
  cheap-first CI. Enforced and green.

## Phase 0 — OpenAI-compatible proxy that works
- ✅ **P0.1** `POST /v1/chat/completions`, health, metrics — `internal/server`
  (body cap, request deadline, honest error taxonomy, reject-don't-drop).
- ✅ **P0.2** Anthropic adapter (official SDK, message normalization, cache-token
  capture) + OpenAI-compatible adapter (typed errors, `Retry-After`, LimitReader).
- ✅ **P0.3** Static router (per-model max-tokens + sampling policy) + cost
  accounting (incl. cache-token pricing) + structured JSON logs.
- ⬜ **P0.4** Streaming passthrough (currently rejected with a clear error).
- ⬜ **P0.5** Full OpenAI-compat conformance suite.
- Reviews: ✅ LLM-apps (`docs/reviews/phase0-llm.md`) · ✅ Go
  (`docs/reviews/phase0-go.md`) — both FAIL→pass-with-follow-ups; must-fixes
  landed, deferred items below.

### Deferred from Phase 0 reviews (do not lose)
- ⬜ OpenAI-compat: flag/estimate cost when upstream omits `usage`.
- ⬜ Retry policy on the OpenAI-compat path (reliability).
- ⬜ `name` field, `system_fingerprint`, error-envelope `param` (low priority).
- ⬜ Send `cache_control` breakpoints (belongs to Phase 2).

## Phase 1 — Controls
- ✅ **Gateway auth + rate caps + budgets (reject before vendor)** —
  `internal/controls` (Bearer + SHA-256 keys, token-bucket rate limit, monthly
  budget via reserve/settle), wired before dispatch; Invariant #7. In-memory,
  per-instance (durable/shared store → Phase 3). Reviews: ✅ security
  (`docs/reviews/phase1-security.md`, FAIL→pass-with-follow-ups) · ✅ Go
  (`docs/reviews/phase1-go.md`).
- ⬜ PII redaction pre-flight hook.
- ⬜ Cross-provider failover with health tracking.

### Deferred from Phase 1 controls reviews
- ⬜ Per-client rejection counters on /metrics (denials now logged, not counted).
- ⬜ Key revocation/expiry + hot config reload (leaked key is live until restart).
- ⬜ Global (gateway-wide) rate + concurrency cap (only per-client today).
- ⬜ Per-model / per-provider budgets; org/team hierarchy (flat per-client now).
- ⬜ Settle billed failures at actual cost (currently released at 0).

## Testing
- ✅ Two-tier tests: hermetic gate (`make check`, `-race`) + live smoke tier
  (`//go:build live`, `make smoke`, nightly `smoke` workflow) — real provider
  calls, key-gated, never in the blocking gate.

## Phase 2 — Cache-aware routing (the wedge)
- ⬜ Redis cache-state store (per-conversation model/prefix_hash/last_seen/ttl).
- ⬜ Router pins to warm-cache model; re-route only on TTL lapse.
- ⬜ Prefix-stability helper (append-only history, stable tool/system order).
- ⬜ Benchmark harness proving savings vs naive routing (this is the marketing).
- Reviews: ⬜ LLM-apps · ⬜ Go

## Phase 3 — Spend visibility
- ⬜ Postgres spend ledger (durable, attributed by org/team/user/key).
- ⬜ Dashboard: spend stacked by org vs token usage; cache-hit rate; savings.
- Reviews: ⬜ LLM-apps · ⬜ Go

## Backlog / cross-cutting
- ⬜ Public repo flip at Phase 1; Apache-2.0 already in place.
- ⬜ Provider-adapter CONTRIBUTING guide (the obvious first contribution).
- ⬜ Branch protection + merge queue (article levers #6/#13).
