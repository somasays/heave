# gateway — agent & contributor guide

Self-hostable, OpenAI-compatible LLM gateway. The differentiator is
**cache-aware routing** (keep a conversation on its warm-cache model; re-route
only when the prefix cache TTL lapses). Everything else is table stakes.

## Stack
- Go 1.26, standard library HTTP. Anthropic via the official `anthropic-sdk-go`.
- Redis (cache-state, Phase 2), Postgres (spend ledger, Phase 3) — behind the
  compose `state` profile; the binary runs without them.

## Commands (source of truth: Makefile / scripts/check.sh)
- `make check` — the one gate, fail-cheap: gofmt → arch grep → build →
  golangci-lint → test -race. Requires `golangci-lint` (brew install).
- `make hooks` — install git hooks (run once after clone).
- `make run`   — build and run against config.yaml.

## Invariants (FULL text + enforcement matrix: docs/INVARIANTS.md)
Design (#1–#6): one OpenAI ingress format; vendors only via `internal/provider`;
routing via `internal/router` (cache-awareness is the wedge); secrets from env;
every request accounted in `internal/ledger`; config is declarative data.
Architecture (#A1–#A5): one-directional layered imports (no cycles), `cmd` is
the only composition root, no global mutable state, context propagates. Plus
error-provenance, concurrency, observability, security invariants. Enforced by
`depguard` (layering), `check_arch.sh` (vendor/secret boundary), `forbidigo`,
`gochecknoglobals`, `noctx`, `bodyclose`, `-race`. Go style: docs/STYLE.md.

## Commit policy (enforced by .githooks/commit-msg)
- Commit messages MUST NOT attribute authorship to an AI: no `Co-Authored-By:
  Claude/Anthropic`, no "Generated with/by Claude", no 🤖. Write plain,
  human-authored messages. (Referencing the Anthropic API / `claude-*` model
  ids in a message is fine — the rule is about authorship, not the vendor name.)
- Do not weaken a quality gate just because generation is fast.

## Definition of done for a phase
A phase is done only after BOTH adversarial reviews pass and findings are
addressed: (a) an LLM-apps expert review, (b) a Go expert review. Record each in
`docs/reviews/`. See docs/INVARIANTS.md → "Phase gate".

## Task tracker (spans sessions)
`docs/TASKS.md` is the durable, committed backlog. Update it as work moves.
The article this project validates: `docs/ARTICLE-VALIDATION.md`.
