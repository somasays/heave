# ADR 0006 — Hierarchical budgets & the resolution protocol

**Status:** proposed (2026-07) · spec for the control-plane MVP (ADR 0005)
**Context:** the org-wide control plane must let operators set a budget at *any*
level of `org ▸ team ▸ app ▸ run` and have every enforcement point honor them
together — e.g. a team with 10 apps, each with its own budget *and* the team with
its own. This ADR pins the **semantics** (how nested budgets compose) before any
code. The runtime mechanism is the atomic multi-scope reserve (ADR 0002); this is
about what the numbers *mean*.

## 1. Hierarchy & identity
- **Provisioned nodes:** `org ▸ team ▸ app`. **Dynamic node:** `run` (supplied per
  request via `X-Heave-Run-Id`, never provisioned).
- A **key** (bearer) belongs to exactly one node — usually an app, sometimes a
  team. A request resolves to a **scope chain**: the key's node + its ancestors +
  the run. Example: a key on `app-3` → chain `[org, team-A, app-3, run-r]`.

## 2. Budget model — **umbrella / shared-pool** (the decision)
Each node's budget is an **independent cap on the aggregate spend at and under
that node.** Not a partition, not a sum of children.

- A **team** budget caps the *total* of all its apps (a shared pool + a ceiling).
- An **app** budget caps that app.
- A request is admitted **iff it fits under every node in its chain at once**, and
  is denied by whichever is **tightest** (the *binding node*).

**Worked example** — team-A = `$1,000/day`; its 10 apps each = `$200/day`:
- app-3 spends freely until **either** app-3 hits `$200` **or** team-A's shared
  pool hits `$1,000` — whichever comes first.
- If apps 1–5 burn `$1,000` between them, **app-6 is denied even with $200 of its
  own budget unused** — the team umbrella is exhausted.
- app-3 is denied at its own `$200` **even if the team pool has room.**
- The app budgets **need not sum to** the team budget. Σ(apps)=$2,000 > $1,000 is
  a *legal, useful* config: generous per-app caps under a hard team umbrella,
  first-come-first-served on the shared pool.

Why this model: it's exactly the atomic multi-scope reserve (already built), needs
no allocation bookkeeping, and matches how finance thinks ("a budget at level X
caps everything under X"). Strict partitioning with per-app *guarantees* is a
different, heavier model — see §9.

## 3. Budget dimensions & windows
A node's budget is a **set** of limits; 0/unset = unconstrained at that node for
that dimension. Reuses the existing engine:

| Dimension | Window | Reset |
|---|---|---|
| `$/min`, `tokens/min` (velocity) | rolling 60s | rolling |
| `$/day`, `$/month` (the "budget") | calendar | midnight / 1st, in the node's tz (default UTC) |
| `max_concurrent` | instantaneous | — |
| `max_usd_per_run` | per run's active life | applies to every run under the node |

`max_usd_per_run` set at team/app level applies to **each** run beneath it ("no run
under team-A may exceed $50") — it's a per-run cap distributed by an ancestor.

## 4. The resolution protocol
Given a request with chain `C = [org, team, app, run]` and estimate `est`:

- **reserve(C, est):** ATOMICALLY, for each node in `C` with a cap on the relevant
  dimension, check `window_spend(node) + est ≤ cap(node)`. If **all** pass →
  add `est` to every node's counter, admit. If **any** fails → reserve nothing
  (all-or-nothing) and return `{admitted:false, binding_node, dimension}`.
- **settle(C, actual):** apply `actual − est` to every node's counter.
- **release(C, est):** subtract `est` from every node's counter (failed call).
- **All levels are enforced independently** — no collapsing to "min ancestor." A
  child may cap tighter *or looser* than a parent; each is checked, the binding
  one denies.

This is ADR-0002's Lua reserve, extended from `[key, run]` to the full chain; the
`kill`/`velocity`/`concurrency`/`budget` checks all run inside the one EVAL.

## 5. Kill, per node
A kill can target **any node** (run, app, team, org). Any request whose chain
includes a killed node is denied (`403`, terminal). So:
- kill a **run** = today's behavior;
- kill an **app** = freeze that app;
- kill a **team**/**org** = an org-wide circuit breaker.

## 6. Deny semantics (so the reason is actionable)
- **Budget/velocity exhausted** → `429` (retryable), body names the **binding node
  + dimension + window** and a `Retry-After` (rolling → seconds; calendar →
  time-to-reset). e.g. `"team-A daily budget exhausted; resets 00:00 UTC"`.
- **Killed** → `403` (terminal, until un-killed / TTL).
- Distinguishing these is required: one says "wait", the other says "stop."

## 7. Inheritance & defaults
- No explicit budget on a node ⇒ that node imposes no constraint (ancestors still
  do).
- **Default templates** (optional, convenience): a parent may set a default that
  applies to children lacking their own (e.g. "apps default to $100/day"). A
  child's explicit value overrides. Templates are sugar over §4 — they only set
  the child node's caps; enforcement is unchanged. *(MVP-optional.)*

## 8. Over-allocation & provisioning rules
- `Σ(children caps) > parent cap` is **allowed** (umbrella model). The management
  API **warns** ("apps allocate $2,000 vs the $1,000 team cap") but does not block
  — it's a valid pattern.
- Validation: caps ≥ 0 (a negative cap is rejected, not silently applied — it
  would deny every request on that scope); valid windows/tz; a key maps to
  exactly one node; ids are a safe token (no `:`/NUL scope-key delimiters,
  bounded length) and globally unique **per type** (the id, not just the name,
  keys the node — stricter than "unique within a parent", which the resolver
  relies on to build unambiguous scope keys).

## 9. Edge cases pinned
- **No run id:** run-level caps don't apply; org/team/app still do (documented).
- **Run id is untrusted input:** validated to a safe token before use (it flows
  into the `run:<owner>\x00<id>` key); a run id can never reconstruct another
  node's scope key because the run is namespaced under the caller's own leaf.
  Rotating run ids each request buys a fresh per-run budget but not escape from
  ancestor caps.
- **Broken ancestry fails closed:** if a node's parent record is missing (a
  durable-store integrity fault), resolution returns an error rather than a
  chain with an ancestor's budget silently omitted — never under-enforce.
- **Run-level kill** (§5) is a concern of the enforcement/reserve layer (runs
  aren't provisioned nodes); node kills (org/team/app) live in this model.
- **Parent exhausted mid-run:** subsequent requests denied (`429`), the run is
  *not* auto-killed — throttled, not terminated (unless a kill was issued).
- **Estimate overshoot:** `actual > est` (chars/4 input error) can push a node
  slightly over; its next reserve denies. Same honest caveat as today; a
  tokenizer-accurate estimate tightens it.
- **Fail-open:** central-store outage ⇒ per-instance local enforcement (bounded),
  counted on `/metrics`. Budgets are best-effort under partition, never $0-open.
- **Concurrency across levels:** per-node `max_concurrent` via the ZSET semaphore,
  reserved in the same atomic op.

## 10. Data model (sketch, Postgres)
```
org(id, name, tz)
team(id, org_id, name)
app(id, team_id, name)
budget(node_type, node_id, dimension, window, limit)        -- a cap on any node
apikey(sha256, node_type, node_id, admin bool)              -- a key → one node
```
Reserve derives scope keys from the chain: `org:<id>`, `team:<id>`, `app:<id>`,
`run:<owner>\x00<id>`; the enforcement point fetches the effective cap set for the
chain (cached, TTL'd) and calls reserve/settle.

## Consequences
- The **enforcement** of nested budgets is the existing primitive at more scopes —
  cheap. The **management** of them (entities, CRUD, key→node mapping, policy
  distribution) is the real new build.
- One coherent rule ("admit iff it fits under every ancestor; settle into all")
  covers org-wide caps, team pools, per-app limits, and per-run backstops.

## Alternatives considered
- **Strict sub-allocation / partitioning with guarantees** (Σ children ≤ parent;
  each app guaranteed its slice; starvation-free) — rejected for MVP: needs
  reservation/guarantee accounting and admission fairness; the umbrella model
  gives cost control without it. Could return as an opt-in `mode: partitioned`
  per node later.

## MVP scope (the build that follows this spec)
1. **Entities + store:** `org/team/app/budget/apikey` in Postgres (reuse the ledger
   DB).
2. **Extend reserve** from `[key, run]` to `[org, team, app, run]`; source caps
   from the policy store instead of static YAML.
3. **Minimal management API:** create team/app, set a budget at any node, issue a
   key mapped to a node, org-wide + per-node kill.
4. **Effective-policy fetch** for enforcement points (cache + TTL); deny responses
   name the binding node.
5. Defer: default templates, RBAC/SSO, a management UI, partitioned mode, audit.
