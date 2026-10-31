#!/usr/bin/env bash
# Start one receiver, replay send-test.sh against it and check the resulting ledger.
# Usage: ./e2e.sh node|python|go
set -euo pipefail

lang="${1:?usage: ./e2e.sh node|python|go}"
root="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
export BSCG_WEBHOOK_SECRET=e2e-secret STATE_FILE="$work/state.json" PORT="${PORT:-18080}"
unset BSCG_BNB_TOKEN

case "$lang" in
  node) cmd=(node server.mjs) ;;
  python) cmd=(python3 -u server.py) ;;
  go) (cd "$root/go" && go build -o "$work/receiver" .) && cmd=("$work/receiver") ;;
  *) echo "unknown receiver: $lang" >&2; exit 2 ;;
esac

(cd "$root/$lang" && exec "${cmd[@]}") >"$work/server.log" 2>&1 &
pid=$!
cleanup() {
  status=$?
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  if [[ "$status" != 0 ]]; then
    echo "--- $lang receiver log ---"
    cat "$work/server.log"
  fi
  rm -rf "$work"
}
trap cleanup EXIT

for _ in $(seq 50); do
  curl -s -o /dev/null "http://127.0.0.1:$PORT/" && break
  sleep 0.2
done

"$root/send-test.sh" "http://127.0.0.1:$PORT/bnb-webhook"

python3 - "$STATE_FILE" <<'EOF'
import json, sys
state = json.load(open(sys.argv[1]))
balance = state["balances"].get("0x1111111111111111111111111111111111111111")
statuses = {id: event["status"] for id, event in state["events"].items()}
expected = {"evt_demo_1": "finalized", "evt_demo_2": "finalized", "evt_demo_3": "removed"}
if balance != "1500000000000000000" or statuses != expected:
    sys.exit(f"unexpected ledger: balance={balance} events={statuses}")
print("ledger ok: 1.5 BNB credited once, reorged deposit dropped")
EOF
