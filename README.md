# heave

A self-hostable, OpenAI-compatible LLM gateway: one endpoint in front of many
models, with **cost accounting applied before any request reaches a vendor**.

The differentiator is being a **runtime spend & quota firewall for agentic
traffic**: hard, real-time controls enforced *before* a request reaches a vendor.
Monthly budgets don't stop a runaway agent burning five figures overnight, and
"failover after a 429" doesn't arbitrate a provider quota shared across ten
teams. heave's job is the enforcement point that does — token-velocity caps
($/min per key/run), per-run kill switches, loop/anomaly detection, concurrency
caps, and provider-quota brokering, on a single fast Go data-plane binary.

> We spiked cache-aware routing as the original wedge and measured it honestly:
> ~10–13%, concentrated in long conversations (see `docs/BENCHMARK.md`). Real,
> but not a headline — it lives on as a cache-efficiency *observability* signal,
> not the reason to deploy heave.

> Status: **Phase 0** — a working OpenAI-compatible proxy with multi-provider
> dispatch and per-request cost accounting. Not yet production-hardened.

## Quick start

```bash
cp config.example.yaml config.yaml        # edit models/providers as you like
export ANTHROPIC_API_KEY=sk-ant-...        # vendor keys come from the env only
make hooks                                 # one-time: install commit gates
make run                                   # build + run on :8080
```

The example config **ships with gateway auth enabled and fails closed** — a
placeholder client rejects everyone until you add your own key. Mint one and set
its hash in `config.yaml`:

```bash
KEY=$(openssl rand -hex 32)
echo "gateway key: $KEY"
printf '%s' "$KEY" | shasum -a 256 | cut -d' ' -f1   # → put under clients[].key_sha256
```

Then call it with any OpenAI client, sending that key as the bearer:

```bash
curl localhost:8080/v1/chat/completions \
  -H "authorization: Bearer $KEY" \
  -H 'content-type: application/json' \
  -d '{"model":"fast","messages":[{"role":"user","content":"hello"}]}'
```

For trusted local-only use, set `auth.enabled: false` in `config.yaml` (a loud
warning is logged, and the bearer header is then optional).

Streaming works too — add `"stream": true` and you get an OpenAI-compatible SSE
stream (`chat.completion.chunk` events + a `[DONE]` terminator).

`GET /healthz` for liveness, `GET /metrics` for running request/token/cost
totals.

### Docker

```bash
docker compose up            # gateway only (Phase 0 needs no external state)
docker compose --profile state up   # + Redis and Postgres for later phases
```

## What's here

- OpenAI-compatible `POST /v1/chat/completions` (non-streaming for now).
- Adapters for Anthropic (official SDK) and any OpenAI-compatible vendor
  (OpenAI, OpenRouter, ...). Adding a provider is the intended first
  contribution.
- Static router + declarative price table; per-request cost accounting to
  structured JSON logs.

## Contributing & project rules

Read `CLAUDE.md` (short) and `docs/INVARIANTS.md` (full). Key points:

- One verification command: `make check` (gofmt → architecture → vet → build →
  test, ordered to fail cheap). It is also the pre-commit hook and the whole CI
  job.
- Architectural boundaries are enforced mechanically (`scripts/check_arch.sh`).
- Commit messages must be plain and human-authored (no AI-attribution
  trailers) — enforced by `.githooks/commit-msg`.
- Each phase ships only after two adversarial reviews (LLM-apps + Go); logs in
  `docs/reviews/`.

This project also doubles as a validation of the essay
[Engineering for AI Agents](https://theprincipledengineer.substack.com/p/engineering-for-ai-agents)
— see `docs/ARTICLE-VALIDATION.md`.

## License

Apache-2.0.
