#!/usr/bin/env bash
# Replay signed webhook requests against a local receiver, no real transfer needed.
# Usage: BSCG_WEBHOOK_SECRET=... ./send-test.sh [http://127.0.0.1:8080/bnb-webhook]
# Expected ledger afterwards: 1.5 BNB (0.5 + 1) for 0x1111...1111.
set -euo pipefail

url="${1:-http://127.0.0.1:8080/bnb-webhook}"
secret="${BSCG_WEBHOOK_SECRET:?set BSCG_WEBHOOK_SECRET}"
address="0x1111111111111111111111111111111111111111"
now="$(date +%s)"
failed=0

event() { # id status kind amountWei amount
  printf '{"id":"%s","status":"%s","address":"%s","direction":"in","amount":"%s","amountWei":"%s","counterparty":"0x2222222222222222222222222222222222222222","kind":"%s","txHash":"0x%064d","blockNumber":100,"blockHash":"0x%064d","timestamp":%s,"balance":"1.5","balanceWei":"1500000000000000000"}' \
    "$1" "$2" "$address" "$5" "$4" "$3" 1 1 "$now"
}

block() { # status events
  printf '{"blockNumber":100,"blockHash":"0x%064d","timestamp":%s,"status":"%s","events":[%s]}' 1 "$now" "$1" "$2"
}

sign() { # timestamp body
  printf '%s.%s' "$1" "$2" | openssl dgst -sha256 -hmac "$secret" -hex | sed 's/^.* //'
}

post() { # name expected-status body timestamp signature
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$url" -H 'Content-Type: application/json' \
    -H "X-BSCG-Timestamp: $4" -H "X-BSCG-Signature: $5" -H "X-BSCG-Delivery-Id: test-$1" --data-raw "$3")"
  if [[ "$code" == "$2" ]]; then
    echo "$1 -> HTTP $code"
  else
    echo "$1 -> HTTP $code (expected $2)"
    failed=1
  fi
}

send() { # name expected-status body
  local ts
  ts="$(date +%s)"
  post "$1" "$2" "$3" "$ts" "sha256=$(sign "$ts" "$3")"
}

send test-push 200 "$(block finalized "$(event evt_demo_test finalized test 0 0)")"
send pending 200 "$(block pending "$(event evt_demo_1 pending internal 500000000000000000 0.5)")"
send finalized 200 "$(block finalized "$(event evt_demo_1 finalized internal 500000000000000000 0.5)")"
send duplicate 200 "$(block finalized "$(event evt_demo_1 finalized internal 500000000000000000 0.5)")"
send batches 200 "{\"batches\":[$(block finalized "$(event evt_demo_2 finalized external 1000000000000000000 1)")]}"
send reorg-pending 200 "$(block pending "$(event evt_demo_3 pending external 2000000000000000000 2)")"
send reorg-removed 200 "$(block removed "$(event evt_demo_3 removed external 2000000000000000000 2)")"

body="$(block finalized "$(event evt_demo_4 finalized external 9000000000000000000 9)")"
post bad-signature 401 "$body" "$now" 'sha256=bad'
stale="$((now - 600))"
post stale-timestamp 401 "$body" "$stale" "sha256=$(sign "$stale" "$body")"

exit "$failed"
