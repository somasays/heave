# Phase 0 — LLM-apps expert review (adversarial)

**Scope:** Phase 0 OpenAI-compatible proxy (Anthropic + OpenAI-compat), pre-commit.
**Initial verdict:** FAIL. **Post-fix verdict:** pass-with-follow-ups.

The proxy claimed to be an honest OpenAI-compatible front end but broke on
first-request-level inputs and failed to capture the very signal (cache tokens)
the project's thesis depends on.

## Must-fix (all resolved)

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | `temperature` forwarded to models that 400 on it (Opus 4.8 / Sonnet 5); `top_p` dropped | Fixed — per-model `sampling: false` capability in config; server strips temp/top_p for such models; `top_p` now forwarded when accepted |
| 2 | `message.content` only decoded as string → hard-fails on parts arrays (vision + many SDKs) | Fixed — `MessageContent` custom unmarshal accepts string / null / parts array; text flattened |
| 3 | `tools`/`functions`/`response_format`/`n>1` silently dropped | Fixed — rejected with a clear 400 (`unsupported()`), never silently discarded |
| 4 | Anthropic request not normalized → system-only / consecutive-role / empty-content inputs 400 | Fixed — `toAnthropicMessages` coalesces same-role turns, drops empty content, requires first=user, else clean 400 |
| 5 | Cache token usage neither captured nor priced (defeats the thesis + Invariant #5) | Fixed — `CacheRead/WriteInputTokens` on `provider.Response`, populated from Anthropic usage, priced in `ledger.Cost` (0.1× / 1.25× input), logged |
| 6 | `max_tokens` default of 4096 silently truncates | Fixed — per-model `max_output_tokens` default from config |
| 7 | Upstream status flattened → 429/`Retry-After` lost | Fixed — status + `Retry-After` propagated (see Go review #3/#4) |
| 11 | `developer` role became a user turn | Fixed — `developer` hoisted into system alongside `system` |

## Deferred (tracked in `docs/TASKS.md`, not shipped as done)

- **#8** OpenAI-compat records zero cost when upstream omits `usage` — no longer
  crashes; documented; flag/estimate in the cost-hardening phase.
- **#9** Retry asymmetry between adapters — deadline now enforced; retry on the
  compat path deferred to the reliability phase.
- **#10** No auth on the gateway itself — **Phase 1 blocker** (API-key check).
- **#11 (rest)** `name` field, `system_fingerprint`, `param` in the error
  envelope — low priority; `stream:true` returns an explicit 400 (acceptable).
- Sending `cache_control` breakpoints is deferred to Phase 2 (the caching phase);
  Phase 0 captures usage, which is the prerequisite that could not wait.
