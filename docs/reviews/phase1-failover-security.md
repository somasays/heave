# Phase 1 (failover + redaction) — gateway-security review (adversarial)

**Scope:** failover dispatch, `internal/redact`, `internal/health`, budget
interaction. **Verdict:** pass-with-follow-ups.

## Must-fix (resolved)

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | Provider-side 401/403 was terminal AND laundered to the client as `authentication_error` → a rotated gateway key hard-fails the alias and blames the client | Fixed — 401/403 are retryable-for-failover; `classifyError` maps an upstream 401/403 to 502 ("provider rejected the gateway's credentials"), never a client 401. `TestProviderAuthMapsTo502NotClient401` |
| 2 | Failover under-counted vendor spend: a primary that failed after generating tokens was booked at $0, then the fallback billed again (invisible double-spend, breaks Invariant #5) | Fixed — every failed attempt is now recorded in the ledger (per-provider); documented that a failed attempt's exact vendor tokens are unknown |
| 3 | "API keys" built-in only matched `sk-`/`sk-ant-`; "scrubs secrets" overclaimed | Fixed — added AWS, GitHub, GCP, Slack, JWT, and PEM private-key detectors; `TestSecretFamilies` |
| 4 | Cross-provider failover silently changed the data subprocessor; client told it was the primary | Fixed — response carries `X-Heave-Provider` / `X-Heave-Upstream` (actual served); documented that cross-provider failover = cross-subprocessor. `TestServedProviderHeader` |
| 5 | Budget bound broke if a fallback was pricier than the primary (reserve was primary-priced) | Fixed — reserve at the MAX estimate across the candidate chain (`maxEstimateUSD`) |
| 6 | The `user` field bypassed redaction and persisted raw in the ledger | Fixed — `redactRequest` scrubs `req.User` too when redaction is on |
| 7 | A brief 429 could open the breaker and evacuate all traffic to fallbacks (cascade) | Fixed — 429 fails over but does NOT count toward the breaker |

## Deferred (tracked in `docs/TASKS.md`)

- Per-alias "no cross-provider failover" flag for strict data-residency contracts.
- Per-attempt sub-deadline (today one deadline is shared across attempts).
- Redaction remains regex best-effort — names/addresses/free-form PII are out of
  scope by design; documented, not claimed.

## Non-issues (confirmed)
Redaction runs once before all attempts (fallbacks see scrubbed content) and
before any logging/ledger; system messages are covered; no ReDoS (RE2);
per-instance breaker documented.
