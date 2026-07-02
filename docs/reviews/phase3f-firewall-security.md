# Phase 3F (firewall MVP) — security review (adversarial)

**Scope:** `internal/firewall` + server wiring (Enter/kill endpoint/velocity).
**Initial verdict:** FAIL — as first built the firewall was advisory, not the
"hard, real-time, pre-vendor, run-scoped" guarantee Invariant #9 sold.
**Post-fix:** the guarantee holds within an instance; cross-replica and
detection-quality limits are now stated, not hidden.

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | Velocity was check-only (TOCTOU) → N concurrent requests overshoot the $/min cap | Fixed — reserve/settle via `Ticket`: the estimate is held in the window at `Enter` (visible to concurrent callers) and reconciled at `Settle`; failed requests release the hold. `TestVelocityReservedAndDrains` |
| 2 | `run_id` was an unauthenticated header → cross-tenant kill/poison/scope-evasion | Fixed — run scope is namespaced by the authenticated key (`run:<key>\x00<id>`); `Kill` is owner-scoped; the kill endpoint kills only the caller's own run. `TestManualKillIsOwnerScoped` |
| 3 | Unbounded maps (windows/killed/loops/inflight) → OOM the enforcement point by rotating run ids | Fixed — inflight deleted at 0; idle/size-capped GC sweep (`gcLocked`) evicts drained scopes, idle loops, expired kills |
| 4 | "Loop/anomaly detection" overclaimed (exact consecutive-hash; nonce/alternation defeat) | Fixed — sliding-window count catches A/B alternation; Invariant #9 reworded to "repeated-prompt detection … exact-hash, a per-turn nonce defeats it; a heuristic, not a security control" |
| 5 | Token velocity was reactive (no estimate) | Fixed — output-token estimate threaded in and reserved; `TestTokenVelocityIsPreventive` |
| 7 | Invariant #9 asserted "hard guarantees" with no per-instance caveat | Fixed — #9 now carries the per-instance N× caveat (mirrors #7) and the auth-off caveat |

## Deferred (tracked in `docs/TASKS.md`)
- **Cross-replica shared store** (Phase 2R) — the per-instance N× / kill-not-global limit.
- **Nonce-robust / structural-prefix loop detection** — beyond exact-hash.
- **Auth-off** makes the scope key client-controlled → firewall is dev-only
  without auth; documented (require auth in production).
- Estimate accuracy (chars/4 heuristic) — swap in a tokenizer estimate later.

## Non-issue confirmed
Concurrency slot lifecycle is safe: `Release` is deferred on every post-Enter
path and is idempotent; no slot leak on panic/error.
