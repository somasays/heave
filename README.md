# heave

A self-hostable, OpenAI-compatible LLM gateway: one endpoint in front of many
models, with **cost accounting applied before any request reaches a vendor**.

The differentiator is **cache-aware routing** — most gateways score each turn
independently and switch models freely, silently destroying the per-model prefix
cache. This gateway treats cache warmth as a first-class routing signal: a
conversation stays on its warm-cache model and only becomes eligible to re-route
once it goes quiet long enough for the prefix-cache TTL to lapse. (That's the
Phase 2 wedge; see `docs/TASKS.md`.)

> Status: **Phase 0** — a working OpenAI-compatible proxy with multi-provider
> dispatch and per-request cost accounting. Not yet production-hardened.

## Quick start

```bash
cp config.example.yaml config.yaml       # edit models/providers as you like
export ANTHROPIC_API_KEY=sk-ant-...       # keys come from the environment only
make hooks                                # one-time: install commit gates
make run                                  # build + run on :8080
```

Then call it with any OpenAI client:

```bash
curl localhost:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"fast","messages":[{"role":"user","content":"hello"}]}'
```

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
