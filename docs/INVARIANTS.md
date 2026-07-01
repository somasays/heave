# Architectural Invariants

These are the rules the codebase upholds. They are enforced mechanically where
possible (`make check` → `scripts/check.sh` → `scripts/check_arch.sh`, run by
the pre-commit hook and CI) and by review otherwise. Changing an invariant is a
deliberate act: update this file in the same change, with rationale.

This project also serves as a working validation of the article
["Engineering for AI Agents"](https://theprincipledengineer.substack.com/p/engineering-for-ai-agents);
see `docs/ARTICLE-VALIDATION.md` for the lever-by-lever mapping.

## Design invariants

**#1 — One ingress format.** Clients speak the OpenAI Chat Completions format
(`internal/openai`). This is the only public request/response shape. Vendor
formats never appear outside adapters. *Why:* a single, familiar surface is the
whole point of a gateway; leaking vendor shapes inward couples the core to every
vendor at once.

**#2 — Vendors only through adapters.** All vendor protocol code lives in
`internal/provider`. No other package imports a vendor SDK or references a vendor
endpoint host. *Enforced:* `scripts/check_arch.sh`. *Why:* provider boundaries
are the seams that let failover, redaction, and cost controls apply uniformly.

**#3 — Routing goes through the router; cache-awareness is the wedge.** Model
selection happens in `internal/router.Route`. The naive approach scores each
turn independently and switches models freely, silently destroying the per-model
prefix cache. The router instead treats cache warmth as a first-class signal: a
conversation stays on its current model while the cache is warm and only becomes
eligible to re-route once it goes quiet long enough for the TTL to lapse. *Why:*
this is the feature that justifies the project existing over LiteLLM/OpenRouter.

**#4 — Secrets from the environment.** API keys are resolved from environment
variables named in config; they never appear in code, config files, or logs.
*Enforced:* `scripts/check_arch.sh` (rejects key-shaped literals in Go).

**#5 — Every request is accounted.** Every dispatched request is recorded via
`internal/ledger` (success or failure), so spend is always attributable. *Why:*
cost tracking is a core promise; an unrecorded request is invisible spend.

**#6 — Config is declarative data.** Model routing and prices live in YAML, not
code, so the community can maintain the price table without a code change.

## Phase gate (definition of done)

A phase is not "done" until BOTH adversarial expert reviews have run and their
findings are addressed or explicitly waived:

1. **LLM-apps expert review** — correctness of the LLM integration: prompt/cache
   semantics, token accounting, streaming, provider quirks, failover behavior,
   cost-control correctness.
2. **Go expert review** — idiomatic Go, concurrency safety, error handling,
   context propagation, API design, resource lifecycle, test quality.

Reviews are adversarial: reviewers try to break the design, not bless it. Record
each review (findings + resolution) under `docs/reviews/<phase>-<discipline>.md`.
A phase's task in `docs/TASKS.md` may only be marked done once both are logged.

## How enforcement is layered (fail fast, fail cheap)

`scripts/check.sh` runs checks cheapest-first so failure is cheap:
`gofmt` → architecture grep → `go vet` → `go build` → `go test`. The same script
is the pre-commit hook and the entire CI job — one source of truth for "OK".
