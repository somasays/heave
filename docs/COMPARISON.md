# How heave differs from LiteLLM, Portkey, and Cloudflare AI Gateway

Short version: those are excellent, mature **LLM proxies with budgets and
observability**. heave is a **runtime spend & quota firewall** — a narrower tool
that does one thing they don't: **hard, real-time enforcement in the data path,
before the vendor is billed, scoped to an agent run.**

If you want routing, caching, prompt management, a big provider matrix, and a
polished UI today, use one of them. If your problem is *an autonomous agent that
can burn five figures before anyone notices*, that's the gap heave is built for.

## The one axis that matters here

|  | heave | LiteLLM | Portkey | Cloudflare AI Gateway |
| --- | --- | --- | --- | --- |
| **Pre-vendor enforcement** (stop the spend *before* the vendor call) | **Core** | Budgets/keys, largely post-hoc | Budgets/guardrails | Rate limits / caching |
| **Per-agent-run scope** (`$/min`, kill switch, `$/run`) | **Yes** | No native run scope | No native run scope | No |
| **Kill switch for a live runaway** | **Yes** (`/v1/runs/{id}/kill` + auto-trip) | No | No | No |
| **Reserve-at-admit** (concurrent requests can't overshoot a cap) | **Yes** | Check-then-spend | — | — |
| **Provider-quota brokering** (reserve a shared RPM/TPM before dispatch) | **Yes** | Load-balance + retry after 429 | Load-balance | Rate limit |
| **Cross-replica caps** (N replicas honor *one* cap, not N×) | **Yes** (atomic Redis) | Depends / DB counters | Managed | Managed/edge |
| OpenAI-compatible ingress | Yes | Yes | Yes | Yes (proxy) |
| Self-hostable, single binary | **Yes** | Yes (Python) | Self-host + SaaS | SaaS/edge |
| Provider breadth | Anthropic + OpenAI-compatible | Very broad | Broad | Broad |
| Caching / routing / prompt mgmt / UI maturity | Minimal | **Mature** | **Mature** | **Mature** |
| Durable spend ledger + attribution by run | Postgres, built-in | Yes (DB) | Yes (SaaS) | Analytics |

## Why the difference is structural, not just missing features

A proxy-with-budgets sits **beside** your spend: it authenticates, forwards, and
*records* what happened, then reconciles budgets on a slow cadence. That's the
right design for cost visibility and routing. But it means:

- A **monthly/daily budget** is the wrong time constant for a failure measured in
  minutes. By the time it trips, the money is gone.
- **"Fail over after a 429"** is reactive — it needs the vendor to reject you
  first, and it doesn't arbitrate a quota shared across teams.
- Under concurrency, **check-then-spend** lets many in-flight requests each pass
  the check and collectively blow the cap (a TOCTOU).

heave sits **in front of** the spend and treats enforcement as the primary job:
generalizing one **reserve/settle** primitive from a monthly budget down to
`$/min`, `tokens/min`, and per-run `$` caps — *reserved at admit* so concurrency
can't overshoot, killable mid-run, and brokered against a known provider quota.
That posture is hard to bolt onto a proxy whose model is record-then-reconcile.

## Where heave is deliberately behind

Owning the limits is the point of this project, so:

- **Provider breadth, caching, routing policies, prompt management, and UI** are
  minimal — the incumbents are years ahead here.
- The **input token estimate is a heuristic** (chars/4), so caps can be overshot
  by roughly one call's estimate error; pair per-run budgets with `max_concurrent`
  for a tighter bound. (Output is bounded by `max_tokens`.)
- **Loop detection is exact-hash** — a per-turn nonce or a growing context defeats
  it; the per-run `$` budget is the backstop for exactly that case.
- Enforcement is only meaningful with **auth enabled**, and cross-replica
  velocity/concurrency require **Redis** (per-instance otherwise).

## When to pick which

- **Pick LiteLLM / Portkey / Cloudflare** for a general LLM gateway: many
  providers, caching, routing, dashboards, a mature ecosystem.
- **Pick heave** when you run autonomous agents at scale and need a *guarantee* —
  "no single run exceeds $X; a runaway is killed in seconds, pre-vendor; one team
  can't starve the shared vendor quota" — enforced in the data path, self-hosted so
  the enforcement point and the spend data stay under your control.
- **Or run both:** heave in front as the enforcement layer, a mature proxy behind
  it for routing/caching. They're complementary, not either/or.
