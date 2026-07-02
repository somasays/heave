# Per-run $ budget (`MaxUSDPerRun`) — Go / concurrency review (adversarial)

**Scope:** the new hard per-run cumulative $ budget on the reserve/settle
machinery — `internal/firewall` (`Limits.MaxUSDPerRun`, `scopeState.runUSD`,
`Ticket.runScopeKey`, the `enterLocked` trip + reserve/settle/release of
`runUSD`), config/main wiring, unit + E2E tests.
**Verdict:** PASS-WITH-NITS.

## Verified sound (no change)
- **No concurrent overshoot.** `enterLocked` holds `f.mu` across the budget read,
  the trip decision, and the reserve (`runUSD += estUSD`) — one critical section,
  so concurrent Enters on a run serialize and cannot collectively pass a check
  only one should. Inherits velocity's reserve-at-admit atomicity.
- **Trip path leaks nothing** — returns before `ensureScopeLocked`/`inflight++`/
  the reserve, nil ticket, no Settle/Release expected.
- **Settle-before-Release** confirmed in the handler; the accumulator nets to
  `actual` per settled ticket, `est` per in-flight; idle eviction can't corrupt an
  in-flight ticket (its run scope holds `inflight >= 1` until Release).

## Must-fix → resolution
| # | Finding | Resolution |
|---|---------|-----------|
| MF-1 | Idle eviction silently resets `runUSD`; the "hard cap" doc/E2E framing didn't match the "active-lifetime" reality, and the reset was untested. | Downgraded the wording everywhere (`Limits.MaxUSDPerRun`, config, config.example, Invariant #9) to "active-lifetime, resets when idle-evicted; the per-client monthly budget is the absolute backstop." Added `TestPerRunBudgetResetsAfterIdleEviction` (pins the reset) and `TestBudgetKilledRunStaysKilledAfterScopeEvicts`. |
| MF-2 | "Hard cap" only holds if `est` is an upper bound; the `actual > est` direction (worse under concurrency) was untested. | Documented the est-upper-bound + `MaxConcurrent` dependency. Added `TestPerRunBudgetOvershootBoundedThenTrips` (single-request overshoot bounded, next trips) and `TestE2E_PerRunBudgetOvershootIsBoundedNotAbsolute` (billed $0.024 over a $0.02 cap, bounded to ~1 call). |

## Nits → resolution
- N1 (Settle/Release ordering): added a `tk.released` guard in `Settle`.
- N2 (auto-kill swallows `doKill` error): added a comment at the trip site noting
  the auto paths intentionally swallow (self-healing via re-trip; observable in
  Stats), unlike the manual endpoint.
- N3 (combined-limits untested): added `TestVelocityRejectDoesNotChargeRunBudget`.
- N4/N5: covered by the stays-killed test; style clean.
