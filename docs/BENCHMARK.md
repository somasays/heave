# Cache-aware routing — benchmark (proof-of-thesis spike)

A **deterministic cost simulation** (not a live benchmark) that quantifies the
project's wedge — cache-aware routing vs naive per-turn routing — on synthetic
multi-turn traffic, to decide whether the org-grade build (shared state,
streaming) is worth it. Single-instance, offline, reproducible.

```
make bench            # or: go run ./cmd/cachebench [-seed N] [-conversations N]
```

## Headline (corrected, honest)

Representative result (seed 42, 500 conversations, difficulty stickiness 0.8):

| router      | cost (USD) | warm % | regret % |
|-------------|-----------:|-------:|---------:|
| naive       |      62.25 |    61% |       0% |
| cache-aware |      54.05 |    67% |      13% |

**~13% cheaper** (10.5–13.5% across seeds), **not** the ~27% an earlier,
over-simplified cost model showed. And the aggregate win is not evenly spread:

> **cache-aware costs *more* than naive on ~308 of 500 conversations.**

The net saving is **concentrated in the minority of long conversations** (large,
growing prefixes where cache reuse dominates); most short/medium conversations
are marginally *worse* off, because pinning can hold a pricier model over a cheap
tail and pays the cache-write premium. The honest conclusion isn't "turn cache-
aware routing on everywhere" — it's "apply it where prefixes are large and
conversations run long," which is a real design constraint for the org-grade
build, not a detail.

## The cost model (`internal/cachebench`)

Per turn on model *M* with cumulative prior prefix *P* and new user input *U*:

- Each model keeps its **own** cached prefix for the conversation, with a
  wall-clock TTL. It **survives intervening turns on other models**, so a naive
  A→B→A sequence gets a **partial** hit on return to A (only the delta since A
  last saw the conversation is new) — not a full cold read.
- The portion *M* already holds is billed at **0.10×** (cache read); the new
  tokens are billed at **1.25×** (cache write); output at the output rate.
- Below a model's **minimum cacheable prefix** (Opus 4.8: 4096, Sonnet/Haiku:
  2048), nothing caches — full rate, no discount.

Prices are the 2026 catalog (haiku $1/$5, sonnet $3/$15, opus $5/$25); 0.10× read
and 1.25× write and the 5-minute TTL match Anthropic's published behavior.

### Fidelity corrections applied after adversarial review

The first cut of this model was found **overstated** (honest in construction, but
one load-bearing error inflated the number ~1.6×). Corrections now in place:

1. **Partial cache hits + per-model TTL** (was: any model switch = full cold read
   of the entire prefix). This alone moved the headline from ~25% to ~13%.
2. **Minimum-cacheable-prefix floor** per model (was: cached from turn 2 at any
   size, over-crediting short conversations).
3. **1.25× cache-write premium** included (was: dropped; including it removes the
   "you hid a cost" objection and slightly widens the honest gap).
4. **Realistic difficulty stickiness (0.8)** default (was: 0.6, which made the
   naive router thrash more and flattered cache-aware). Sweep it and the wedge
   shrinks toward zero as stickiness → 1.0.
5. **Per-conversation loss accounting** (`Compare`) surfaces the tail where
   cache-aware loses; the quality trade-off (`regret %`) is reported, and the
   naive baseline is per-turn-ideal (regret 0 by construction).

### Where the wedge shrinks or reverses

From parameter sweeps: it fades for **short conversations** (1–2 turns ≈ single
digits), **sticky-difficulty** workloads (stickiness 0.9 ≈ 8%, 1.0 ≈ 0%), tight
or frequently-expired TTLs, and any conversation where a hard opening turn pins
an expensive model over a long cheap tail (a pathological case can be **several ×
more expensive**). It is strongest on **long (10+ turn), large-system-prompt,
near-noisy-difficulty** conversations with few idle breaks.

### What it deliberately does not model

Real token distributions (synthetic, jittered); provider-specific breakpoint
mechanics beyond one TTL/read/write triple; the *business* value of quality
(regret is counted in turns, not priced); and multi-instance behavior — across
replicas cache-aware routing needs shared state or conversation-sticky load
balancing (see `docs/TASKS.md`). Swap in measured token/difficulty distributions
to sharpen the estimate.

## Decision input

The wedge is **real but modest (~10–13%) and concentrated**, not the halving the
LinkedIn post implies for a whole org (that figure bundles cheaper-defaults +
caching + harness tuning, not routing alone). It justifies the org-grade build
*if* the target traffic is long-multi-turn/agentic — and argues for applying it
selectively (long conversations, large prefixes) rather than globally.
