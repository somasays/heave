# Phase 2R (streaming) — OpenAI-SSE-compat review (adversarial)

**Scope:** SSE wire format + streaming cost/enforcement. **Initial verdict:**
wire PASS for official SDKs, but F1/F2 were real firewall bypasses (FAIL of the
guarantee). **Post-fix:** the bypasses are closed; wire parity improved.

The reviewer confirmed official clients (openai-python, openai-node) consume the
stream correctly: stable `id`, `object`, `created`, `model`, per-chunk `index`,
leading `role` chunk, `data: `/`\n\n` framing, `[DONE]` terminator.

## Must-fix (resolved)

| # | Finding | Resolution |
|---|---------|-----------|
| F1 | Mid-stream client disconnect recorded $0 and released the velocity hold → an agent streams, reads, RSTs before the final chunk = free tokens, no cap tripped | Fixed — abort-after-bytes charges the reserved estimate (fail closed); `TestStreamingAbortChargesEstimate` |
| F2 | Usage-omitting compat backends → cost 0 → free + velocity-free | Fixed — zero-usage success fails closed to the estimate; `TestStreamingUsageOmittedChargesEstimate` |
| F6 | Compat reader swallowed upstream mid-stream error chunks → silent truncated-as-success | Fixed — abort on an `error` chunk; require a completion marker |

## Wire correctness (done)
- **F3** — `finish_reason` is now `*string`, emitting `null` on content chunks
  (present, not omitted) and the reason only on the terminal chunk.
- **F4** — usage now rides a **separate** `choices: []` trailer chunk (OpenAI's
  `include_usage` shape), so LangChain/LlamaIndex usage callbacks see it.

## Deferred (documented)
- **F5** — a mid-stream error after a 200 can only be signaled in-band
  (`data: {"error":...}` + `[DONE]`); official SDKs raise on it, but some
  LangChain/Vercel paths may see a silently truncated completion. No cleaner
  post-200 convention exists; `finishError` now emits a normalized error object.
