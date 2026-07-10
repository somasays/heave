# Deploying heave

How heave fits into a real environment: where it sits, how apps adopt it, the
topologies, scaling/HA, configuration, and the security posture.

## 1. Where it sits — two modes

The enforcement engine is the product; there are two ways to place it.

- **Inline (this section).** heave is the egress point: agents call it, it enforces
  and forwards. Zero integration (a base-URL swap), strongest bypass story (lock
  egress). Best when heave can be the only route to the vendors.
- **Decision point / OOB (§3C).** If you already run a gateway (LiteLLM/Portkey/
  Envoy) and won't add a data-path hop, heave is consulted *out of band* via the
  `/v1/guard` reserve/settle/release decision API — the LLM traffic never flows
  through heave. Same engine, same budgets; see §3C + [ADR 0007](adr/0007-guard-decision-api.md).

Either way, budgets are provisioned in the **org control plane** (§9).

### Inline placement

Instead of each agent calling `api.anthropic.com` (or OpenAI, etc.) directly, it
calls heave; heave authenticates, enforces the spend/quota firewall **before**
dispatch, forwards to the real vendor, settles the actual spend, and returns the
response.

```
 agents / apps ──(OpenAI API + Bearer + X-Heave-Run-Id)──▶  heave  ──▶  vendors
                                                              │           (Anthropic,
                                             ┌────────────────┤            OpenAI-compatible:
                                    Redis (shared caps + kills)            OpenRouter, Azure,
                                    Postgres (durable spend ledger)        vLLM, …)
                                             └── both optional ──┘
```

**The enforcement guarantee is only as good as the network placement.** The
strongest setup makes heave the *only* path to the vendors:

- Put heave on an internal network / behind your ingress.
- Apply an **egress policy** so only heave's pods/hosts can reach the vendor API
  domains. Now an agent literally cannot bypass the firewall — misconfigured or
  compromised code can't call the vendor directly.
- Vendor API keys live **only** on heave (from the environment), so application
  teams never hold them.

## 2. Adopting it (the client change)

heave implements the OpenAI Chat Completions API, so adoption is a base-URL swap
plus two headers — no SDK change.

- **Base URL** → your heave endpoint (`https://heave.internal/v1`).
- **`Authorization: Bearer <team-key>`** → a per-team key you issue (heave stores
  only its SHA-256; see §5).
- **`X-Heave-Run-Id: <run-id>`** → a stable id for the agent run. **This header is
  what turns on the per-run controls** (kill switch, per-run `$` budget, per-run
  velocity). Untagged traffic still gets per-key caps, but per-run enforcement
  needs it — make your agent framework set it once per run.

```python
# Python (openai>=1.x)
from openai import OpenAI
client = OpenAI(base_url="https://heave.internal/v1", api_key=TEAM_KEY)
client.chat.completions.create(model="fast", messages=msgs,
                               extra_headers={"X-Heave-Run-Id": run_id})
```
```ts
// TypeScript
const client = new OpenAI({ baseURL: "https://heave.internal/v1", apiKey: TEAM_KEY });
await client.chat.completions.create(
  { model: "fast", messages },
  { headers: { "X-Heave-Run-Id": runId } },
);
```
```bash
curl https://heave.internal/v1/chat/completions \
  -H "Authorization: Bearer $TEAM_KEY" -H "X-Heave-Run-Id: $RUN_ID" \
  -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}'
```

Models are **aliases** you define in config (`fast`, `balanced`, `smart`, …) that
map to a provider + upstream model + price + policy — so client code references
stable names, and you re-map or add fallbacks without touching apps.

## 3. Topologies

### A. Central shared service — **recommended**
One horizontally-scaled deployment that *all* teams route through, with Redis and
Postgres.

- **Why:** the highest-value controls are inherently global. A provider's rate
  limit is one shared number; cross-team velocity, kills, and attribution only
  make sense from a shared vantage point. Sidecars can't arbitrate a shared quota.
- **Shape:** N stateless replicas behind an internal load balancer; Redis for
  shared caps + kills; Postgres for the durable ledger; per-team keys; auth **on**.

### B. Sidecar (per app)
heave next to a single application. Fine for isolating one noisy app, but you lose
cross-team quota brokering and fleet-wide attribution (each sidecar sees only its
own traffic). Use only when a single app is the whole story.

### C. Decision point (OOB) alongside an existing gateway — **no data-path hop**
When a mature gateway (LiteLLM/Portkey/Envoy) already owns routing/caching, heave
enforces **out of band**: the gateway asks heave for a decision; the LLM traffic
keeps flowing gateway → vendor directly. heave is a **Policy Decision Point**, not
a second proxy.

```
 agents ──▶ LiteLLM ──────────────────────────────▶ vendors   (data path — heave NOT in it)
               └── reserve/settle/release ──▶ heave /v1/guard   (a fast decision call: scope + a number)
```

- The gateway's policy hook calls `POST /v1/guard/reserve` before the vendor call
  (deny → block pre-vendor), `settle` on success, `release` on failure. The reserve
  carries a scope (`key_sha256` + `run_id`) and an estimate — **never the prompt or
  response.** It maps 1:1 onto the same engine the inline path uses.
- Ship the adapter into your gateway: the reference LiteLLM `CustomGuardrail` is in
  [`integrations/litellm/`](../integrations/litellm/) (~50 lines + a config block);
  Envoy `ext_authz` / a library are the same three calls.
- **Requires the shared store** (`firewall.redis_url`): the decision API's
  cross-replica idempotency + orphaned-hold reaping depend on it (ADR 0007). Set
  `control_plane.guard_secret_env` (a ≥32-byte HMAC key) to mount `/v1/guard`.

Alternatively, still want heave *in front* for its own reasons? Point heave's
provider `base_url` at the downstream gateway (both are OpenAI-compatible) — but
that's a data-path hop; the decision-point mode above is preferred. See
[`COMPARISON.md`](COMPARISON.md).

## 4. Deploy targets

**Docker** (a `Dockerfile` ships in the repo):
```bash
docker build -t heave .
docker run -p 8080:8080 -e ANTHROPIC_API_KEY -v $PWD/config.yaml:/config.yaml:ro heave
```

**Docker Compose** — the binary runs alone; Redis + Postgres are behind the
`state` profile:
```bash
docker compose up                     # gateway only (in-memory)
docker compose --profile state up     # + Redis + Postgres
```

**Kubernetes** (illustrative — heave is a stateless Deployment; wire Redis/
Postgres as managed services or their own workloads):
```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: heave }
spec:
  replicas: 3                          # stateless — scale freely
  selector: { matchLabels: { app: heave } }
  template:
    metadata: { labels: { app: heave } }
    spec:
      containers:
        - name: heave
          image: ghcr.io/somasays/heave:latest
          args: ["-config", "/etc/heave/config.yaml"]
          ports: [{ containerPort: 8080 }]
          env:                          # secrets from env, never config (Invariant #4)
            - name: ANTHROPIC_API_KEY
              valueFrom: { secretKeyRef: { name: heave-secrets, key: anthropic } }
            - name: DATABASE_URL         # referenced by ledger.database_url_env
              valueFrom: { secretKeyRef: { name: heave-secrets, key: database_url } }
          volumeMounts: [{ name: cfg, mountPath: /etc/heave }]
          readinessProbe: { httpGet: { path: /healthz, port: 8080 } }
          livenessProbe:  { httpGet: { path: /healthz, port: 8080 } }
      volumes:
        - name: cfg
          configMap: { name: heave-config }   # config.yaml (no secrets in it)
---
apiVersion: v1
kind: Service
metadata: { name: heave }
spec: { selector: { app: heave }, ports: [{ port: 80, targetPort: 8080 }] }
```
Set `firewall.redis_url` in the mounted config to your Redis service so caps are
cluster-wide across the 3 replicas.

## 5. Scaling & high availability

- **Stateless replicas.** heave holds no per-request state that must survive a
  restart. Scale horizontally behind any L7 load balancer; graceful shutdown
  drains in-flight requests on `SIGTERM`.
- **Shared state = Redis.** With `firewall.redis_url`, N replicas honor **one**
  velocity/concurrency/quota cap and a kill on any replica stops the run on all —
  via a single atomic reserve/settle (see [ADR 0002](adr/0002-cross-replica-velocity-concurrency.md)).
  A single-shard or proxy-clustered Redis is assumed.
- **Fail-open on a Redis outage.** If Redis is unreachable, heave **degrades to
  local per-instance enforcement** (still bounded — N× — never *unenforced*), and
  counts it on `/metrics` as `firewall_scope_degraded` / `broker_scope_degraded`.
  Alert on those. The per-client **monthly budget** stays authoritative regardless.
- **Durable ledger = Postgres** (optional). Records are batched asynchronously and
  **dropped-with-a-counter** under backpressure so accounting never blocks the
  request path; watch `/metrics ledger_dropped`. Apply your own
  retention/partitioning to the `spend` table.
- **Health:** `GET /healthz` for liveness/readiness. Tune `server.request_timeout`,
  `server.max_request_bytes`, and the read/write timeouts to your workload.

## 6. Configuration & secrets

Config is declarative data ([`config.example.yaml`](../config.example.yaml)).
**Secrets are never in it** (Invariant #4): providers name an env var for their
API key (`api_key_env`), and the durable-ledger DSN is named via
`ledger.database_url_env`. Mount `config.yaml` (a ConfigMap / read-only volume);
inject keys via your secret store.

Turn the firewall on and set the caps that matter for your risk tolerance:
```yaml
firewall:
  enabled: true
  redis_url: "redis://redis:6379/0"   # cluster-wide caps; omit for single node
  max_usd_per_min: 5.0                 # per key AND per run
  max_tokens_per_min: 200000
  max_concurrent: 20
  loop_threshold: 8
  max_usd_per_run: 25.0                # hard per-run backstop
```

## 7. Security & operations posture

- **Self-hosted:** the enforcement point *and* the spend/attribution data stay in
  your environment — nothing leaves your VPC.
- **Auth:** enable it (`auth.enabled: true`). Issue a per-team bearer key; heave
  stores only its SHA-256. Enforcement is only meaningful with auth on.
- **Observability endpoints are admin-gated:** `/v1/stats`, `/v1/spend`, and the
  `/dashboard` expose cross-tenant spend — they require an `admin: true` key when
  auth is on. `/metrics` is aggregate-only; `/healthz` is open. Put all of them on
  an internal network.
- **Egress lockdown** (see §1) makes heave the only route to the vendors.
- **CI-enforced architecture** and per-phase adversarial reviews back the
  correctness claims ([`INVARIANTS.md`](INVARIANTS.md), [`reviews/`](reviews/)).

## 8. Org control plane & console

Beyond per-key caps, heave models an **org hierarchy** — `org ▸ team ▸ app ▸ run` —
with a budget settable at *any* level (a team pool **and** per-app caps), per-node
kill switches, and key→node mappings. Budgets compose under one rule: a request is
admitted iff it fits under **every** ancestor; the tightest binds ([ADR 0006](adr/0006-hierarchical-budgets-resolution.md)).
It applies to both deployment modes (inline and OOB).

Enable it and mount the admin surfaces:
```yaml
control_plane:
  enabled: true
  guard_secret_env: HEAVE_GUARD_SECRET     # ≥32B HMAC key → mounts the /v1/guard decision API
  console:                                  # the admin console (optional)
    enabled: true
    session_secret_env: HEAVE_SESSION_SECRET
    base_url: https://heave.internal
    admin_domains: [acme.com]               # SSO identities authorized as admin
    google: { client_id: "...", client_secret_env: HEAVE_GOOGLE_SECRET }
    github: { client_id: "...", client_secret_env: HEAVE_GITHUB_SECRET }
    accounts:                               # local break-glass operators (PBKDF2 hashes)
      - { username: ops, password_hash: "pbkdf2$sha256$...", admin: true }
```

- **Management API** (`/v1/policy/*`, admin-gated): create org/team/app, set a
  budget at any node, kill/un-kill a node, issue a key → node.
- **Console** at `GET /console`: the same operations in a UI, with local login +
  Google/GitHub SSO. Admin is granted by the `admin_emails`/`admin_domains`
  allowlist (SSO) or the `admin` flag (local accounts). Cookies are Secure by
  default; the management API accepts the console session **or** an admin bearer.
- The store is in-memory today (provision via the API/console; re-apply after a
  restart); a durable Postgres-backed policy store is on the roadmap.

## 9. Rollout checklist
1. Deploy heave (central service) with `auth.enabled: true` and the firewall on.
2. Issue per-team keys; give ops an `admin: true` key for the dashboard.
3. Point one low-risk app's OpenAI `base_url` at heave; add the run-id header.
4. Set conservative caps; watch `/dashboard` + `/metrics`; tune.
5. Add Redis (cluster-wide caps) and Postgres (durable ledger) when you go
   multi-replica.
6. Apply the egress policy so nothing can bypass heave. Roll out team by team.
