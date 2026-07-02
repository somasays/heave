# Phase 1 (failover + redaction) — Go expert review (adversarial)

**Scope:** `internal/health`, `internal/redact`, router `Candidates`, server
dispatch loop. **Verdict:** pass-with-follow-ups. `-race`/`vet` clean; ReDoS
cleared (Go `regexp` is RE2, linear-time — confirmed empirically).

| # | Finding | Sev | Resolution |
|---|---------|-----|-----------|
| 1 | Health breaker poisoned: `RecordFailure` ran for every non-nil error before the retryable/ctx check → client 4xx or client-cancel could open a healthy provider's breaker (a client could DoS a provider's rotation with bad input) | HIGH | Fixed — failure is recorded only for retryable, non-canceled, non-429 errors; terminal/canceled errors break without touching health. Tests `TestClientErrorDoesNotPoisonBreaker`, `Test429FailsOverWithoutOpeningBreaker` |
| 2 | Credit-card regex redacted any 13–16 digit run (no Luhn) → corrupted order ids/quantities | MED | Fixed — Luhn gate on the CC rule; `TestCreditCardLuhnGate` proves order-id runs survive, valid cards redact |
| 3 | Success inferred from `presp == nil`; a `(nil,nil)` provider return mis-classified | LOW | Fixed — explicit `served` sentinel driven by `err == nil` |
| 4 | `New` didn't nil-guard the new `Deps` pointer fields (nil `Redactor` → panic) | LOW | Fixed — `New` defaults Guard/Health/Redactor/Log |

**Follow-ups (tracked):** single shared deadline across attempts (a slow primary
starves fallbacks) → per-attempt sub-deadline in Phase 2; CC regex still
under-matches 17+ digit runs (documented best-effort). Added health concurrency
test to make `-race` meaningful for the breaker.
