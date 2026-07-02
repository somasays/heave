# Phase 1 (controls) — gateway-security expert review (adversarial)

**Scope:** `internal/controls`, server wiring, config validation, example config.
**Initial verdict:** FAIL. **Post-fix verdict:** pass-with-follow-ups.

The code was clean, but two of the three headline controls didn't hold under
adversarial conditions.

## Must-fix (resolved)

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | Budget was check-then-add → unbounded concurrent overshoot (real vendor spend past the cap) | Fixed — reserve/settle: `Reserve` holds an upper-bound estimate atomically before dispatch, `Settle` reconciles to actual. Overshoot bounded to one request's estimate error. Test `TestReserveBoundsConcurrentOvershoot` proves ≤ cap under 200 concurrent reserves |
| 2 | Example config shipped auth-off → copy-paste deploy = open proxy to paid keys | Fixed — example now `auth.enabled: true` with an all-zero placeholder client that matches no key (fails closed, 401 for everyone until a real key is added); README quickstart updated |
| 3 | Per-instance limits are silently N× with replicas (undocumented) | Fixed (doc) — stated on the config fields, in the `controls` package header, and in Invariant #7 |
| 4 | Budget-exceeded → bare 429, inviting a month-long retry storm | Fixed — `BudgetError` carries `RetryAfterSec` = seconds to next UTC month; server sets `Retry-After` |

## Deferred (tracked in `docs/TASKS.md`)

- **#5 key entropy** — documented: keys MUST be `openssl rand -hex 32` (≥256-bit);
  enforcement from a hash alone is impossible, so docs are the control.
- **#6 bearer whitespace** — fixed now (`strings.TrimSpace` on scheme + token).
- **#7 denials bypass the ledger** — partly addressed: denials now emit a
  structured `request denied` log with reason + client; per-client rejection
  counters deferred to the Phase 3 metrics work.
- **#8 failed/billed requests settle at 0** — deferred to the streaming/settlement
  work (most 4xx/timeouts aren't billed today; streaming is rejected).
- **#9 missing controls** — enumerated as backlog: key revocation/expiry + hot
  reload, global (gateway-wide) rate/concurrency cap, per-model/per-provider
  budgets, org/team hierarchy.

## Non-issues (confirmed)
Check order (auth → rate before parse/route) has no bypass; token-bucket burst is
standard; unsalted SHA-256 is fine for high-entropy keys.
