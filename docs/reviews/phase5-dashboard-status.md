# Phase 5 increment 1 (attribution + built-in dashboard) — reviews

**Status: reviews COMPLETE.** The two adversarial expert reviews initially failed
to run (provider session limit) and were re-run once the limit reset. Both
returned **PASS-WITH-NITS, no must-fix**. Nits from both were folded.

## Go review — PASS-WITH-NITS
Verified sound: Record/Snapshot concurrency (both under `l.mu`, value-copies
returned — race-clean under `-race`); ring modular indexing at wraparound and
partial fill; overflow bucket reconciles to the grand total; runID threaded
through all three spend-recording paths (success, error, aborted); `requireAdmin`
correct for auth-off / bad-key / non-admin / admin; `NamedStat` JSON flattens.

Nits folded:
- Overflow/anonymous sentinel keys could collide with a client literally named
  `(other)`/`(anonymous)` → NUL-prefixed sentinels (`\x00(other)`, `\x00(anonymous)`),
  stripped for display.
- `recentN` was `int64` (a theoretical wrap → negative ring index → panic on the
  write path) → changed to `uint64` (index can never be negative).
- `/v1/stats` consumed the admin key's chat rate-limit bucket → added
  `controls.Guard.Authenticate` (auth without touching the bucket); `requireAdmin`
  uses it.
- Test gaps filled: partial-fill newest-first order, overflow reconciles
  cost+tokens, runID on the error record, auth-off `/v1/stats` open.

## Security review — PASS-WITH-NITS
Verified: admin gate correct + sufficient (401/403/200); `/dashboard` shell and
`/metrics` carry no per-tenant data; sessionStorage token handling sound (bearer
only, never URL/log); **stored XSS closed** — every client-controlled value
(`user`, `run_id`, `alias`, …) is `esc()`-escaped before `innerHTML`, all sinks
are text nodes; run-id enumeration + snapshot-sort DoS both neutralized by the
admin gate; exposure + auth-off posture documented.

Nits folded:
- Snapshot sorted under `l.mu` (contends the billing hot path) → sorts now run
  AFTER releasing the lock (only the map/ring copy is under the lock).
- `esc()` didn't escape `'` → added (`&#39;`), future-proofing against a
  single-quoted-attribute sink.
- Fail-open default (a config with no `auth` section serves `/v1/stats` open):
  consistent with the gateway's documented dev posture (chat is open too); the
  shipped `config.example.yaml` ships auth ENABLED and fail-closed.

## Follow-ups (tracked)
- Durable Postgres ledger behind the same `Record` call (the "attribution" half of
  Phase 5 not yet built — this increment is in-memory only).
- Near-limit-run / quota-headroom dashboard panels (needs firewall/broker to
  expose live scope snapshots).
- Optional brief Snapshot cache if poll volume grows.
