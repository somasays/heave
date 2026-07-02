# Phase 3F (firewall MVP) — Go expert review (adversarial)

**Scope:** `internal/firewall` + server wiring. **Verdict:** pass-with-follow-ups
→ follow-ups addressed. `-race`/`vet` clean.

The reviewer verified the ring-window math, the single-mutex check-and-reserve,
`Release` idempotency/non-negativity, the `defer release()` contract on every
server path, and the pointer-receiver error types — all sound.

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | Unbounded maps; `inflight` left permanent zero entries | Fixed — inflight deleted at 0; `gcLocked` idle/size-capped sweep |
| 2 | Velocity not reservation-based → overshoot under concurrency | Fixed — reserve/settle `Ticket` (see security #1) |
| 3 | Token velocity ignored the request estimate (lagging) | Fixed — estimate threaded in, estimate-inclusive comparison |
| 4 | Wall-clock (not monotonic) drove the window; a backward step could fold spend into a live slot | Fixed — `advance` clamps `nowSec` to be non-decreasing |
| 5 | `promptHash` hashed the full message list (grows per turn) + NUL framing | Documented as a heuristic (exact-repeat); Invariant #9 reworded. Structural-prefix hashing tracked as a follow-up |
| 6 | Tests didn't exercise concurrency / token cap / release idempotency / drain boundary | Fixed — added `TestConcurrentEntersRaceClean`, `TestTokenVelocityIsPreventive`, `TestConcurrencyCapAndReleaseIdempotent`, `TestFailedRequestReleasesHold`, alternation loop test |
