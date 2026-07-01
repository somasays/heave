# Phase 0 — Go expert review (adversarial)

**Scope:** Phase 0 foundation, initial scaffold (pre-commit).
**Initial verdict:** FAIL. **Post-fix verdict:** pass-with-follow-ups.

The SDK usage (`anthropic-sdk-go` v1.55.0 value client, param types, usage
fields) was confirmed correct. The flagged concurrency "bait" (router map, ledger
mutex) was clean. Real defects were in HTTP hardening, error taxonomy, and test
coverage.

| # | Finding | Severity | Resolution |
|---|---------|----------|-----------|
| 1 | Unbounded request body → OOM DoS | blocker | Fixed — `http.MaxBytesReader` + 413 (`server.go`), `max_request_bytes` config |
| 2 | No server timeouts / no enforced request deadline | blocker | Fixed — `Read/Write/Idle` timeouts (`main.go`) + per-request `context.WithTimeout` (`server.go`), `request_timeout` config |
| 3 | Upstream 4xx laundered into 502 | high | Fixed — typed `provider.Error{StatusCode,Type,RetryAfter}`; `classifyError` preserves 4xx |
| 4 | `client.Timeout` misreported as 502 not 504 | high | Fixed — `classifyError` checks `ctx.Err()` / `DeadlineExceeded` / `os.IsTimeout` → 504 |
| 5 | No tests for server/config/providers; `check.sh` lacked `-race` | high | Fixed — `-race` added; `httptest` tests for server + openaicompat + config + anthropic normalization |
| 6 | `top_p` silently dropped | medium | Fixed — `TopP` on `provider.Request`, forwarded by both adapters |
| 7 | Shutdown drain-timeout → `os.Exit(1)`; in-flight not cancelable | medium | Fixed — cancelable `BaseContext`; drain timeout logs + `Close()`, returns nil (exit 0) |
| 8 | Unbounded read of upstream body | medium | Fixed — `io.LimitReader(resp.Body, 16 MiB)` |
| 9 | Duplicate provider names allowed; `rand.Read` err ignored | low | Fixed — duplicate name/alias/default validated in config. `crypto/rand.Read` never fails on modern Go — left as-is (noted). |

**Follow-ups tracked in `docs/TASKS.md`:** retry policy on the OpenAI-compat path
(reliability phase); the Anthropic SDK's own ~10-min default timeout is now
capped by the gateway deadline but not separately tuned.
