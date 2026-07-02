# Validating "Engineering for AI Agents"

This repo is a live validation of the 13 levers in
[Engineering for AI Agents](https://theprincipledengineer.substack.com/p/engineering-for-ai-agents).
The article's thesis — *generation is no longer the constraint; everything
downstream of it is* — is exactly why this project leads with enforced
invariants and a fail-cheap pipeline instead of relying on review discipline.

Status legend: ✅ implemented · 🟡 partial / scaffolded · ⬜ planned.

| # | Lever | Status | Where in this repo |
|---|-------|--------|--------------------|
| 1 | Self-describing repo context (`CLAUDE.md`, <60 lines) | ✅ | `CLAUDE.md` |
| 2 | Context window as a managed resource | 🟡 | Reviews run in isolated sub-agents; durable decisions in `docs/` + memory |
| 3 | Fail fast and local (one command, <60s, ordered) | ✅ | `scripts/check.sh`, `make check`, `.githooks/pre-commit`; live/networked tests quarantined out of the gate behind `//go:build live` (`make smoke`, separate `smoke` workflow) |
| 4 | Build caching / affected-only | 🟡 | Docker layer caching (`Dockerfile`); Go build cache in CI |
| 5 | Monorepo with real tooling | ⬜ | single module today; not needed yet |
| 6 | Merge queue with blast-radius tiering | ⬜ | Phase 1+ (branch protection first) |
| 7 | Decouple deploy from release | ⬜ | Phase 3+ |
| 8 | Signal-gated promotion | ⬜ | Phase 3+ |
| 9 | Machine-readable runbooks + structured logs | 🟡 | Structured JSON logs (`internal/ledger`, slog); runbooks TBD |
| 10 | Brokered credentials (read broad / write gated) | 🟡 | Secrets from env only (Invariant #4); brokering is Phase 1+ |
| 11 | Mine sessions into team knowledge | 🟡 | `docs/reviews/`, ADRs under `docs/adr/` |
| 12 | Enforce design in CI, not review | ✅ | `depguard` in `.golangci.yml` enforces the package layering (the Go analog of import-linter/ArchUnit); `scripts/check_arch.sh` enforces the vendor/secret boundary |
| 13 | Version specs/prompts as code; protect main | ✅ | `CLAUDE.md`, `docs/INVARIANTS.md`, config all in VC; hooks + CI |

## Honest claims (the wedge, measured not asserted)

The runtime spend firewall is validated by an end-to-end counterfactual
(`internal/server/e2e_firewall_test.go`, plus a `live`-tagged twin against real
Anthropic): the same agent traffic with the firewall OFF vs ON, asserting that
blocked requests never reach the vendor (`blocked == attempts − vendor_calls`).
Adversarially reviewed (by Fable 5, `docs/reviews/phase3f-firewall-e2e-fable.md`),
the claim is scoped precisely: heave bounds the *blast radius* of a runaway
pre-vendor — it does **not** halve steady-state bill (that was the cache lever,
demoted in ADR 0001). The reported metric is the loss bound, not a
spend-reduction percentage. The E2E deliberately includes the *negative* result —
loop detection is blind to a growing-context runaway — and shows the hard per-run
$ budget as the backstop that catches it. Owning the limits is the point: an
engineering artifact that only shows the flattering case isn't validation.

## The two meta-forces, in this repo

- **Shorten feedback latency:** the identical `scripts/check.sh` runs locally, in
  the pre-commit hook, and as the whole CI job — a failure surfaces at the
  earliest, cheapest point.
- **Make trust automatic:** the architecture boundary, the no-AI-attribution
  commit policy, and the format/build/test gates are mechanical, not
  discretionary. The one place we deliberately keep a *human/expert* gate is
  design correctness — hence the per-phase adversarial reviews (Invariant:
  Phase gate), matching lever #12's "CI enforces structure; it cannot enforce
  good taste."
