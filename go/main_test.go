package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

const address = "0x1111111111111111111111111111111111111111"

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	m.Run()
}

func event(id, status string) Event {
	return Event{ID: id, Status: status, Address: address, Direction: "in", Amount: "0.5",
		AmountWei: "500000000000000000", Kind: "external", TxHash: "0xabc", BlockNumber: 100, Balance: "1"}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(body)))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func mustApply(t *testing.T, store *Store, events ...Event) {
	t.Helper()
	if err := store.ApplyAll(events); err != nil {
		t.Fatal(err)
	}
}

func TestVerify(t *testing.T) {
	body := []byte(`{"events":[]}`)
	now := time.Unix(1791100000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	cases := []struct {
		name      string
		secret    string
		timestamp string
		body      []byte
		now       time.Time
		want      bool
	}{
		{"valid", "secret", ts, body, now, true},
		{"wrong secret", "other", ts, body, now, false},
		{"tampered body", "secret", ts, []byte(`{"events": []}`), now, false},
		{"stale", "secret", ts, body, now.Add(301 * time.Second), false},
		{"missing timestamp", "secret", "", body, now, false},
	}
	for _, c := range cases {
		if got := Verify(c.secret, c.timestamp, sign("secret", ts, body), c.body, c.now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPayloadEvents(t *testing.T) {
	var single, batched Payload
	json.Unmarshal([]byte(`{"events":[{"id":"a"}]}`), &single)
	json.Unmarshal([]byte(`{"batches":[{"events":[{"id":"a"}]},{"events":[{"id":"b"},{"id":"c"}]}]}`), &batched)
	if len(single.Events()) != 1 || len(batched.Events()) != 3 {
		t.Fatalf("got %d and %d events", len(single.Events()), len(batched.Events()))
	}
}

func TestCreditsOnceOnFinalized(t *testing.T) {
	store := newStore(t)
	mustApply(t, store, event("a", "pending"))
	if got := store.state.Balances[address]; got != "" {
		t.Fatalf("pending credited: %s", got)
	}
	mustApply(t, store, event("a", "finalized"))
	mustApply(t, store, event("a", "finalized"))
	mustApply(t, store, event("a", "pending"))
	if got := store.state.Balances[address]; got != "500000000000000000" {
		t.Fatalf("balance %s", got)
	}
}

func TestRemovedIsNeverCredited(t *testing.T) {
	store := newStore(t)
	mustApply(t, store, event("a", "pending"), event("a", "removed"))
	mustApply(t, store, event("a", "finalized"))
	if got := store.state.Balances[address]; got != "" {
		t.Fatalf("removed credited: %s", got)
	}
	if got := store.state.Events["a"].Status; got != "removed" {
		t.Fatalf("status %s", got)
	}
}

func TestOutAndFeeDebitedTestIgnored(t *testing.T) {
	store := newStore(t)
	in := event("in", "finalized")
	in.AmountWei = "1000000000000000000"
	out := event("out", "finalized")
	out.Direction, out.AmountWei = "out", "300000000000000000"
	fee := event("fee", "finalized")
	fee.Direction, fee.AmountWei, fee.Kind = "out", "21000000000000", "fee"
	test := event("t", "finalized")
	test.Kind = "test"
	mustApply(t, store, in, out, fee, test)
	if got := store.state.Balances[address]; got != "699979000000000000" {
		t.Fatalf("balance %s", got)
	}
	if _, ok := store.state.Events["t"]; ok {
		t.Fatal("test push recorded")
	}
}

func TestFailingBatchLeavesNoTrace(t *testing.T) {
	store := newStore(t)
	mustApply(t, store, event("a", "finalized"))
	bad := event("c", "finalized")
	bad.AmountWei = "oops"
	if err := store.ApplyAll([]Event{event("b", "finalized"), bad}); err == nil {
		t.Fatal("bad amountWei accepted")
	}
	if _, ok := store.state.Events["b"]; ok {
		t.Fatal("half-applied batch kept")
	}
	mustApply(t, store, event("b", "finalized"), event("c", "finalized"))
	if got := store.state.Balances[address]; got != "1500000000000000000" {
		t.Fatalf("balance %s", got)
	}
	reopened, err := NewStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.state, store.state) {
		t.Fatal("state not persisted")
	}
}

func TestHandler(t *testing.T) {
	store := newStore(t)
	server := httptest.NewServer(NewHandler(store, "secret"))
	defer server.Close()

	post := func(body []byte, signature string) int {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		if signature == "" {
			signature = sign("secret", ts, body)
		}
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/bnb-webhook", bytes.NewReader(body))
		request.Header.Set("X-BSCG-Timestamp", ts)
		request.Header.Set("X-BSCG-Signature", signature)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}

	batch, _ := json.Marshal(map[string]any{"batches": []Block{{Events: []Event{event("a", "finalized")}}}})
	bad := event("x", "finalized")
	bad.AmountWei = "oops"
	failing, _ := json.Marshal(Block{Events: []Event{bad}})
	for _, c := range []struct {
		name      string
		body      []byte
		signature string
		want      int
	}{
		{"bad signature", batch, "sha256=bad", 401},
		{"not json", []byte("not json"), "", 400},
		{"processing error", failing, "", 500},
		{"batch", batch, "", 200},
		{"duplicate", batch, "", 200},
	} {
		if got := post(c.body, c.signature); got != c.want {
			t.Errorf("%s: HTTP %d, want %d", c.name, got, c.want)
		}
	}
	if got := store.state.Balances; !reflect.DeepEqual(got, map[string]string{address: "500000000000000000"}) {
		t.Fatalf("balances %v", got)
	}
}

func TestBackfillFollowsCursorAndDedupes(t *testing.T) {
	pages := map[string]any{
		"":   map[string]any{"data": []Event{event("a", "finalized"), event("b", "pending")}, "nextCursor": "p2"},
		"p2": map[string]any{"data": []Event{event("a", "finalized"), event("b", "finalized")}, "nextCursor": ""},
	}
	var auth []string
	var since int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		since, _ = strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		json.NewEncoder(w).Encode(pages[r.URL.Query().Get("cursor")])
	}))
	defer api.Close()

	store := newStore(t)
	total, err := Backfill(api.Client(), store, api.URL, "tok", 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(auth) != 2 || auth[0] != "Bearer tok" {
		t.Fatalf("total %d, requests %v", total, auth)
	}
	if drift := time.Since(time.Unix(since, 0)) - 2*time.Hour; drift.Abs() > 5*time.Second {
		t.Fatalf("since off by %s", drift)
	}
	if got := store.state.Balances[address]; got != "1000000000000000000" {
		t.Fatalf("balance %s", got)
	}
}
