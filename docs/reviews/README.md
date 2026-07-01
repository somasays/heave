# Adversarial phase reviews

Every phase is gated on two adversarial expert reviews before it can be marked
done (see `docs/INVARIANTS.md` → "Phase gate"):

- **LLM-apps expert** — LLM integration correctness: cache/prompt semantics,
  token accounting, streaming, provider quirks, failover, cost-control logic.
- **Go expert** — idiomatic Go, concurrency safety, error handling, context
  propagation, API design, resource lifecycle, test quality.

Reviewers are adversarial: the job is to break the design, not bless it.

## File naming
`<phase>-<discipline>.md`, e.g. `phase0-llm.md`, `phase0-go.md`.

## Each review records
1. Scope reviewed (commit/diff range).
2. Findings, ranked by severity, each with a concrete failure scenario.
3. Resolution per finding: fixed (link), waived (why), or deferred (task id).
4. Verdict: pass / pass-with-follow-ups / fail.
