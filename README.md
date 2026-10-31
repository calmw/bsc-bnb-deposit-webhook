# BSC BNB Deposit Webhook Receiver

[![CI](https://github.com/calmw/bsc-bnb-deposit-webhook/actions/workflows/ci.yml/badge.svg)](https://github.com/calmw/bsc-bnb-deposit-webhook/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

English | [简体中文](README.zh-CN.md)

Ready-to-run webhook receivers for **native BNB deposits on BNB Smart Chain**, in Node.js, Python and Go. Each one is a single file with **zero dependencies**.

A listener that only watches `tx.to` misses BNB that arrives through contract calls: multisig wallets (Safe), batch payouts, DEX routers swapping into BNB. These "internal transfers" emit no logs, so `eth_getLogs` misses them as well. [BSC Query](https://bscquery.com/bnb?utm_source=github&utm_medium=referral&utm_campaign=examples) traces every block and pushes each BNB balance change of your addresses, internal transfers and gas fees included, with the address balance at the end of the block.

These receivers take care of the parts that are easy to get wrong:

- **Signature check**: HMAC-SHA256 over the raw body; requests more than 5 minutes off are rejected to stop replays.
- **Credit exactly once**: dedupe by event `id`, credit only on `finalized`. Retries, duplicates and backfill overlaps never double-credit.
- **Reorgs**: `pending` shows "confirming", `finalized` credits, `removed` drops the pending entry.
- **All-or-nothing**: return 200 only after the whole request is stored; on any error, discard it and return 500 so the platform retries the full batch.
- **Both payload shapes**: one block per request, or `{"batches": [...]}` when multi-block delivery is on.
- **Backfill**: on startup, pull the last 24 hours over REST, then the last 2 hours every hour, so deliveries missed while you were down are filled in.

## Quick start

### 1. Get a free plan

Connect a BSC wallet at [bscquery.com](https://bscquery.com/bnb?utm_source=github&utm_medium=referral&utm_campaign=examples) and enable the BNB monitor free plan (10 addresses, 10,000 changes per 30 days, `finalized` only). On the BNB plan page, copy:

- the **token** (starts with `bscg_bnb_`), used for REST calls;
- the **signing secret**, in the "Webhook" section.

### 2. Run a receiver

Pick a language:

```bash
git clone https://github.com/calmw/bsc-bnb-deposit-webhook.git
cd bsc-bnb-deposit-webhook

export BSCG_WEBHOOK_SECRET='your signing secret'
export BSCG_BNB_TOKEN='bscg_bnb_...'   # optional, enables backfill

cd node   && node server.mjs        # Node.js 18+
cd python && python3 server.py      # Python 3.9+
cd go     && go run .               # Go 1.22+
```

It listens on `http://0.0.0.0:8080/bnb-webhook`.

| Variable | Meaning | Default |
|---|---|---|
| `BSCG_WEBHOOK_SECRET` | Signing secret, required | — |
| `BSCG_BNB_TOKEN` | BNB token, used for backfill | backfill off |
| `BSCG_API` | API base URL | `https://api.bscquery.com` |
| `PORT` | Listen port | `8080` |
| `STATE_FILE` | State file | `state.json` |
| `BACKFILL_HOURS` | Hours to backfill on startup | `24` |

### 3. Test locally, no real transfer

In another terminal:

```bash
BSCG_WEBHOOK_SECRET='your signing secret' ./send-test.sh
```

The script signs and sends a test push, a deposit going `pending` → `finalized`, a duplicate, a multi-block request, a deposit that gets reorged away, and two forged requests:

```text
test-push -> HTTP 200
pending -> HTTP 200
finalized -> HTTP 200
duplicate -> HTTP 200
batches -> HTTP 200
reorg-pending -> HTTP 200
reorg-removed -> HTTP 200
bad-signature -> HTTP 401
stale-timestamp -> HTTP 401
```

The receiver credits 0.5 BNB once, then 1 BNB, and never the reorged 2 BNB: `state.json` ends at 1.5 BNB.

### 4. Receive real pushes

The callback URL must be public HTTPS. For local debugging, Cloudflare Tunnel works:

```bash
cloudflared tunnel --url http://localhost:8080
# use https://xxxx.trycloudflare.com/bnb-webhook as the callback URL
```

Save the callback URL on the BNB plan page and click "Send test push"; the receiver logs `test push received`.

Add the addresses to watch through the API:

```bash
curl -X POST https://api.bscquery.com/v1/bnb/addresses \
  -H "Authorization: Bearer $BSCG_BNB_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"addresses":["0xYourAddress"]}'
```

Send 0.001 BNB to it and the receiver logs `credited ...` a few seconds later. Send BNB through a multisig or a DEX swap and you will see `(internal)`.

## How a change is delivered

```text
block with a change to your address
  → pending      show "confirming", do not credit
  → finalized    credit once (BSC fast finality: usually seconds)
  → or removed   the block was reorged away; drop the pending entry
```

A change may arrive directly as `finalized`. Statuses only move forward, and the `id` never changes. A transaction re-included after a reorg gets a new `id`, and only one of them ever becomes `finalized`.

One request body (single-block mode):

```json
{
  "blockNumber": 124380000,
  "blockHash": "0x9f2c...",
  "timestamp": 1791100000,
  "status": "finalized",
  "events": [
    {
      "id": "evt_8f3c2a91d0b44e6f",
      "status": "finalized",
      "address": "0xyour_deposit_address",
      "direction": "in",
      "amount": "0.5",
      "amountWei": "500000000000000000",
      "counterparty": "0xsender",
      "kind": "internal",
      "txHash": "0x...",
      "blockNumber": 124380000,
      "blockHash": "0x9f2c...",
      "timestamp": 1791100000,
      "balance": "3.5",
      "balanceWei": "3500000000000000000"
    }
  ]
}
```

`kind` is `external` (the transaction's own value), `internal` (a transfer inside contract execution), `fee` (gas, always `out`) or `test`. `balance` is the address balance at the end of that block.

Signature headers:

```text
X-BSCG-Timestamp: 1791100005
X-BSCG-Signature: sha256=<hex of HMAC-SHA256(secret, timestamp + "." + raw body)>
```

Any 2xx means the whole request was stored. Anything else is retried after 1, 5 and 30 minutes.

## Before production

The receivers keep state in a JSON file so they run anywhere. For real use:

- **Use a database.** Handle each request in one transaction and return 200 after commit. Make the event `id` the primary key of the events table and a unique key of the ledger table.
- **Choose your crediting rule.** The demo adds `in` and subtracts `out` (fees included), which tracks the on-chain balance. A deposit product usually credits only `direction = in` to the user and keeps the rest for reconciliation.
- **Reconcile with `balanceWei`.** For one address, the difference between the `balanceWei` of two blocks equals the sum of the `finalized` changes between them. If it does not, run a backfill.
- **Be fast.** BSC makes a block about every 0.75 s; a handler slower than that builds a backlog. Store first and process asynchronously, or enable multi-block delivery.
- **Use `amountWei`.** It is an integer string; never do money math with floats.
- **Mind the retention.** Changes stay queryable for 1 day after confirmation (at least the latest 5,000 per subscription). After a longer outage, correct balances from `balanceWei`.

## Tests

```bash
cd node && npm test
cd python && python3 -m unittest discover -v
cd go && go test ./...
./e2e.sh node   # or python / go: starts the receiver, runs send-test.sh, checks the ledger
```

## Links

- [BNB webhook and REST reference](https://bscquery.com/docs/bnb?utm_source=github&utm_medium=referral&utm_campaign=examples)
- [BNB balance monitor](https://bscquery.com/bnb?utm_source=github&utm_medium=referral&utm_campaign=examples)

## License

[MIT](LICENSE)
