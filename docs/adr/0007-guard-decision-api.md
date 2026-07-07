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
  applicable. `reason ∈ {velocity, concurrency, killed}`. (A per-run-$ breach
  auto-kills the run, so it surfaces as `killed`, not a distinct reason.)
- The estimate is the PEP's upper bound: input tokens (countable) + `max_tokens`
  (the output bound) × price. `settle` replaces it with real usage.

## 4. The reservation_id — a signed, stateless token (the key design call)
`reservation_id` is an **HMAC-signed opaque token carrying the minimal reconcile
state**: the scope keys reserved, the concurrency hold id, the held `estUSD/estTokens`,
the run scope key, and an expiry. It is NOT a lookup handle into per-instance memory.

Why signed-stateless, not an in-memory registry keyed by a random id:
- **Replica independence for reconcile.** Any replica can `settle`/`release` a
  reservation another replica issued — it decodes the token and reconciles against
  the SHARED store. A random-id in-memory registry would force sticky routing.
- **No unbounded server state** for the reconcile handle. Tampering is caught by
  the HMAC (a forged/edited token is rejected before it is parsed).

Trade-off: the token is opaque but not tiny (~200–300 bytes). Acceptable — it is
carried by the PEP, not the end user, and never logged with payload.

**Idempotency is NOT free from statelessness** (two adversarial reviews caught
this). A stateless token can be replayed to two replicas; the rebuilt ticket's
own settled/released flags are per-object and give zero cross-call protection. So
settle/release dedup on the token's **nonce** via a SHARED claim — Redis
`SET nonce NX EX(lease)` (`redisstore.ClaimReconcile`) — atomic fleet-wide. The
nonce TTL ≥ the lease, so a live token's nonce is never forgotten. On a dedup-store
error the reconcile applies best-effort + logs (fail-open, matching ADR 0002); the
bounded cost is a possible double-apply of one reservation's estimate during an
outage.

## 5. The lease — orphaned reserves self-heal (REQUIRES the shared store)
A `reserve` with no matching `settle`/`release` (PEP crashed, network dropped) MUST
NOT hold budget forever. Self-heal is real **only in shared mode**, so the guard
API is mounted ONLY when the firewall is backed by the shared store (enforced in
wiring — see §7):
- The concurrency hold is reaped by the shared store's **hold-TTL** (ADR 0002).
- The velocity reserve drains with the rolling 60s window.
- The per-run cumulative reserve is reclaimed when the run scope idles out.
- The token also carries an **expiry** (`reserve_ttl` = request timeout + 60s):
  a reconcile arriving after it is a no-op (the hold already self-healed).

In pure LOCAL mode (no shared store) an orphaned reserve's concurrency hold has NO
reaper — it would pin the slot and the scope-map entry forever (a DoS/OOM). So
local-mode guard is DISALLOWED, not merely discouraged. This is why the primitive
is reserve/settle/**release-with-timeout** rather than a debit/credit that leaks.

## 6. Auth & safety
- **Service credential:** a config flag marks a client as `guard` (or reuse
  `admin`); only such keys may call `/v1/guard/*`. A tenant key must NOT (it could
  reserve/settle arbitrary scopes). Gate identically to the management API.
- **Idempotency:** `settle`/`release` on an already-reconciled or expired
  reservation return `{ ok: true, applied: false }` — never double-apply. Enforced
  by the SHARED nonce claim (§4) + the token expiry, NOT by the per-object ticket
  guards (which don't survive a rebuild). So a PEP retry — even to a different
  replica — is safe.
- **Estimate sanity:** a reserve estimate must be in `[0, 1e6]` USD / `[0, 1e9]`
  tokens (rejected otherwise) so a bad PEP can't poison a scope with a near-infinite
  reserve; a **settle** actual is clamped to ≥0 so a negative actual can't deflate a
  shared counter's slot below this reservation's own contribution.
- **Scope assertion:** `key_sha256` is required (non-empty) and canonicalized
  (lowercase hex) so it matches the inline path.
- **Scope spoofing:** a PEP is trusted to assert scope, but the scope still must
  resolve to a provisioned node (or flat); it cannot invent caps, only spend
  against existing ones. `key_sha256` is validated/canonicalized (lowercase hex).

## 7. Edge cases pinned
- **No run id:** reserve resolves org/team/app only; per-run cap/loop don't apply
  (ADR 0006 §9; the 6.6a governance-gap decision applies here too).
- **Shared store is REQUIRED, not optional.** The guard API mounts only when the
  firewall is backed by the shared store AND a shared reconcile dedup is present
  (enforced in `server.New`; `cmd` warns + disables otherwise). This is what makes
  orphaned-hold reaping and cross-replica idempotency real. A guard secret set
  without `firewall.redis_url` logs a warning and the API stays OFF — never a
  silent, leak-prone local mount.
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
