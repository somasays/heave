# Per-run $ budget (`MaxUSDPerRun`) — security / LLM-apps review (adversarial)

**Scope:** the per-run cumulative $ budget as an anti-overspend control —
`internal/firewall` + `internal/server` estimate/wiring + config + tests.
**Verdict:** PASS-WITH-NITS. The reserve/settle plumbing, owner scoping, and
pre-vendor placement are correct; the mechanism catches the stated threat (a
changing-prompt runaway against a well-behaved provider). The *guarantee as
worded* was overclaimed and is now corrected.

## Confirmed no finding
- Owner scoping is solid: `clientName` uses the authenticated key, never the
  spoofable `user` field; `runKey` is length-prefixed (no forgeable cross-owner
  collision).

## Must-fix → resolution
| # | Finding | Resolution |
|---|---------|-----------|
| 1 | The cap is enforced on `est`, and the input term is `chars/4` (bytes) — NOT an upper bound. Adversarial/byte-heavy input tokenizes higher, so an admitted call can bill up to ~4× its estimate; the "upper-bound cost" comments were false. | Downgraded the wording in `estimate` (`server.go`), `Limits.MaxUSDPerRun`, config, config.example, and Invariant #9 to "cap on the estimate; actual may exceed by ~one call's estimation error, more under concurrency; pair with `MaxConcurrent` + a tokenizer estimate for tightness." Demonstrated by `TestE2E_PerRunBudgetOvershootIsBoundedNotAbsolute` (billed exceeds the cap, bounded to ~1 call). |
| 2 | `runUSD` is silently reset on idle scope eviction → a runaway paced slower than the eviction window never trips; breaks the cap for benign long runs too. Config docs said "hard cumulative cap." | Aligned all docs to "active-lifetime, resets when idle-evicted"; named the per-client **monthly budget** (Invariant #7) as the absolute, non-evictable backstop across idle gaps and run-id rotation. Pinned by `TestPerRunBudgetResetsAfterIdleEviction`. |

## Nits (disclosed / tracked, not fixed here)
- Run-id omission/rotation evades the per-run cap (inherent to per-run scoping;
  disclosed — "needs a run id"; the per-key velocity + monthly budget still apply).
- Auto-trip kill swallows `ErrKillStoreFull` (self-healing; commented; observable
  via Stats) — parity note vs. the manual endpoint's 503.
- Scope map is GC-triggered, not hard-capped like the kill map (pre-existing;
  tracked in `docs/TASKS.md`).
- openai-compat with no `max_output_tokens` → output est can undercount the
  vendor default; mitigated by setting `max_output_tokens` per model (the example
  config does); documented as the estimate's precondition.

**Bottom line:** ships as "a per-run spend backstop enforced on the estimate over
a run's active lifetime, pre-vendor," with the monthly budget as the absolute
ceiling — not as an absolute hard cap.
