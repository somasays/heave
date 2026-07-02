# Phase 2R (streaming) — Go expert review (adversarial)

**Scope:** SSE path — `server.go` (serveStreaming/runCandidates/sseWriter),
`provider` streaming methods. **Verdict:** pass-with-follow-ups → follow-ups
addressed. `-race`/`vet` clean.

The reviewer verified the load-bearing bits are **correct**: header deferral (no
double `WriteHeader`), failover-before-first-byte gating, and settle-before-
release ordering.

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | Zero-usage streamed success settled to $0 → firewall/budget bypass on usage-omitting backends | Fixed — `recordSuccess` fails closed to the reserved estimate when usage is zero; `TestStreamingUsageOmittedChargesEstimate` |
| 2 | Anthropic stream never `Close()`d — latent connection leak | Fixed — `defer stream.Close()` |
| 3 | Truncated compat stream (clean FIN, no `[DONE]`) reported as a successful `stop` | Fixed — require `[DONE]`/terminal finish_reason else return a truncation error; also abort on an upstream error chunk |
| 4 | `finishError`/client-disconnect/mid-stream paths untested | Fixed — delta-scripted fake + `TestStreamingAbort*` / usage-omitted tests |
| 5 | `finishError` leaked raw upstream text/type; `start()` swallowed first-write error | Fixed — `finishError` emits only a normalized type/status; `start()` error now propagates out of `writeDelta` |

Also fixed the paired compat-review finding: a **mid-stream abort/disconnect**
now charges the reserved estimate (not $0), so streaming can't be a free
firewall bypass (`TestStreamingAbortChargesEstimate`).

Deferred: 1 MiB SSE line cap (large single-chunk backends); byte-based (vs
reserved-estimate) settlement for aborted streams.
