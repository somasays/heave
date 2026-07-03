# Contributing to heave

Thanks for your interest. heave is a spend & quota **firewall** — correctness and
honesty matter more than speed, so the bar is deliberately high and mechanical.

## Before you start
- Read [`docs/INVARIANTS.md`](docs/INVARIANTS.md) — the architectural rules the
  codebase upholds (they're enforced in CI, not just review).
- Skim [`docs/STYLE.md`](docs/STYLE.md) and a couple of [`docs/reviews/`](docs/reviews/)
  docs to see the review bar.
- For anything non-trivial, **open an issue first** so we agree on the approach.

## The one gate
```bash
make hooks     # once, after cloning — installs the git hooks
make check     # gofmt → arch boundaries → build → golangci-lint → test -race
```
`make check` must pass. It's fail-cheap (cheapest checks first) and is the exact
job CI runs. Don't weaken a quality gate to make a change land.

## Definition of done
- `make check` green (lint 0 issues, `-race`).
- New behavior has tests. Concurrency changes are exercised under `-race`.
- Public API / behavior changes update the relevant docs and, if it's a design
  decision, add an ADR under `docs/adr/`.
- Substantial firewall/enforcement changes should be reviewable against the
  security + Go bars in `docs/reviews/` — call out the limits you're aware of; we
  value honest "here's what this doesn't cover" over silent gaps.

## Commit policy (enforced by a hook)
- Commit messages MUST NOT attribute authorship to an AI (no `Co-Authored-By:`
  an AI, no "Generated with/by …", no 🤖). Write plain, human-authored messages.
  (Naming the vendor/API — e.g. an Anthropic model id — is fine.)
- Keep messages descriptive: what changed and why.

## Good first issues
Look for the `good first issue` label. Docs fixes, additional provider adapters,
test coverage, and tokenizer-accurate estimation are friendly entry points (see
[`docs/TASKS.md`](docs/TASKS.md) for the tracked backlog).

## Reporting security issues
See [`SECURITY.md`](SECURITY.md) — please don't open a public issue for a
vulnerability.
