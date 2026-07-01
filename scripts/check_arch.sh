#!/usr/bin/env bash
# Architecture boundary check. Enforces the structural invariants in
# docs/INVARIANTS.md at commit and in CI (article lever #12: enforce design in
# CI, not review). Cheap (pure grep), so it runs early in the fail-fast chain.
set -euo pipefail

fail=0
note() { printf '  ARCH VIOLATION: %s\n' "$1" >&2; fail=1; }

# Invariant #2: only internal/provider may import a vendor SDK.
# Any Go file outside internal/provider importing the Anthropic SDK is a leak.
vendor_sdk='github.com/anthropics/anthropic-sdk-go'
while IFS= read -r f; do
  case "$f" in
    ./internal/provider/*) ;;
    *) note "$f imports the vendor SDK ($vendor_sdk); only internal/provider may." ;;
  esac
done < <(grep -rl "$vendor_sdk" --include='*.go' . 2>/dev/null || true)

# Invariant #2: only internal/provider may hardcode a vendor endpoint host.
while IFS= read -r hit; do
  f="${hit%%:*}"
  case "$f" in
    ./internal/provider/*|./cmd/heave/main.go) ;;  # main sets the openai default base
    *) note "$hit — vendor endpoint host referenced outside internal/provider." ;;
  esac
done < <(grep -rn -e 'api\.anthropic\.com' -e 'api\.openai\.com' --include='*.go' . 2>/dev/null || true)

# Invariant #4: no obvious hardcoded secrets in Go source.
while IFS= read -r hit; do
  note "$hit — looks like a hardcoded credential; use an env-sourced config key."
done < <(grep -rnE '(sk-[A-Za-z0-9]{20,}|sk-ant-[A-Za-z0-9_-]{20,})' --include='*.go' . 2>/dev/null || true)

if [ "$fail" -ne 0 ]; then
  echo "architecture check FAILED" >&2
  exit 1
fi
echo "architecture check ok"
