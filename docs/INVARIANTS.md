# Architectural Invariants

These are the rules the codebase upholds. They are enforced mechanically where
possible — the enforcement matrix at the bottom says exactly which tool enforces
each one — and by adversarial review otherwise. Changing an invariant is a
deliberate act: update this file in the same change, with rationale.

The single verification command `make check` (→ `scripts/check.sh`) runs every
mechanical check, and is also the pre-commit hook and the entire CI job. Style
conventions live in `docs/STYLE.md`; this project also validates the essay
["Engineering for AI Agents"](https://theprincipledengineer.substack.com/p/engineering-for-ai-agents)
lever-by-lever in `docs/ARTICLE-VALIDATION.md`.

---

## 1. Design invariants

**#1 — One ingress format.** Clients speak the OpenAI Chat Completions format
(`internal/openai`). This is the only public request/response shape; vendor
formats never appear outside adapters. *Why:* a single familiar surface is the
point of a gateway; leaking vendor shapes inward couples the core to every
vendor at once.

**#2 — Vendors only through adapters.** All vendor protocol code lives in
`internal/provider`. No other package imports a vendor SDK or references a vendor
endpoint host. *Why:* provider boundaries are the seams that let failover,
redaction, and cost controls apply uniformly.

**#3 — Routing goes through the router; cache-awareness is the wedge.** Model
selection happens in `internal/router.Route`. The naive approach scores each turn
independently and switches models freely, silently destroying the per-model
prefix cache. The router instead treats cache warmth as a first-class signal: a
conversation stays on its current model while the cache is warm and only becomes
eligible to re-route once it goes quiet long enough for the TTL to lapse. *Why:*
this is the feature that justifies the project over LiteLLM/OpenRouter.

**#4 — Secrets from the environment.** API keys are resolved from environment
variables named in config; they never appear in code, config files, or logs.

**#5 — Every request is accounted.** Every dispatched request is recorded via
`internal/ledger` (success or failure), including cache tokens, so spend is
always attributable. An unrecorded request is invisible spend.

**#6 — Config is declarative data.** Model routing, prices, and per-model policy
(max output, sampling support) live in YAML, not code, so operators and the
community can maintain them without a code change.

**#7 — Controls apply before the vendor.** Authentication, per-client rate
limits, and budget caps are checked in `internal/controls` and reject the request
*before* it reaches a provider — the gateway spends its own CPU rejecting abuse,
never a vendor's billed tokens. Client keys are stored only as SHA-256 hashes
(never plaintext) and MUST be high-entropy random. Budget uses a reserve/settle
hold (an upper-bound estimate is reserved before dispatch, reconciled after) so
concurrent requests cannot all pass a stale pre-check and overshoot the cap.
*Why:* a cost/abuse control that runs after the vendor call has already lost the
money it was meant to save. *Caveat:* limits are in-memory and per-instance — N
replicas allow N× the configured RPM/budget until the shared store lands
(Phase 3); documented on the config fields and in `controls`.

---

## 2. Architecture invariants

**#A1 — Layered, one-directional dependencies.** Imports flow one way. A package
may only import packages below it; there are no cycles.

```
cmd/heave            (composition root — wires everything, reads config)
    │  imports ▼
internal/server        (request flow: admit ▸ translate ▸ route ▸ dispatch ▸ account)
    │  imports ▼
internal/openai  internal/router  internal/ledger  internal/provider  internal/controls
  (wire types)     (routing)        (accounting)      (vendor adapters)   (auth/rate/budget)
        │               │                │                  │                  │
        └── stdlib ─────┴────────────────┴──────────────────┴──────────────────┘
                                                    vendor SDKs ┘ (provider only, outward)

internal/config        (loaded only by cmd/heave)
```

Concretely:
- `internal/openai`, `internal/router`, `internal/ledger` are **pure**: stdlib
  only, no other internal package. They are trivially unit-testable in isolation.
- `internal/config` imports stdlib + the YAML lib only, and is imported **only**
  by `cmd/heave`.
- `internal/provider` faces **outward** (vendor SDKs) and must not import the
  core inward (`server`/`router`/`ledger`/`config`/`openai`) — so Phase 1 can
  wrap adapters with failover/redaction without a dependency inversion.
- `internal/server` may use `openai`/`provider`/`router`/`ledger`, but **not**
  `config`.
*Enforced:* `depguard` in `.golangci.yml` (one rule per layer) + `check_arch.sh`
for the vendor-boundary and secret rules.

**#A2 — The composition root is `cmd/heave`.** Only `cmd/heave` constructs
concrete implementations and wires them together. Library packages accept
interfaces/values through `New(...)` constructors and never self-wire or reach
for globals. *Why:* dependencies are visible and swappable; tests inject fakes.

**#A3 — Provider is an interface, owned by the consumer side.** `provider.Provider`
is defined where it is used (the boundary), not in each adapter. Adapters are
values behind it. Adding a vendor = one new file implementing the interface, no
changes elsewhere — the intended first contribution.

**#A4 — No package-level mutable state.** No mutable globals; state is held in
constructed values (`Server`, `Ledger`, ...). Constants and pure lookup tables
are fine. *Why:* globals defeat testability and create hidden coupling and races.
*Enforced:* `gochecknoglobals`.

**#A5 — Context propagates; the deadline is imposed once.** I/O functions take
`ctx` first and pass the caller's context downward. The single per-request
deadline is set in the server and flows to the provider; adapters never invent a
`context.Background()` mid-request. *Enforced:* `noctx` (outbound requests carry
a context) + review.

---

## 3. Error-handling invariants

**#E1 — Errors are values, checked and wrapped.** Every returned error is handled;
wraps use `%w` and add *what failed*. No `panic` for control flow or bad input.
*Enforced:* `errcheck`, `govet`; review for wrap quality.

**#E2 — Provenance survives the boundary.** Vendor failures cross as
`*provider.Error{StatusCode,Type,RetryAfter}`. The server maps upstream 4xx →
same 4xx, timeouts → 504, everything else → 502, and never launders a client's
own bad request into a gateway-fault status. *Why:* clients back off / fix inputs
correctly only if status is honest. *Enforced:* `server` + `provider` tests.

**#E3 — Unsupported is rejected, never silently dropped.** A capability the
gateway does not implement (streaming, tools, `n>1`, image parts) returns a clear
`400`, so the gateway never pretends to honor what it discards.

---

## 4. Concurrency invariants

**#C1 — Shared state is guarded; the race detector stays clean.** Mutable shared
state (e.g. the ledger totals) is mutex-guarded; the router map is build-once /
read-only. `go test -race` runs in the gate and must be green. *Enforced:*
`go test -race`.

**#C2 — Goroutines have an owner and an exit.** Every goroutine terminates on
context cancellation or a closed channel; no fire-and-forget. Graceful shutdown
cancels in-flight work via a cancelable base context rather than hanging.

---

## 5. Observability invariants

**#O1 — Structured logs only.** Logging is `log/slog` key/value pairs, never
`fmt.Print*` or interpolated sentences, so logs stay queryable (article lever #9).
*Enforced:* `forbidigo`.

**#O2 — Never log secrets or full prompt bodies.** Ledger/records log identifiers,
token counts, cost, latency, status — not API keys or message content. *Enforced:*
review (candidate for a future automated check).

---

## 6. Security invariants

**#S1 — No secrets in the repo.** Keys come from the environment (Invariant #4);
key-shaped literals in Go are rejected. *Enforced:* `check_arch.sh` +
`.gitignore` (`.env`, `config.yaml`).

**#S2 — Untrusted input is bounded.** Request bodies are size-capped before
decode; upstream response bodies are read through an `io.LimitReader`. *Why:* an
unauthenticated caller (or a hostile upstream) must not be able to OOM the
process. *Enforced:* `bodyclose` (bodies closed) + review/tests for the caps.

---

## 7. Style invariants

Full Go style/convention rules — naming, error wrapping, comments, tests — are in
`docs/STYLE.md` and enforced by `gofmt`, `revive`, `misspell`, `unconvert`,
`staticcheck`, and the linters above.

---

## Phase gate (definition of done)

A phase is not "done" until BOTH adversarial expert reviews have run and their
findings are addressed or explicitly waived:

1. **LLM-apps expert review** — LLM integration correctness: prompt/cache
   semantics, token accounting, streaming, provider quirks, failover, cost logic.
2. **Go expert review** — idiomatic Go, concurrency safety, error handling,
   context propagation, API design, resource lifecycle, test quality.

Reviews are adversarial: reviewers try to break the design, not bless it. Record
each under `docs/reviews/<phase>-<discipline>.md` with findings + resolution. A
phase's task in `docs/TASKS.md` may only be marked done once both are logged.
This is the one deliberately-human gate: CI enforces structure; it cannot enforce
good taste (article lever #12).

---

## Enforcement matrix

Fail-cheap ordering inside `scripts/check.sh`: gofmt → architecture grep → build
→ golangci-lint → `go test -race`.

| Invariant | Enforced by |
|---|---|
| #1 ingress format / #A1 layering | `depguard` (per-layer rules) + review |
| #2 vendors only in provider / #S1 no secrets | `scripts/check_arch.sh` (grep) |
| #3 routing through router | review (structural today; behavioral in Phase 2 benchmarks) |
| #4 secrets from env | `check_arch.sh` + `.gitignore` |
| #5 every request accounted | `server`/`ledger` tests + review |
| #6 config is data | `config` validation + review |
| #7 controls before vendor | `controls`/`server` tests (auth/rate/budget) + `depguard` (controls-pure) |
| #A2 composition root / #A3 interface ownership | `depguard` + review |
| #A4 no global mutable state | `gochecknoglobals` |
| #A5 context propagation | `noctx` + review |
| #E1 errors checked/wrapped | `errcheck`, `govet`, `staticcheck` |
| #E2 error provenance / #E3 reject-don't-drop | `server` + `provider` tests |
| #C1 race-clean | `go test -race` |
| #C2 goroutine lifecycle | review |
| #O1 structured logs | `forbidigo` |
| #O2 no secrets in logs | review |
| #S2 bounded input | `bodyclose` + tests/review |
| style (docs/STYLE.md) | `gofmt`, `revive`, `misspell`, `unconvert`, `staticcheck` |
| commit authorship policy | `.githooks/commit-msg` |
| phase gate | adversarial review (`docs/reviews/`) |
