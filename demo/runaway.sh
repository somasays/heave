#!/usr/bin/env bash
# heave demo: a runaway agent, stopped PRE-vendor.
#
#   export ANTHROPIC_API_KEY=sk-ant-...
#   ./demo/runaway.sh
#
# Starts heave with demo/config.yaml (firewall ON), then simulates a runaway agent
# hammering the same prompt on one run. The firewall kills the run after 3 calls —
# every call after that is refused with HTTP 403 BEFORE it ever reaches the vendor.
set -euo pipefail

KEY="heave-demo-key"           # demo bearer (matches demo/config.yaml)
RUN="agent-run-7"
BASE="http://localhost:8080"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "set ANTHROPIC_API_KEY first (a Claude key; the demo uses cheap haiku calls)"; exit 1
fi

echo "▸ building heave…"
( cd "$DIR" && go build -o /tmp/heave-demo ./cmd/heave )

echo "▸ starting heave (firewall ON: loop_threshold=3, max_usd_per_run=\$0.01)…"
/tmp/heave-demo -config "$DIR/demo/config.yaml" >/tmp/heave-demo.log 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
sleep 1

send() {  # $1 = attempt number
  local code
  code=$(curl -s -o /tmp/heave-resp.json -w '%{http_code}' \
    -H "Authorization: Bearer $KEY" \
    -H "X-Heave-Run-Id: $RUN" \
    -d '{"model":"fast","max_tokens":32,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}' \
    "$BASE/v1/chat/completions")
  if [[ "$code" == "200" ]]; then
    echo "  call $1 → 200 OK        (reached the vendor, billed)"
  elif [[ "$code" == "403" ]]; then
    echo "  call $1 → 403 RUN KILLED (refused PRE-vendor — \$0)"
  else
    echo "  call $1 → $code"
  fi
}

echo
echo "▸ a runaway agent starts looping the same request on run '$RUN':"
for i in $(seq 1 8); do send "$i"; done

echo
echo "▸ /v1/stats — spend is bounded; the run was auto-killed:"
curl -s -H "Authorization: Bearer $KEY" "$BASE/v1/stats" > /tmp/heave-stats.json
python3 - <<'PY'
import json
d = json.load(open("/tmp/heave-stats.json"))
t = d["total"]
print("  billed: %d vendor calls, $%.5f — then the firewall stopped it" % (t["requests"], t["cost_usd"]))
print("  live kills recorded: %d" % d["firewall"]["LocalKills"])
PY
echo
echo "✓ Without heave, all 8 calls bill. With heave, the runaway is capped at the"
echo "  threshold and every later call costs \$0 — hard, real-time, PRE-vendor."
