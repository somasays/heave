# Phase 2R (Redis shared kill store) — security / LLM-apps review (adversarial)

**Scope:** the shared run-kill store — `internal/firewall` (layered local+shared
kill state), `internal/redisstore`, server kill endpoint + run-id handling,
config/main wiring.
**Initial verdict:** FAIL — two defects let the kill switch's core guarantee
("a kill … stops the run") be silently false.
**Post-fix:** the guarantee holds, with the fail-open / in-flight limits stated
rather than hidden.

## Must-fix

| # | Finding | Resolution |
|---|---------|-----------|
| MF-1 | `memKillStore.Kill` capped the map by evicting the **oldest** entry. The oldest is the longest-established *live* kill — so under a spray of 100k kills the 100,001st silently evicts a victim's still-live kill and **resurrects a killed runaway** (single-node, or Redis down/brownout/LRU-evicted). Presented in docs as benign OOM-prevention. Zero test coverage of the cap. | Fixed — **never evict a live kill**. `Kill` sweeps expired first; if still at the cap it is full of live kills, so it **refuses** the new kill with `ErrKillStoreFull` (surfaced as HTTP 503 by the endpoint) rather than dropping one. `/metrics` exposes `firewall_kill_rejections`. `TestKillStoreRefusesWhenFullNeverEvictsLiveKill` |
| MF-2 | `X-Heave-Run-Id` accepted an arbitrary header (only end-trimmed) and reserved `run:<owner>\x00<id>`, but the kill route `{run_id}` is a single path segment. A run id containing `/` (or other non-segment bytes) was **reservable but unkillable** — the advertised manual kill switch didn't apply. | Fixed — `validRunID` (`[A-Za-z0-9._-]`, 1–128) enforced **identically** on the reserve and kill paths; a malformed id is rejected on ingress (400) so every reservable run is addressable. Also excludes NUL/control/separator bytes (closes the `\x00`-collision nit). `TestRunIDCharsetIsValidatedOnBothPaths` |

## Qualified nits — addressed / documented

- **Fail-open reads = cross-replica bypass window.** With Redis down, a
  *remotely*-issued kill isn't seen on other replicas (reads fail open), so that
  runaway keeps spending there until Redis recovers. Now stated explicitly in
  Invariant #9 ("best-effort, Redis-availability-gated"); the issuing replica
  still enforces locally.
- **Auto-kill (loop-trip) swallowed the shared-write error.** Now counted in
  `firewall_shared_kill_errors` (surfaced on `/metrics`) and asserted by
  `TestLoopTripSharedErrorIsObservable`; the manual path already returned 503.
- **TTL resurrection.** Kill timestamp is now **refreshed on read**, so an
  actively-checked kill never ages out; only abandoned runs expire. Documented;
  `TestKilledRefreshesTTL`.
- **`\x00` delimiter collision.** Run key now length-prefixes the owner
  (`run:<len>:<owner>\x00<id>`), unambiguous regardless of owner contents.
- **Auth-off owner="" collapse.** Unchanged behavior (dev-only) but now a **loud
  warning** logs at startup when the firewall is enabled with auth disabled.

## Non-issue confirmed

- A locally-issued kill survives a Redis partition (layered store); a shared
  write failure never reports a false success (`TestKillErrorSurfacesButLocalStillHolds`).
- Cross-instance visibility works (`TestKillIsVisibleAcrossInstances`).

## Still deferred (tracked in `docs/TASKS.md`)

- Cancelling in-flight (streaming) requests on kill — today a kill stops only
  subsequent requests.
- Durable/"permanent" kill flag beyond TTL.
- Cross-replica velocity/concurrency (needs an atomic Redis reserve/settle).
