import hashlib
import hmac
import json
import os
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import server

ADDRESS = "0x1111111111111111111111111111111111111111"


def event(id, status, direction="in", amount_wei="500000000000000000", kind="external"):
    return {"id": id, "status": status, "address": ADDRESS, "direction": direction, "amount": "0.5",
            "amountWei": amount_wei, "kind": kind, "txHash": "0xabc", "blockNumber": 100, "balance": "1"}


def sign(secret, timestamp, body):
    return "sha256=" + hmac.new(secret.encode(), f"{timestamp}.".encode() + body, hashlib.sha256).hexdigest()


class StoreTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.store = server.Store(os.path.join(self.dir.name, "state.json"), log=lambda *_: None)

    def tearDown(self):
        self.dir.cleanup()

    def balance(self):
        return self.store.state["balances"].get(ADDRESS)

    def test_verify(self):
        body, now = b'{"events":[]}', 1791100000
        ts = str(now)
        self.assertTrue(server.verify("secret", ts, sign("secret", ts, body), body, now))
        self.assertFalse(server.verify("secret", ts, sign("other", ts, body), body, now))
        self.assertFalse(server.verify("secret", ts, sign("secret", ts, body), b'{"events": []}', now))
        self.assertFalse(server.verify("secret", ts, sign("secret", ts, body), body, now + 301))
        self.assertFalse(server.verify("secret", None, sign("secret", ts, body), body, now))
        self.assertFalse(server.verify("secret", ts, None, body, now))

    def test_events_accepts_both_shapes(self):
        self.assertEqual(server.events({"events": [1, 2]}), [1, 2])
        self.assertEqual(server.events({"batches": [{"events": [1]}, {"events": [2, 3]}]}), [1, 2, 3])

    def test_credits_once_on_finalized(self):
        self.store.apply_all([event("a", "pending")])
        self.assertIsNone(self.balance())
        self.store.apply_all([event("a", "finalized")])
        self.store.apply_all([event("a", "finalized")])
        self.store.apply_all([event("a", "pending")])
        self.assertEqual(self.balance(), "500000000000000000")

    def test_removed_is_never_credited(self):
        self.store.apply_all([event("a", "pending"), event("a", "removed")])
        self.store.apply_all([event("a", "finalized")])
        self.assertIsNone(self.balance())
        self.assertEqual(self.store.state["events"]["a"]["status"], "removed")

    def test_out_and_fee_debited_test_ignored(self):
        self.store.apply_all([
            event("in", "finalized", amount_wei="1000000000000000000"),
            event("out", "finalized", direction="out", amount_wei="300000000000000000"),
            event("fee", "finalized", direction="out", amount_wei="21000000000000", kind="fee"),
            event("t", "finalized", kind="test"),
        ])
        self.assertEqual(self.balance(), "699979000000000000")
        self.assertNotIn("t", self.store.state["events"])

    def test_failing_batch_leaves_no_trace(self):
        self.store.apply_all([event("a", "finalized")])
        with self.assertRaises(ValueError):
            self.store.apply_all([event("b", "finalized"), event("c", "finalized", amount_wei="oops")])
        self.assertNotIn("b", self.store.state["events"])
        self.assertEqual(self.balance(), "500000000000000000")

        self.store.apply_all([event("b", "finalized"), event("c", "finalized")])
        self.assertEqual(self.balance(), "1500000000000000000")
        self.assertEqual(server.Store(self.store.path).state, self.store.state)


class HTTPTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.store = server.Store(os.path.join(self.dir.name, "state.json"), log=lambda *_: None)
        self.httpd = server.serve(self.store, "secret", 0)
        self.httpd.RequestHandlerClass.log_message = lambda *_: None
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}/bnb-webhook"

    def tearDown(self):
        self.httpd.shutdown()
        self.httpd.server_close()
        self.dir.cleanup()

    def post(self, body, signature=None):
        ts = str(int(time.time()))
        request = urllib.request.Request(self.url, data=body, method="POST", headers={
            "X-BSCG-Timestamp": ts, "X-BSCG-Signature": signature or sign("secret", ts, body)})
        try:
            with urllib.request.urlopen(request) as response:
                return response.status
        except urllib.error.HTTPError as err:
            return err.code

    def test_webhook(self):
        batch = json.dumps({"batches": [{"events": [event("a", "finalized")]}]}).encode()
        self.assertEqual(self.post(batch, signature="sha256=bad"), 401)
        self.assertEqual(self.post(b"not json"), 400)
        self.assertEqual(self.post(json.dumps({"events": [event("x", "finalized", amount_wei="oops")]}).encode()), 500)
        self.assertEqual(self.post(batch), 200)
        self.assertEqual(self.post(batch), 200)
        self.assertEqual(self.store.state["balances"], {ADDRESS: "500000000000000000"})


class BackfillTest(unittest.TestCase):
    def test_follows_cursor_and_dedupes(self):
        pages = {
            "": {"data": [event("a", "finalized"), event("b", "pending")], "nextCursor": "p2"},
            "p2": {"data": [event("a", "finalized"), event("b", "finalized")], "nextCursor": ""},
        }
        seen = []

        class API(BaseHTTPRequestHandler):
            def do_GET(self):
                query = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
                seen.append((self.headers["Authorization"], int(query["since"][0])))
                body = json.dumps(pages[query.get("cursor", [""])[0]]).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_):
                pass

        api = ThreadingHTTPServer(("127.0.0.1", 0), API)
        threading.Thread(target=api.serve_forever, daemon=True).start()
        self.addCleanup(api.server_close)
        self.addCleanup(api.shutdown)

        with tempfile.TemporaryDirectory() as tmp:
            store = server.Store(os.path.join(tmp, "state.json"), log=lambda *_: None)
            total = server.backfill(store, f"http://127.0.0.1:{api.server_address[1]}", "tok", 2)
            self.assertEqual(total, 4)
            self.assertEqual(store.state["balances"][ADDRESS], "1000000000000000000")
        self.assertEqual(len(seen), 2)
        self.assertEqual(seen[0][0], "Bearer tok")
        self.assertLess(abs(seen[0][1] - (time.time() - 7200)), 5)


if __name__ == "__main__":
    unittest.main()
