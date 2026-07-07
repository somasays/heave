# heave × LiteLLM

Enforce heave's real-time spend & quota firewall on traffic that flows through a
**LiteLLM proxy** — without making heave a second proxy hop. heave is consulted as
a **Policy Decision Point** (a fast `reserve` call carries a scope + a number,
never the payload); the LLM traffic keeps flowing LiteLLM → vendor directly.

## Why a guardrail, not a logger

LiteLLM's guardrail lifecycle fires around every call. This adapter binds those
firings to heave's enforcement-native verbs — it is a *policy* hook, not logging:

| LiteLLM hook | heave call | effect |
|---|---|---|
| `async_pre_call_hook` | `reserve(scope, estimate)` | on deny → raise → the call is blocked **before** the vendor (429 budget/velocity, 403 killed) |
| `async_post_call_success_hook` | `settle(reservation_id, actual)` | reconcile the held estimate to real usage |
| `async_post_call_failure_hook` | `release(reservation_id)` | free the hold — the call never billed |

The `reservation_id` is threaded through `data["metadata"]`, so no external
correlation store is needed. If LiteLLM crashes between reserve and settle, heave's
reservation **lease self-heals** the hold — a missing settle never leaks budget.

## Setup

1. **Run heave** with the control plane + decision API on:
   ```yaml
   control_plane:
     enabled: true
     guard_secret_env: HEAVE_GUARD_SECRET     # >= 32 bytes
   firewall:
     redis_url: redis://redis:6379/0          # required for the decision API
   auth:
     enabled: true
   clients:
     - { name: litellm-pep, key_sha256: <sha256 of the PEP admin key>, admin: true }
   ```

2. **Provision** an org hierarchy + budgets (see `example_config.yaml` for the
   `POST /v1/policy/*` calls), and issue a key per tenant/app.

3. **Install** the adapter next to your LiteLLM proxy and add the `guardrails:`
   block from `example_config.yaml`. Set `HEAVE_URL` and `HEAVE_ADMIN_KEY`.

That's it — every request through LiteLLM now hits heave's firewall pre-vendor.

## Scope mapping

The adapter resolves each call to a heave `(key_sha256, run_id)`:
- `key_sha256`: `metadata.heave_key_sha256` if set, else the SHA-256 of the
  caller's LiteLLM virtual key (provision heave keys with that hash).
- `run_id`: `metadata.heave_run_id` or the `X-Heave-Run-Id` header — enables the
  per-run budget, loop detection, and the per-run kill switch.

## Notes / limits

- **USD estimates.** For `$/min` and per-run `$` caps, the adapter estimates cost
  from an optional `prices` map (per model). Without it, `usd=0` is sent and only
  token/concurrency caps apply. Pricing the reserve heave-side (by model) is a
  planned enhancement so the PEP won't need a price map.
- **HA.** heave's decision API requires the shared store (`firewall.redis_url`);
  reserve/settle/release are idempotent across heave replicas.
- This is the **reference** adapter. The same three calls back any PEP: Envoy
  `ext_authz`, a sidecar, or a thin client library.
