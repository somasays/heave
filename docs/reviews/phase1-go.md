# Phase 1 (controls) — Go expert review (adversarial)

**Scope:** `internal/controls` + callers. **Verdict:** pass-with-follow-ups.

`go test -race` and `go vet` clean. Mutex discipline sound: `byHash` is
write-once in `New` then read-only; every `spentUSD`/`monthKey` access is under
`st.mu`, every bucket access under `b.mu`. No data race or correctness bug for
the stated single-instance in-memory scope.

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | Tests were all single-goroutine → `-race` was a false comfort (a dropped lock would pass CI) | Fixed — `TestConcurrentAdmitSettleRaceClean` contends `st.mu`/`b.mu` across 50 goroutines; `TestReserveBoundsConcurrentOvershoot` adds 200 |
| 2 | Budget check-then-add TOCTOU overshoot, undocumented at call site | Fixed — replaced with the reserve/settle hold (see security review #1); overshoot bound now documented in the package header |
| 3 | `subtle.ConstantTimeCompare` in `lookup` was unreachable dead code with a misleading comment | Fixed — removed; comment now explains the map hit *is* the comparison and the compared value is a hash of caller input (no timing channel) |
| 4 | `capacity == RPM` allows ~2×RPM in the opening minute — undocumented | Fixed (doc) — burst behavior noted on the `RateLimitRPM` field |
| 5 | Budget-rejected request still consumes a rate token (rate checked before budget) | Kept intentionally (cheap abuse-throttling, honors Invariant #7); noted |
| 6 | Month-boundary straddle books a request in the new month | Accepted — not a race, documented calendar-reset behavior |

**Coverage gaps closed:** `RateLimitRPM==0` unlimited, `MonthlyBudgetUSD==0`
unlimited-but-tracked, multi-client isolation, concurrent reserve/settle.
