# Phase 2 spike — cost-model honesty review (adversarial)

**Scope:** `internal/cachebench` cost model + `docs/BENCHMARK.md` claim.
**Initial verdict:** OVERSTATED (honest in construction, not seed-rigged, prices
real — but one load-bearing fidelity error inflated the headline ~1.6×).
**Post-fix:** the headline is corrected from ~27% to a defensible ~13%.

The reviewer confirmed the benchmark was **not** seed-cherry-picked (24–28% band
across seeds on the old model) and prices/multipliers were real, then found the
model rigged in one way and thin in several.

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | Binary cold-on-any-switch: charged a switch as a full cold read of the entire growing prefix; real per-model caches give partial hits and survive intervening turns | Fixed — per-model cached-prefix tracking with wall-clock TTL and partial hits. Headline 25%→16%→(with the rest)~13%. `TestPartialHitOnSwitchBackIsWarm` |
| 2 | Difficulty stickiness 0.6 flattered cache-aware (naive thrashes); real convs are stickier | Fixed — default 0.8; doc reports the sweep and that the wedge → 0 as stickiness → 1 |
| 3 | Missing minimum-cacheable-prefix floor over-credited short conversations | Fixed — per-model floor (Opus 4096, Sonnet/Haiku 2048); `TestBelowMinCacheFloorNoDiscount` |
| 4 | Regret was a turn-count fig leaf; per-conversation cost regressions were invisible | Fixed — `Compare` reports the loss tail: cache-aware costs MORE on ~308/500 convs; worst-case $ loss surfaced |
| 5 | 1.25× cache-write premium dropped | Fixed — now included (widens the honest gap; removes the "hid a cost" objection) |
| 6 | Naive is a semi-strawman (re-tiers every turn, no hysteresis) | Acknowledged in `docs/BENCHMARK.md`; a hysteresis baseline is a follow-up. Kept the article's literal "score each turn independently" naive as the reference point |

**Net effect on the conclusion:** the wedge is **real but modest (~10–13%) and
concentrated in long conversations**, not a halving. This directly informs the
org-grade design (apply selectively; long/large-prefix traffic) and is captured
in `docs/BENCHMARK.md` and `docs/TASKS.md`.
