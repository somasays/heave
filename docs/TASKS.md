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
- ✅ PII redaction pre-flight hook — `internal/redact` (regex, opt-in; email,
  SSN, Luhn-gated credit card, phone, and secret families: AWS/GitHub/GCP/Slack/
  JWT/PEM), applied to message content + `user` before dispatch; Invariant #7.
- ✅ Cross-provider failover with health tracking — `internal/health` circuit
  breaker + router `fallbacks` + server dispatch loop; Invariant #8. Records each
  attempt; 401/403 fail over (→502, never client 401); 429 fails over without
  opening the breaker; served provider surfaced via `X-Heave-*` headers.
- Reviews (failover+redaction): ✅ Go (`docs/reviews/phase1-failover-go.md`) · ✅
  security (`docs/reviews/phase1-failover-security.md`) — both pass-with-follow-ups.

**Phase 1 complete.**

### Deferred from Phase 1 reviews (tracked, not lost)
- ⬜ Per-client rejection counters on /metrics (denials logged, not counted).
- ⬜ Key revocation/expiry + hot config reload (leaked key is live until restart).
- ⬜ Global (gateway-wide) rate + concurrency cap (only per-client today).
- ⬜ Per-model / per-provider budgets; org/team hierarchy (flat per-client now).
- ⬜ Settle billed failures at actual cost (failed attempts recorded, tokens 0).
- ⬜ Per-attempt failover sub-deadline (one deadline shared across attempts).
- ⬜ Per-alias "no cross-provider failover" flag (strict data-residency).

## Testing
- ✅ Two-tier tests: hermetic gate (`make check`, `-race`) + live smoke tier
  (`//go:build live`, `make smoke`, nightly `smoke` workflow) — real provider
  calls, key-gated, never in the blocking gate.

## Phase 2 — Cache-aware routing (the wedge)
### Spike (option A): prove the thesis — DONE
- ✅ In-memory cache-state store (`internal/cache`: per-conversation warm model,
  TTL, prefix-hash conversation key).
- ✅ Deterministic benchmark (`internal/cachebench`, `cmd/cachebench`, `make
  bench`), faithful cost model (per-model partial cache hits + TTL, min-cache
  floor, 1.25× write premium). Documented in `docs/BENCHMARK.md`.
- **Result (honest, post-review):** cache-aware is **~10–13% cheaper** on the
  default multi-turn workload — NOT the ~27% the first over-simplified model
  showed — and the win is **concentrated in long conversations**: cache-aware
  costs *more* on ~60% of conversations. → apply selectively (long/large-prefix
  traffic), not globally.
- Reviews (spike): ✅ cost-model honesty (`docs/reviews/phase2-spike-costmodel.md`,
  OVERSTATED→corrected) · ✅ Go (`docs/reviews/phase2-spike-go.md`).
- **Decision:** the wedge is real but modest; org-grade build is justified for
  long-multi-turn/agentic traffic. Proceed to option B when ready.

### Org-grade build (option B) — next, gated on the spike result
- ⬜ Shared state store (Redis) for cache-state AND budgets/rate/health (fixes
  the N×-per-instance problem at the same time).
- ⬜ Streaming (SSE) — the wedge's value lives in interactive/agentic traffic.
- ⬜ Wire cache-aware selection into the live request path (an "auto" alias with
  a candidate pool + difficulty scorer); degrade to stateless if Redis is down.
- ⬜ Prefix-stability helper (append-only history, stable tool/system order);
  reconcile with redaction (which mutates the prefix).

## Phase 3 — Spend visibility
- ⬜ Postgres spend ledger (durable, attributed by org/team/user/key).
- ⬜ Dashboard: spend stacked by org vs token usage; cache-hit rate; savings.
- Reviews: ⬜ LLM-apps · ⬜ Go

## Backlog / cross-cutting
- ⬜ Public repo flip at Phase 1; Apache-2.0 already in place.
- ⬜ Provider-adapter CONTRIBUTING guide (the obvious first contribution).
- ⬜ Branch protection + merge queue (article levers #6/#13).
