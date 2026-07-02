# ADR 0001 — Pivot the wedge from cache-aware routing to an agent-spend firewall

**Status:** accepted (2026-07)
**Context:** decision made after a proof-of-thesis spike + an adversarial
strategic review (Claude Fable 5) against the honest spike numbers.

## Context

heave was conceived as an OpenAI-compatible LLM gateway whose differentiator was
**cache-aware routing** — keep a conversation on its warm-cache model, re-route
only when the prefix-cache TTL lapses (Invariant #3, original wording).

We spiked it (`internal/cachebench`, `docs/BENCHMARK.md`) and, after an
adversarial cost-model review corrected an initial overstatement, measured it
honestly:

- ~10–13% cheaper than naive routing (not the ~27% first shown, nor the
  "halved spend" of the viral claim — that bundles cheaper defaults + caching
  hygiene + harness tuning).
- The win is concentrated in long, large-prefix conversations; cache-aware costs
  **more** on ~60% of conversations, and collapses toward 0% as difficulty
  stickiness rises. Caching discounts input only; output is never cached.

A strategic review then ranked the real pains for a mid-large org and found the
gateway aimed at the weakest one (cost-optimization routing), while as a
proxy-with-budgets it is a commodity strictly behind LiteLLM/Portkey/Cloudflare/
Kong. Its genuinely differentiated asset is the **pre-vendor reserve/settle
enforcement** machinery (Invariant #7) and a single fast Go data-plane binary.

## Decision

Reposition heave as a **runtime spend & quota firewall for agentic traffic**:
hard, real-time, pre-vendor enforcement (Invariant #9). Generalize reserve/settle
from a monthly budget to short time constants and run scope — token-velocity caps
($/min per key/run), per-run kill switches, loop/anomaly detection, concurrency
caps, and provider-quota brokering.

Cache-aware routing is **demoted**: the cache-state store (`internal/cache`) is
retained as a cache-efficiency observability signal, not the headline. Invariant
#3 rewritten to match the evidence; README/CLAUDE headline changed.

## Rationale

- **It's the acute, underserved pain.** Monthly budgets are the wrong time
  constant for a runaway agent (five figures in hours); "failover after a 429"
  doesn't arbitrate a provider quota shared across teams. No incumbent does hard,
  pre-vendor, real-time enforcement with guarantees.
- **It aims at heave's strength.** Invariant #7 (controls-before-vendor,
  reserve/settle so concurrent requests can't overshoot) is exactly the
  load-bearing primitive; Go's in-memory speed + race-freedom matter for a
  data-plane enforcement point (unlike proxying, where they buy little).
- **~80% reuse:** auth, reserve/settle, rate limits, failover, ledger all carry
  over; the ledger becomes attribution *for the firewall*.

## Consequences

- Streaming (SSE) and a shared state store (Redis) become prerequisites: without
  them the flagship guarantee is fiction at >1 replica (per-instance N× problem).
- The engineering-discipline story (enforced invariants, adversarial phase gates,
  a self-correcting benchmark) is a portfolio/essay-credibility asset, kept
  distinct from the market wedge.
- Roadmap rewritten in `docs/TASKS.md`.

## Alternatives considered

- **Reframe as cache-observability, stay a general gateway** — rejected: still a
  commodity behind LiteLLM with no defensible wedge.
- **Keep cache-aware routing as the headline** — rejected: our own benchmark
  refutes the claim; it would not survive scrutiny.
