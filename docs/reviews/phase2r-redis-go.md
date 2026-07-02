# Phase 2R (Redis shared kill store) — Go / distributed-systems review (adversarial)

**Scope:** `internal/firewall` (layered kill state, `Enter`/`enterLocked` split,
`memKillStore`), `internal/redisstore`, config/main wiring.
**Initial verdict:** PASS-WITH-NITS — the lock hierarchy is provably acyclic and
the `Enter` refactor is correct, but the kill-map cap was a silent correctness
cliff with no coverage.
**Post-fix:** cliff removed; coverage added.

## Verified correct (no change needed)

- **Lock order is acyclic.** The only nesting is `firewall.gcLocked` (holds
  `f.mu`) → `localKills.gc` (takes `localKills.mu`); order is always
  `f.mu → localKills.mu`. Every other `localKills` access (`isKilled`, `doKill`)
  is deliberately outside `f.mu`, and no `memKillStore` method calls back into
  `Firewall`. No cycle.
- **`Enter`/`enterLocked` split** — `defer f.mu.Unlock()` restores panic-safety;
  the tripped path returns before reserving, so no reservation/slot leaks; kill
  happens after unlock (shared store I/O never under the lock).

## Must-fix

| # | Finding | Resolution |
|---|---------|-----------|
| M1 | `evictOldestLocked` evicts a live kill under cap pressure (silent resurrection) and picks the worst victim (longest-established). | Fixed — eviction removed entirely; at the cap `Kill` **refuses** a new kill (`ErrKillStoreFull`) after sweeping expired, and `Killed` refreshes on read so an active kill is the *last* thing to expire. See security MF-1. |
| M2 | No test filled the map (cap uncovered); loop-trip shared-error path uncovered. | Fixed — `TestKillStoreRefusesWhenFullNeverEvictsLiveKill`, `TestKilledRefreshesTTL`, `TestLoopTripSharedErrorIsObservable`. |

## Nits — addressed

- **N1 (hot-path shared read).** Noted: `isKilled` consults the shared store on
  every admit (local miss common), a synchronous Redis RTT with a 500ms budget.
  Left as-is for correctness (a negative cache would open a bypass window);
  called out in Invariant #9 as a Redis-availability latency consideration.
- **N2 (swallowed auto-kill error).** Now counted in `firewall_shared_kill_errors`
  and asserted.
- **N3 (error provenance / ttl≤0).** `redisstore.Kill/Killed` errors are now
  wrapped (`redis set kill %q: %w`); `NewClient` guards `ttl<=0` with a fallback
  so a misconfig can't create never-expiring keys.
- **N4 (trip→kill micro-window).** Documented as harmless (loop detection is a
  heuristic; the concurrent request re-evaluates).
- **N5 (duplicated TTL default).** Single-sourced: `firewall.DefaultKillTTL`
  resolved once in `main` and fed to both the firewall and `redisstore`.

## Sign-off

Both reviews' must-fixes are folded in; `make check` (gofmt, arch, build,
golangci-lint 0 issues, `-race`) passes. Phase gate met.
