// BSC Query BNB balance-change webhook receiver. Go standard library only.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	maxSkew      = 5 * time.Minute
	maxBodyBytes = 32 << 20
)

type Event struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Address     string `json:"address"`
	Direction   string `json:"direction"`
	Amount      string `json:"amount"`
	AmountWei   string `json:"amountWei"`
	Kind        string `json:"kind"`
	TxHash      string `json:"txHash"`
	BlockNumber int64  `json:"blockNumber"`
	Balance     string `json:"balance"`
}

type Block struct {
	Events []Event `json:"events"`
}

// With "multi-block delivery" enabled the body is {"batches": [...]}; accept both shapes.
type Payload struct {
	Block
	Batches []Block `json:"batches"`
}

func (p Payload) Events() []Event {
	if p.Batches == nil {
		return p.Block.Events
	}
	var events []Event
	for _, block := range p.Batches {
		events = append(events, block.Events...)
	}
	return events
}

// Verify checks the signature over the raw request body: parsing and re-serializing the JSON
// changes the bytes.
func Verify(secret, timestamp, signature string, body []byte, now time.Time) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || now.Sub(time.Unix(ts, 0)).Abs() > maxSkew {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hmac.Equal([]byte(signature), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))
}

type state struct {
	Events   map[string]Event  `json:"events"`
	Balances map[string]string `json:"balances"`
}

// Store keeps the demo restartable with a JSON file. In production use a database and handle
// each request in one transaction.
type Store struct {
	mu    sync.Mutex
	path  string
	state state
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	return s, s.reload()
}

func (s *Store) reload() error {
	s.state = state{Events: map[string]Event{}, Balances: map[string]string{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &s.state)
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ApplyAll is all or nothing: if any event fails, it drops the half-applied state so the
// platform's retry replays the whole request against the last saved state.
func (s *Store) ApplyAll(events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range events {
		if err := s.apply(event); err != nil {
			return errors.Join(err, s.reload())
		}
	}
	if err := s.save(); err != nil {
		return errors.Join(err, s.reload())
	}
	return nil
}

func (s *Store) apply(event Event) error {
	if event.Kind == "test" {
		log.Println("test push received", event.ID)
		return nil
	}
	// finalized and removed are terminal. Skipping them makes retries, duplicates and backfill
	// overlaps credit each event exactly once.
	if known, ok := s.state.Events[event.ID]; ok && known.Status != "pending" {
		return nil
	}
	switch event.Status {
	case "finalized":
		if err := s.credit(event); err != nil {
			return err
		}
	case "pending":
		log.Printf("confirming %s %s %s BNB (%s) tx=%s", event.Address, event.Direction, event.Amount, event.Kind, event.TxHash)
	case "removed":
		log.Printf("reorged, dropping %s tx=%s", event.ID, event.TxHash)
	}
	s.state.Events[event.ID] = event
	return nil
}

func (s *Store) credit(event Event) error {
	delta, ok := new(big.Int).SetString(event.AmountWei, 10)
	if !ok {
		return fmt.Errorf("invalid amountWei %q", event.AmountWei)
	}
	if event.Direction != "in" {
		delta.Neg(delta)
	}
	balance, ok := new(big.Int).SetString(s.state.Balances[event.Address], 10)
	if !ok {
		balance = new(big.Int)
	}
	s.state.Balances[event.Address] = balance.Add(balance, delta).String()
	log.Printf("credited %s %s %s BNB (%s) on-chain balance %s BNB tx=%s", event.Address, event.Direction, event.Amount, event.Kind, event.Balance, event.TxHash)
	return nil
}

// Backfill pulls every change whose status moved in the last hours hours and applies the ones
// that were never delivered. Overlapping an earlier window is harmless because apply dedupes by id.
func Backfill(client *http.Client, store *Store, api, token string, hours int) (int, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	cursor, total := "", 0
	for {
		query := url.Values{"since": {strconv.FormatInt(since, 10)}, "limit": {"500"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		request, err := http.NewRequest(http.MethodGet, api+"/v1/bnb/events?"+query.Encode(), nil)
		if err != nil {
			return total, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("User-Agent", "bsc-bnb-deposit-webhook/1")
		response, err := client.Do(request)
		if err != nil {
			return total, err
		}
		var page struct {
			Data       []Event `json:"data"`
			NextCursor string  `json:"nextCursor"`
		}
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
			response.Body.Close()
			return total, fmt.Errorf("backfill failed: HTTP %d %s", response.StatusCode, body)
		}
		err = json.NewDecoder(response.Body).Decode(&page)
		response.Body.Close()
		if err != nil {
			return total, err
		}
		if err := store.ApplyAll(page.Data); err != nil {
			return total, err
		}
		total += len(page.Data)
		if cursor = page.NextCursor; cursor == "" {
			return total, nil
		}
	}
}

func NewHandler(store *Store, secret string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /bnb-webhook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if !Verify(secret, r.Header.Get("X-BSCG-Timestamp"), r.Header.Get("X-BSCG-Signature"), body, time.Now()) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var payload Payload
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := store.ApplyAll(payload.Events()); err != nil {
			log.Println("processing failed, waiting for the platform to retry", r.Header.Get("X-BSCG-Delivery-Id"), err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	secret := os.Getenv("BSCG_WEBHOOK_SECRET")
	token := os.Getenv("BSCG_BNB_TOKEN")
	api := env("BSCG_API", "https://api.bscquery.com")
	if secret == "" {
		log.Fatal(`Set BSCG_WEBHOOK_SECRET (the signing secret under "Webhook" on the BNB plan page)`)
	}
	store, err := NewStore(env("STATE_FILE", "state.json"))
	if err != nil {
		log.Fatal(err)
	}

	if token != "" {
		hours, err := strconv.Atoi(env("BACKFILL_HOURS", "24"))
		if err != nil {
			log.Fatal("invalid BACKFILL_HOURS: ", err)
		}
		client := &http.Client{Timeout: 30 * time.Second}
		go func() {
			for {
				if total, err := Backfill(client, store, api, token, hours); err != nil {
					log.Println(err)
				} else {
					log.Printf("backfill done: %d changes in the last %dh", total, hours)
				}
				time.Sleep(time.Hour)
				hours = 2
			}
		}()
	} else {
		log.Println("BSCG_BNB_TOKEN not set, backfill disabled")
	}

	server := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           NewHandler(store, secret),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	log.Printf("listening on http://0.0.0.0%s/bnb-webhook", server.Addr)
	log.Fatal(server.ListenAndServe())
}
