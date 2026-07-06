# ADR 0004 — The enforcement integration contract (reserve/settle/release)

**Status:** proposed (2026-07)
**Context:** heave's inline proxy is the simplest deployment, but it's the weakest
when an org already runs a gateway (LiteLLM/Portkey) or is latency-sensitive:
every LLM call then hops through another proxy, which relays payloads/streams and
becomes a data-path SPOF. heave's *value*, though, is the reserve/settle
enforcement engine — the proxy is just one way to invoke it. So the integration
point should be the engine, exposed as a clean, host-neutral contract, not a
specific gateway's hook.

## Decision

**The integration point is heave's own enforcement contract**, not any host's
extension mechanism. Three enforcement-native verbs, versioned and stable:

```
reserve(scope, estimate) -> { admitted, reservation_id, reason, retry_after }
settle(reservation_id, actual_usage)          # reconcile to real spend
release(reservation_id)                        # failed / aborted call
```

- **`reserve`** runs the full pre-vendor check-and-hold (kill + velocity + per-run
  budget + concurrency + provider quota) atomically. On breach it returns
  `admitted=false` with the reason; the caller rejects the request (e.g. 429/403).
- **`settle`** reconciles the held estimate to actual token usage (reserve/settle).
- **`release`** frees a reservation whose call never billed the vendor.

This maps exactly onto `firewall.Enter` / `Ticket.Settle` / `Ticket.Release` +
the broker — the engine already implements it. The only new server-side state is
a TTL'd registry mapping `reservation_id -> held ticket/lease`.

### The primitive integrators touch: `guard`, not a callback
The contract is wrapped in a **`guard`** — a context manager / middleware /
decorator that does reserve → settle/release around a call, so integrating is
enforcement-shaped, never logging-shaped:

```python
with heave.guard(run_id=run, model=model, estimate=est) as g:
    resp = call_the_llm(...)     # YOUR data path — heave is not in it
    g.settle(resp.usage)         # (auto on context exit; release on exception)
```

### Runtime modes (same contract, different transport)
1. **HTTP PDP** — `POST /v1/policy/{reserve,settle,release}`. Any language/gateway.
   heave is out of the data path; only tiny control calls cross the wire.
2. **In-process library** — a Go data plane imports the engine directly; zero hop,
   shared state via the same Redis.
3. **Middleware** — the `guard` as an ASGI/Express/`http.Handler` wrapper for raw
   apps.
4. **Service-mesh** — heave implements Envoy **`ext_authz`** for the `reserve`/kill
   check (zero-code for any ext_authz gateway: Istio/Gloo/Emissary/APISIX);
   `settle` comes from the access-log stream (ALS) or a companion call.

### Adapters use each host's POLICY hook — never a logger
A gateway adapter is a thin shim (~tens of lines) over the `guard` client, bound
to the host's *enforcement* extension:
- **LiteLLM** → subclass `CustomGuardrail` (`async_pre_call_hook` → reserve/reject;
  `async_post_call_success_hook` / failure hook → settle/release). **Not**
  `CustomLogger` — that is a logging hook and the wrong abstraction.
- **Envoy/mesh** → `ext_authz`.

## Consequences
- heave is deployable **out of the data path** — no extra proxy hop, no payload/
  stream relay, and it can fail open without failing the request.
- Integration is a stable contract + a `guard` verb; gateway adapters are thin and
  swappable. LiteLLM is one caller of many, not the design center.
- New work: the `/v1/policy/*` endpoints + reservation registry, `guard` clients
  (Go in-process first, then Python/TS), the LiteLLM guardrail adapter + an
  integration test, and (follow-up) the ext_authz surface.
- Honest cost: the PDP/adapter modes require the data plane to make **two calls**
  (reserve pre, settle post) — a callback in a gateway, or the `guard` wrapper in
  an app. The run id must reach heave (app sets it in metadata/header). Settle
  correctness depends on the caller reporting actual usage; a missing settle
  falls back to the reserved estimate (fail-closed), never $0.

## Alternatives considered
- **Inline proxy only** — kept as mode (1) of ADR-adjacent deployment, but rejected
  as the *only* integration: forces a hop and a re-point of every app.
- **Build to LiteLLM's `CustomLogger`** — rejected: it's a logging hook (wrong
  semantics) and LiteLLM-specific (couples our contract to one gateway).
