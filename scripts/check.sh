#!/usr/bin/env bash
# The single verification command (article lever #3: fail fast and local).
# Ordered cheapest-first so the pipeline fails cheap — formatting and the pure
# grep architecture check run before the compiler, and the compiler before the
# tests. The same script runs locally, in the pre-commit hook, and in CI, so
# there is one source of truth for "is this change OK".
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

# 3. Vet — cheap static analysis (compiles, so it comes before tests).
step "go vet"
go vet ./...

# 4. Build — must compile.
step "go build"
go build ./...

# 5. Tests — hermetic and fast; last because they are the most expensive.
# -race is on: the single verification command must catch concurrency bugs.
step "go test -race"
go test -race ./...

echo
echo "all checks passed"
