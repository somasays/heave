#!/usr/bin/env bash
# The single verification command (article lever #3: fail fast and local).
# Ordered cheapest-first so the pipeline fails cheap — formatting and the pure
# grep architecture check run before the compiler, the compiler before the
# linter, and the linter before the race-enabled tests. The same script runs
# locally, in the pre-commit hook, and in CI, so there is one source of truth
# for "is this change OK".
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n==> %s\n' "$1"; }

# 1. Formatting — near-instant, catches the most common noise.
step "gofmt (formatting)"
unformatted="$(gofmt -l . 2>/dev/null || true)"
if [ -n "$unformatted" ]; then
  echo "these files are not gofmt-clean:" >&2
  echo "$unformatted" >&2
  echo "run: gofmt -w ." >&2
  exit 1
fi

# 2. Architecture boundaries — pure grep, no compile needed.
step "architecture boundaries"
bash scripts/check_arch.sh

# 3. Build — must compile before deeper analysis.
step "go build"
go build ./...

# 4. Lint — layering (depguard) + style enforcement (docs/INVARIANTS.md,
#    docs/STYLE.md). Reuses the build cache from step 3.
step "golangci-lint"
if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "golangci-lint is required. Install: brew install golangci-lint" >&2
  echo "  (or see https://golangci-lint.run/welcome/install/)" >&2
  exit 1
fi
golangci-lint run ./...

# 5. Tests — hermetic and fast; last because they are the most expensive.
#    -race is on: the single verification command must catch concurrency bugs.
step "go test -race"
go test -race ./...

echo
echo "all checks passed"
