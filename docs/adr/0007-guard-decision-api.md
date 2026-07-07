# ADR 0007 — The `/v1/guard/*` decision API

**Status:** proposed (2026-07) · concretizes ADR 0004 for HTTP integrators
**Context:** ADR 0004 pins the integration *contract* (`reserve/settle/release` +
the `guard` primitive) and says the only new server-side state is "a TTL'd registry
mapping `reservation_id -> held ticket/lease`." Today that contract is reachable
ONLY inline: to use the engine you must send the actual chat completion *through*
heave (`POST /v1/chat/completions`), so heave sits in the data path. The whole
PDP/OOB story (LiteLLM `CustomGuardrail`, Envoy `ext_authz`, a Go/Python library)
is pinned on exposing the SAME engine as a **pure decision** — scope + a number,
never the payload. This ADR specifies that HTTP surface.

## 1. Decision — three verbs, one thin layer over the built engine
```
POST /v1/guard/reserve   { scope, estimate } -> { admitted, reservation_id, http_status, reason, binding_node, retry_after_sec }
POST /v1/guard/settle    { reservation_id, actual }          -> { ok }
POST /v1/guard/release   { reservation_id }                  -> { ok }
```
`reserve` runs the full pre-vendor check-and-hold; `settle` reconciles the held
estimate to real usage; `release` frees a call that never billed. They map 1:1 onto
`firewall.EnterChain` (via the resolver) / `Ticket.Settle` / `Ticket.Release` — the
engine is unchanged; this is a transport + a reservation lifecycle around it.

**heave stays OUT of the data path.** The reserve call carries a scope and an
estimate (bytes: a few hundred), never the prompt/response. The LLM traffic flows
PEP→vendor directly. This is what makes heave a decision point, not a proxy hop.

## 2. Who calls it, and how scope is identified
The caller is a **trusted PEP** (LiteLLM, Envoy, a sidecar) acting on behalf of
many tenants — not the end tenant. So:
- The endpoints are authenticated by a **service credential** (an admin-class key;
  see §6). Only trusted infrastructure may ask heave for a decision.
- The **tenant scope is in the request body**, not derived from the caller's
  identity: `scope = { key_sha256, run_id }` (optionally `{ org, team, app, run }`
  explicitly). heave resolves the chain via `enforcer.Resolve(key_sha256, run_id)`
  — the exact same path the inline handler uses (6.5) — so a governed key gets
  per-scope caps + node kills, an unprovisioned key falls back to flat, and a
  resolution failure fails CLOSED (`admitted=false`).

This keeps ONE resolution/enforcement path for both inline and OOB — no second
enforcement engine to drift.

## 3. `reserve` — request / response
```jsonc
// request
{ "key_sha256": "<hex>", "run_id": "run-abc",
  "estimate": { "usd": 0.004, "tokens": 1200 } }
// response — admitted
{ "admitted": true, "reservation_id": "<opaque token>" }
// response — denied
{ "admitted": false, "http_status": 429, "reason": "velocity",
  "binding_node": "team:eng", "retry_after_sec": 60 }
```
- `http_status` is what the PEP should return to its caller: **429** (budget/
  velocity/concurrency — retryable) or **403** (killed — terminal). Mirrors the
  inline deny semantics (ADR 0006 §6).
- `binding_node` names the scope that denied (actionable); omitted when not
  applicable. `reason ∈ {velocity, concurrency, killed, per_run_budget}`.
- The estimate is the PEP's upper bound: input tokens (countable) + `max_tokens`
  (the output bound) × price. `settle` replaces it with real usage.

## 4. The reservation_id — a signed, stateless token (the key design call)
`reservation_id` is an **HMAC-signed opaque token carrying the minimal reconcile
state**: the scope keys reserved, the concurrency hold id, the held `estUSD/estTokens`,
the run scope key, and an expiry. It is NOT a lookup handle into per-instance memory.

Why signed-stateless, not an in-memory registry keyed by a random id:
- **Replica independence.** In shared mode (Redis scope store, ADR 0002) any
  replica can `settle`/`release` a reservation another replica issued — it decodes
  the token and reconciles against the SHARED store. A random-id in-memory registry
  would force sticky routing (reserve and settle must hit the same replica), which
  a PEP behind a load balancer cannot guarantee.
- **No unbounded server state.** No registry to size/evict/OOM; the PEP holds the
  handle. Tampering is caught by the HMAC (a forged/edited token is rejected).
- Single-instance (no Redis) deployments work identically against local state.

Trade-off: the token is opaque but not tiny (~200–300 bytes). Acceptable — it is
carried by the PEP, not the end user, and never logged with payload.

## 5. The lease — orphaned reserves self-heal
A `reserve` with no matching `settle`/`release` (PEP crashed, network dropped) MUST
NOT hold budget forever:
- The token carries an **expiry** (`reserve_ttl`, default = request timeout + 60s).
- The concurrency hold is reaped by the shared store's hold TTL (already built,
  ADR 0002); the velocity reserve drains with the rolling 60s window; the
  per-run cumulative reserve is reclaimed when the run scope idles out (the
  documented "active lifetime" bound, firewall.go).
- So a missing settle over-reserves for at most the lease/window, then self-heals.
  This is why the primitive is reserve/settle/**release-with-timeout**, not a
  debit/credit that leaks on a dropped call.

## 6. Auth & safety
- **Service credential:** a config flag marks a client as `guard` (or reuse
  `admin`); only such keys may call `/v1/guard/*`. A tenant key must NOT (it could
  reserve/settle arbitrary scopes). Gate identically to the management API.
- **Idempotency:** `settle`/`release` on an already-settled, already-released, or
  expired reservation return `{ ok: true, applied: false }` — never double-apply
  (the `Ticket` settled/released guards + the token expiry enforce this). So a PEP
  retry is safe.
- **Estimate sanity:** negative estimates are clamped to 0 (firewall already does
  this); an absurd estimate only over-reserves the caller's own scope.
- **Scope spoofing:** a PEP is trusted to assert scope, but the scope still must
  resolve to a provisioned node (or flat); it cannot invent caps, only spend
  against existing ones. `key_sha256` is validated/canonicalized (lowercase hex).

## 7. Edge cases pinned
- **No run id:** reserve resolves org/team/app only; per-run cap/loop don't apply
  (ADR 0006 §9; the 6.6a governance-gap decision applies here too).
- **Cross-replica settle without a shared store:** a single-instance deployment is
  fine; a multi-replica deployment WITHOUT Redis cannot guarantee settle lands on
  the reserving replica — so the guard API requires the shared store for HA, same
  precondition as cross-replica velocity/concurrency (ADR 0002). Documented, warned.
- **Streaming:** one `settle` at stream end with the final usage; interim chunks
  don't settle.
- **Double reserve for one logical call:** each reserve is independent; a PEP that
  reserves twice must settle/release twice. The `guard` wrapper (ADR 0004) makes
  this automatic.

## 8. What this unblocks (the follow-on tasks)
- **7.2** builds these three endpoints + the signed reservation token over the
  existing `EnterChain/Settle/Release`.
- **7.3** ships the LiteLLM `CustomGuardrail` adapter: `pre_call → reserve`,
  `post_success → settle`, `post_failure → release`, threading `reservation_id`
  through `data["metadata"]`.
- Envoy `ext_authz` and a Go/Python client library are the same three calls.

## Consequences
- ONE enforcement path (resolver + EnterChain) serves inline AND every OOB
  integration — no drift between "the proxy" and "the decision API".
- heave becomes a true PDP: consulted, not interposed. The inline proxy remains as
  the zero-integration option; `/v1/guard/*` is the option for orgs that already
  run a gateway and won't accept a second data-path hop.

## Alternatives considered
- **In-memory reservation registry keyed by a random id** — rejected: forces
  sticky routing or a shared registry; the signed-stateless token gets replica
  independence for free and adds no server state.
- **Deriving scope from the caller's own credential** — rejected: the PEP acts for
  many tenants; scope must be asserted per-request, with the PEP as trusted infra.
- **A LiteLLM-specific endpoint** — rejected (ADR 0004): the contract is
  host-neutral; LiteLLM is one adapter over the same three verbs.
