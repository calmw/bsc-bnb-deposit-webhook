"""BSC Query BNB balance-change webhook receiver. Python 3.9+, standard library only."""

import hashlib
import hmac
import json
import os
import sys
import threading
import time
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_SKEW_SECONDS = 300
MAX_BODY_BYTES = 32 << 20


def verify(secret, timestamp, signature, raw_body, now=None):
    """The signature covers the raw request body: parsing and re-serializing the JSON changes the bytes."""
    try:
        ts = int(timestamp)
    except (TypeError, ValueError):
        return False
    if abs((time.time() if now is None else now) - ts) > MAX_SKEW_SECONDS:
        return False
    expected = "sha256=" + hmac.new(secret.encode(), f"{timestamp}.".encode() + raw_body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(signature or "", expected)


def events(payload):
    """With "multi-block delivery" enabled the body is {"batches": [...]}; accept both shapes."""
    blocks = payload["batches"] if isinstance(payload.get("batches"), list) else [payload]
    return [event for block in blocks for event in block["events"]]


class Store:
    """A JSON file keeps the demo restartable. In production use a database and handle each
    request in one transaction."""

    def __init__(self, path, log=print):
        self.path = path
        self.log = log
        self.lock = threading.Lock()
        self.reload()

    def reload(self):
        if os.path.exists(self.path):
            with open(self.path, encoding="utf-8") as f:
                self.state = json.load(f)
        else:
            self.state = {"events": {}, "balances": {}}

    def apply_all(self, events):
        """All or nothing: if any event fails, drop the half-applied state so the platform's retry
        replays the whole request against the last saved state."""
        with self.lock:
            try:
                for event in events:
                    self.apply(event)
                self.save()
            except Exception:
                self.reload()
                raise

    def apply(self, event):
        if event["kind"] == "test":
            self.log(f"test push received {event['id']}")
            return
        known = self.state["events"].get(event["id"])
        # finalized and removed are terminal. Skipping them makes retries, duplicates and backfill
        # overlaps credit each event exactly once.
        if known and known["status"] != "pending":
            return
        self.state["events"][event["id"]] = {
            k: event[k] for k in ("status", "address", "direction", "kind", "amountWei", "txHash", "blockNumber")
        }
        if event["status"] == "finalized":
            self.credit(event)
        elif event["status"] == "pending":
            self.log(f"confirming {event['address']} {event['direction']} {event['amount']} BNB "
                     f"({event['kind']}) tx={event['txHash']}")
        elif event["status"] == "removed":
            self.log(f"reorged, dropping {event['id']} tx={event['txHash']}")

    def credit(self, event):
        delta = int(event["amountWei"]) * (1 if event["direction"] == "in" else -1)
        balance = int(self.state["balances"].get(event["address"], "0")) + delta
        self.state["balances"][event["address"]] = str(balance)
        self.log(f"credited {event['address']} {event['direction']} {event['amount']} BNB ({event['kind']}) "
                 f"on-chain balance {event['balance']} BNB tx={event['txHash']}")

    def save(self):
        tmp = self.path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(self.state, f, indent=2)
        os.replace(tmp, self.path)


def backfill(store, api, token, hours):
    """Pull every change whose status moved in the last `hours` hours and apply the ones that were
    never delivered. Overlapping an earlier window is harmless because apply dedupes by id."""
    since = int(time.time()) - hours * 3600
    cursor, total = "", 0
    while True:
        query = {"since": since, "limit": 500}
        if cursor:
            query["cursor"] = cursor
        request = urllib.request.Request(
            f"{api}/v1/bnb/events?{urllib.parse.urlencode(query)}",
            # Cloudflare blocks the default Python-urllib User-Agent (403, error code 1010).
            headers={"Authorization": f"Bearer {token}", "User-Agent": "bsc-bnb-deposit-webhook/1"},
        )
        with urllib.request.urlopen(request, timeout=30) as response:
            page = json.load(response)
        store.apply_all(page["data"])
        total += len(page["data"])
        cursor = page["nextCursor"]
        if not cursor:
            return total


def backfill_loop(store, api, token, hours):
    while True:
        try:
            print(f"backfill done: {backfill(store, api, token, hours)} changes in the last {hours}h")
        except Exception as err:
            print("backfill failed:", err, file=sys.stderr)
        time.sleep(3600)
        hours = 2


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/bnb-webhook":
            self.reply(404)
            return
        length = int(self.headers.get("Content-Length") or 0)
        if length > MAX_BODY_BYTES:
            self.reply(413)
            return
        raw = self.rfile.read(length)
        if not verify(self.server.secret, self.headers.get("X-BSCG-Timestamp"),
                      self.headers.get("X-BSCG-Signature"), raw):
            self.reply(401)
            return
        try:
            payload = json.loads(raw)
        except ValueError:
            self.reply(400)
            return
        try:
            self.server.store.apply_all(events(payload))
        except Exception as err:
            print("processing failed, waiting for the platform to retry",
                  self.headers.get("X-BSCG-Delivery-Id"), repr(err), file=sys.stderr)
            self.reply(500)
            return
        self.reply(200)

    def reply(self, status):
        self.send_response(status)
        self.send_header("Content-Length", "0")
        self.end_headers()


def serve(store, secret, port):
    server = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    server.store = store
    server.secret = secret
    return server


def main():
    secret = os.environ.get("BSCG_WEBHOOK_SECRET", "")
    token = os.environ.get("BSCG_BNB_TOKEN", "")
    api = os.environ.get("BSCG_API", "https://api.bscquery.com")
    port = int(os.environ.get("PORT", "8080"))
    if not secret:
        sys.exit('Set BSCG_WEBHOOK_SECRET (the signing secret under "Webhook" on the BNB plan page)')
    store = Store(os.environ.get("STATE_FILE", "state.json"))
    if token:
        hours = int(os.environ.get("BACKFILL_HOURS", "24"))
        threading.Thread(target=backfill_loop, args=(store, api, token, hours), daemon=True).start()
    else:
        print("BSCG_BNB_TOKEN not set, backfill disabled")
    print(f"listening on http://0.0.0.0:{port}/bnb-webhook", flush=True)
    serve(store, secret, port).serve_forever()


if __name__ == "__main__":
    main()
