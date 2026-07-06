# ADR 0005 — Control-plane / data-plane topology (org-wide spend governance)

**Status:** proposed (2026-07)
**Context:** the goal is an **organization-wide** spend & quota control — one
central place that defines budgets and *sees* all AI spend across every team and
agent — not a heave instance bolted onto each app. That is a control-plane /
data-plane split: policy and aggregation are central; enforcement is distributed
to wherever requests are made (via the ADR 0004 contract).

## Decision

Split heave into two roles (deployable from the same binary):

### Control plane (org-central, HA)
Owns the org-wide concerns:
- **Policy & hierarchy** — budgets and caps defined at each level of
  `org ▸ team ▸ app ▸ agent-run`, with inheritance/cascade. Mutable via a **policy
  API**, not a static file, and **distributed** to enforcement points (pull +
  cache + TTL/hot-reload).
- **Identity** — issue keys mapped to a hierarchy node; RBAC (later).
- **Aggregation & governance** — org/team/app spend roll-ups from the durable
  ledger, budgets-vs-actuals, alerts, org-wide kill, chargeback/showback (later).
- **Shared state it owns/exposes** — Redis (live reserve/settle counters) and
  Postgres (durable ledger + policy store).

### Data plane (distributed enforcement points)
The ADR 0004 surfaces — `guard` clients, gateway adapters (LiteLLM guardrail),
`ext_authz`, the Go library, or the inline proxy. Each one:
1. **pulls its effective policy** for the scope chain from the control plane
   (cached; degrades to last-known on a control-plane blip),
2. **reserves/settles** against the shared counters at request time, pre-vendor,
3. and **reports** actual spend into the durable ledger via settle.

```
        governance API / UI  ─────▶  CONTROL PLANE (policy · hierarchy · aggregation · issuance)
                                        │  owns:  Redis (live counters) · Postgres (ledger + policy)
                   pull policy  ◀───────┼───────▶  reserve / settle
        ┌───────────────────────────────┴───────────────────────────────┐
   enforcement point            enforcement point            enforcement point
   (guard in an app)            (LiteLLM guardrail)           (Envoy ext_authz)
```

## Why the core already supports this
heave's **multi-scope reserve/settle is already hierarchical**: a request must fit
under *all* its scopes atomically (today `key` AND `run`; ADR 0002's Lua reserves
across N scopes all-or-nothing). Extending the scope chain to
`org ▸ team ▸ app ▸ run` is the **same mechanism at a larger radius** — a request
is admitted only if it fits under org, team, app, and run budgets simultaneously,
and settles into all of them. Redis is already the org-wide live counter; Postgres
is already the org ledger. So this is an *extension of the primitive*, not a new
engine.

## What is genuinely new
- **Hierarchy/tenancy model** — scope keys and config gain `org/team/app` levels;
  budgets cascade; keys map to nodes.
- **Central policy service** — CRUD + versioned store (Postgres) + a
  distribution/fetch endpoint enforcement points cache. Replaces the org-managed
  parts of the static YAML.
- **Org aggregation & governance** — roll-ups, budgets-vs-actuals, alerts,
  org-wide kill, exports.
- **HA of the control plane** — it is now org-critical; enforcement points must
  degrade safely (fail-open to local/last-known) when it or Redis blips.

## Open-core layering (adoption model)
Keep the easy-to-try wedge separate from the enterprise platform so neither dulls
the other:
- **Open core** — the enforcement engine + `guard`/adapters + single-node firewall.
  `docker compose up`; trivial to adopt; the launch wedge.
- **Enterprise layer** — the control plane (org policy management, hierarchy,
  governance UI). Built on the same open core.

## Non-goals / phasing
- **MVP:** extend scope keys to `org▸team▸app▸run`; a minimal policy API (budgets
  per node, fetched+cached by enforcement points); org/team roll-ups in the
  ledger. Reuse the existing Redis + Postgres substrate.
- **Deferred:** RBAC/SSO, chargeback exports, policy versioning/audit, multi-region
  control plane, a full governance UI.
- Cross-replica velocity/concurrency for the shared caps still relies on the atomic
  Redis reserve/settle (ADR 0002); the control plane does not change that path.
