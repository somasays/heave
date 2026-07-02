# Firewall E2E — strategic + technical review (Fable 5, adversarial)

**Scope:** the end-to-end validation of the wedge — `internal/server/e2e_firewall_test.go`
(hermetic counterfactual) + `internal/server/live_test.go::TestLiveFirewallBoundsRunawaySpend`
(real Anthropic) — judged against the project goal (Invariant #9).
**Verdict:** *Mechanism proven; original claim overstated; publishable once the
claim is narrowed to what the test proves.* Fable re-ran the hermetic suite and
reproduced the numbers exactly.

## What it ruled sound
- The fake is only at the `provider.Provider` boundary — which is *where "vendor"
  is defined_ (Invariant #2). `Enter` runs before dispatch; the assertion
  `blocked == attempts − fp.calls` structurally proves every denial is pre-vendor.
  Not narrated — asserted.
- The live twin closes the residual gap with real billing ($0.00008).
- The OFF/ON counterfactual is honestly constructed (only `firewall.New(enabled,…)`
  differs; budget/rate deliberately unlimited so the firewall is the only gate).

## What it flagged, and what we did
| # | Finding | Action |
|---|---------|--------|
| 1 | "75% reduction" is a test artifact (= (attempts−served)/attempts); conflated with the LinkedIn "halve our spend" story, which was the *cache* lever we pivoted away from. | Reframed the artifact to a **loss bound** (`≤ threshold × per-call cost`) + a cumulative-$ curve showing the flat line after the trip. Invariant #9 + ARTICLE-VALIDATION now state tail-bounding ≠ bill-reduction. |
| 2 | **Real control gap:** no `MaxUSDPerRun`. A changing-prompt runaway under the $/min cap had no automatic backstop; the "per-run kill switch" was manual/heuristic. | **Built `MaxUSDPerRun`** — a hard cumulative per-run $ budget on the reserve/settle machinery that auto-kills the run. Unit-tested + E2E. |
| 3 | Flagship scenario (identical prompt ×8) flatters the weakest control (exact-hash loop detection); not the canonical agentic runaway. | Added `TestE2E_GrowingContextRunawayNeedsBudgetNotLoopDetection`: shows loop detection **blind** (8/8 served, 0 kills) on a growing context, then the per-run budget catching it. The negative result is asserted, not hidden. |
| 4 | Concurrency cap / reserve-at-admit differentiator absent from the E2E (unit-tested only). | Added `TestE2E_ConcurrentBurstCannotOvershoot`: 20 concurrent requests through the full HTTP path, served spend bounded far below all-pass ($0.018 vs $0.12). |
| 5 | Per-instance velocity (N×), run-id prerequisite, tail-bound vs bill-reduction not stated for the article. | Stated plainly in Invariant #9 caveats and ARTICLE-VALIDATION. |

## Residual limits (owned, tracked)
- Per-run $ budget is enforced over a run's *active* lifetime (idle-reclaimed) on
  the reserved upper-bound estimate — not a cross-restart durable lifetime cap.
- Velocity/concurrency remain per-instance (needs atomic Redis reserve/settle).
- Kill does not cancel an in-flight stream.

**Standing behind it publicly as:** "a spend firewall that bounds agentic
blowups pre-vendor, with these disclosed limits" — including the negative on
growing-context loop detection. NOT as validation of a bill-halving claim.
