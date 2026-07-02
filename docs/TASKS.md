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

## Phase 2 — Cache-aware routing (spiked, then DEMOTED — see ADR 0001)
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

### Cache-aware routing — DEMOTED (see ADR 0001)
Not the wedge. Cache-state (`internal/cache`) is retained as a cache-efficiency
observability signal (hit-rate, cache-busting / prefix-stability detection). The
org-grade "cache-aware selection on the live path" is deprioritized.

---

# NEW DIRECTION — agent-spend firewall (ADR 0001, Invariant #9)

The wedge: hard, real-time, **pre-vendor** enforcement for agentic traffic —
generalizing reserve/settle from monthly budgets to short time constants + run
scope. ~80% reuse of what's built (auth, reserve/settle, rate, failover, ledger).

## Phase 2R — Prerequisites (gate the guarantees at scale)
- ✅ **Streaming (SSE)** — OpenAI-compatible `stream:true` end-to-end (Anthropic
  via SDK; OpenAI-compat via SSE relay). Pre-first-byte failover; mid-stream
  abort / usage-omitting backends **fail closed** (charge the estimate) so
  streaming can't be a free firewall bypass. Reviews: ✅ Go
  (`docs/reviews/phase2r-streaming-go.md`) · ✅ SSE-compat
  (`docs/reviews/phase2r-streaming-compat.md`).
- ✅ **Shared state store (Redis)** — **run-kill** + **velocity/concurrency** now
  cross-replica (ADR 0002); the per-instance N× caveat is closed for these.
  `internal/redisstore` implements `firewall.KillStore` structurally
  (firewall stays pure); kill state is layered (always-on local map + optional
  Redis), so a kill on one replica propagates to all AND a locally-issued kill
  survives a Redis blip. Reads fail open (availability); a kill that can't durably
  record surfaces as 5xx (never a false success); at the map cap a new kill is
  refused, never satisfied by evicting a live kill; run ids charset-validated on
  both reserve+kill paths; `/metrics` exposes kill-pressure + propagation-failure
  counts. Hermetic tests via miniredis (cross-instance, TTL, fail-open, cap).
  Reviews: ✅ security (`docs/reviews/phase2r-redis-security.md`, FAIL→fixed) ·
  ✅ Go (`docs/reviews/phase2r-redis-go.md`, PASS-with-nits→fixed).
  - ✅ **Shared velocity + concurrency** (ADR 0002) — `internal/redisstore`
    `ScopeStore`: the whole multi-scope check-and-reserve is one atomic Lua `EVAL`
    (velocity = rolling-window hashes; concurrency = ZSET distributed semaphore
    with crash-safe hold TTL). The firewall delegates when `redis_url` is set;
    fails OPEN (counted as `firewall_scope_degraded`). Hermetic miniredis tests +
    a cross-replica E2E (two servers, one Redis, one shared $/min cap — 2/6 served
    vs ~4 per-instance). On a Redis outage it falls back to LOCAL enforcement
    (bounded N×, never unenforced), counted as `firewall_scope_degraded`. Reviews:
    ✅ Go (`docs/reviews/phase2r-scopestore-go.md`, FAIL→fixed) · ✅ security
    (`docs/reviews/phase2r-scopestore-security.md`, PASS-with-nits→fixed).
  - ⬜ Still per-instance: loop detection (local heuristic), `max_usd_per_run`
    (mitigated by shared kills — first replica to trip stops the run everywhere).
  - ⬜ Deferred: cancel in-flight (streaming) requests on kill; durable/permanent
    kill beyond TTL; 1 MiB SSE line cap; byte-based settlement for aborted streams.

## Phase 3F — Firewall primitives (the headline) — MVP DONE
- ✅ **Run identity** — `X-Heave-Run-Id`, scope namespaced by the authenticated
  key (a spoofed run id can't touch another caller's run).
- ✅ **Velocity caps** — $/min and tokens/min per key AND per run, **reserved**
  at admit via a `Ticket` (reserve/settle) so concurrent requests can't overshoot.
- ✅ **Per-run kill switch** — `POST /v1/runs/{id}/kill` (owner-scoped) + auto-trip.
- ✅ **Concurrency caps** — max in-flight per key/run.
- ✅ **Repeated-prompt detection** — sliding-window (catches A/B alternation);
  exact-hash, so a per-turn nonce defeats it (heuristic, documented).
- ✅ **Hard per-run $ budget** (`max_usd_per_run`) — cumulative, auto-kills a run
  once its spend would exceed the cap; the backstop for changing-prompt runaways
  loop detection can't see. Built on reserve/settle. (Fable review finding #2.)
- ✅ **End-to-end validation** — `internal/server/e2e_firewall_test.go` (hermetic
  OFF-vs-ON counterfactual: runaway/kill/velocity/growing-context/concurrent-burst,
  asserting denials are pre-vendor) + a `live` twin against real Anthropic. Metric
  reframed to a loss bound (not a spend-reduction %), with the honest negative on
  growing-context loop detection shown explicitly.
- Reviews: ✅ security (`docs/reviews/phase3f-firewall-security.md`, FAIL→fixed) ·
  ✅ Go (`docs/reviews/phase3f-firewall-go.md`) · ✅ strategic/E2E (Fable 5,
  `docs/reviews/phase3f-firewall-e2e-fable.md`) · ✅ per-run-budget Go + security
  (`docs/reviews/phase3f-firewall-budget-{go,security}.md`, PASS-with-nits→fixed).
  In-memory/per-instance MVP.

### Deferred from the E2E/Fable + budget reviews (tracked)
- ⬜ Durable/cross-restart per-run budget (today: active-lifetime, idle-reclaimed;
  monthly budget is the absolute backstop).
- ⬜ Tokenizer-accurate cost estimate (today: chars/4 input heuristic — not a
  strict upper bound, so per-run/velocity caps carry ~Nx slack on adversarial input).
- ⬜ Hard-cap the scope map like the kill map (today: GC-triggered idle eviction;
  a >50k distinct-run-id spray within the idle window can grow it past the cap).
- ⬜ Growing-context / structural-similarity loop detection (beyond exact-hash).
- ⬜ Guidance/middleware so stock agent frameworks send `X-Heave-Run-Id`
  (per-run controls are inert without it).

### Deferred from firewall reviews (tracked)
- 🟡 Cross-replica shared store (Phase 2R) — **kill is now shared** (Redis);
  velocity/concurrency still per-instance until an atomic Redis reserve/settle.
- ⬜ Nonce-robust / structural-prefix loop detection (beyond exact-hash).
- ⬜ Require auth for firewall enforcement to be meaningful (auth-off = dev only).
- ⬜ Tokenizer-based cost/token estimate (replace the chars/4 heuristic).

## Phase 4F — Provider-quota brokering
- ✅ **Quota-aware failover** (ADR 0003) — reserve a provider's known shared
  RPM/TPM PRE-vendor (reusing the ADR-0002 atomic scope store: RPM→count
  dimension, TPM→tokens); if a provider is at its ceiling, skip to the next
  candidate; if all are exhausted, return **429 + Retry-After** (a truthful "quota
  full", never a provoked vendor 429). `internal/broker` (pure; injected store).
  Requires the shared store (a global limit can't be brokered per-instance);
  inert without it. Fails open. Unit tests (fake store) + server integration
  (failover, 429, **cross-replica RPM=2 → 2 served across 2 replicas**). Reviews:
  ✅ Go (`docs/reviews/phase4f-broker-go.md`) · ✅ security
  (`docs/reviews/phase4f-broker-security.md`).
- ⬜ Deferred: bounded admission **queuing** (hold a request briefly for headroom
  vs. reject); **priority / fair-share** weighting across teams; a provider
  concurrency dimension; single-node local fast path.

## Phase 5 — Attribution & visibility (was "spend dashboard")
- ⬜ Durable ledger (Postgres) attributed by org/team/key/**run**.
- ⬜ Dashboard framed around the firewall: spend velocity, near-limit runs,
  kills, top burners, cache-hit rate as a secondary efficiency panel.

## Carried-over deferred items (from Phase 0/1 reviews — still live)
- ⬜ Per-client/route rejection + velocity counters on /metrics.
- ⬜ Key revocation/expiry + hot config reload; global rate/concurrency cap.
- ⬜ Per-model/per-provider budgets; org/team hierarchy.
- ⬜ Per-attempt failover sub-deadline; per-alias no-cross-provider flag.
- ⬜ `ConversationKey` length-prefixed framing.

## Backlog / cross-cutting
- ⬜ Public repo flip when the firewall MVP lands; Apache-2.0 already in place.
- ⬜ Provider-adapter CONTRIBUTING guide (the obvious first contribution).
- ⬜ Branch protection + merge queue (article levers #6/#13).
