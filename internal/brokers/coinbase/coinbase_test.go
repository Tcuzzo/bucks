package coinbase

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

func dec(t *testing.T, s string) orders.Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return d
}

// fakeClock is a deterministic, advanceable clock for driving JWT expiry.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeCoinbase is an httptest server mirroring the Advanced Trade JSON shapes.
// It records every Bearer token it saw, so a test can prove the adapter refreshes
// the JWT (a NEW token after the old one expires). NO live network.
type fakeCoinbase struct {
	t            *testing.T
	server       *httptest.Server
	mu           sync.Mutex
	seenTokens   []string
	placedClOrd  []string
	ordersByClID map[string]map[string]any
}

func newFakeCoinbase(t *testing.T) *fakeCoinbase {
	f := &fakeCoinbase{t: t, ordersByClID: map[string]map[string]any{}}
	mux := http.NewServeMux()

	record := func(r *http.Request) bool {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			return false
		}
		f.mu.Lock()
		f.seenTokens = append(f.seenTokens, strings.TrimPrefix(auth, "Bearer "))
		f.mu.Unlock()
		return true
	}

	mux.HandleFunc("/api/v3/brokerage/accounts", func(w http.ResponseWriter, r *http.Request) {
		if !record(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{
			"accounts": []map[string]any{
				{"currency": "USD", "available_balance": map[string]any{"value": "1500.25"}},
				{"currency": "BTC", "available_balance": map[string]any{"value": "0.50000000"}},
			},
		})
	})

	mux.HandleFunc("/api/v3/brokerage/products/", func(w http.ResponseWriter, r *http.Request) {
		if !record(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{
			"product_id": strings.TrimPrefix(r.URL.Path, "/api/v3/brokerage/products/"),
			"price":      "65000.10",
			"best_bid":   "64999.00",
			"best_ask":   "65001.00",
		})
	})

	mux.HandleFunc("/api/v3/brokerage/orders", func(w http.ResponseWriter, r *http.Request) {
		if !record(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method"}`, http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		clID, _ := req["client_order_id"].(string)
		f.mu.Lock()
		f.placedClOrd = append(f.placedClOrd, clID)
		f.mu.Unlock()
		writeJSON(w, map[string]any{
			"order_id":             "cb-ord-1",
			"client_order_id":      clID,
			"product_id":           req["product_id"],
			"side":                 req["side"],
			"status":               "PENDING",
			"filled_size":          "0",
			"average_filled_price": "0",
			"order_size":           "0.01",
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeCoinbase) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seenTokens))
	copy(out, f.seenTokens)
	return out
}

func TestCoinbase_JWTAutoRefreshBeforeExpiry(t *testing.T) {
	fake := newFakeCoinbase(t)
	clk := &fakeClock{t: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)}

	// Count how many times a token is minted, and stamp each with the issue time
	// so we can see a NEW token after expiry.
	var mintCount int
	mint := func(cfg Config, issuedAt time.Time) (string, time.Time) {
		mintCount++
		return "tok-" + issuedAt.Format("150405.000"), issuedAt.Add(2 * time.Minute)
	}

	cb, err := New(Config{
		APIKeyName: "k", APISecret: "s",
		BaseURL:  fake.server.URL,
		TokenTTL: 2 * time.Minute,
		Now:      clk.now,
		Mint:     mint,
	}, fake.server.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// Call 1 at t=0 → mints token A.
	if _, err := cb.Account(context.Background()); err != nil {
		t.Fatalf("account #1: %v", err)
	}
	if mintCount != 1 {
		t.Fatalf("after first call mintCount=%d, want 1", mintCount)
	}

	// Call 2 at t=30s → token A still valid (well within 2-min TTL, beyond skew)
	// → NO new mint.
	clk.advance(30 * time.Second)
	if _, err := cb.Account(context.Background()); err != nil {
		t.Fatalf("account #2: %v", err)
	}
	if mintCount != 1 {
		t.Fatalf("at +30s mintCount=%d, want still 1 (token reused)", mintCount)
	}

	// Call 3 at t=1m55s → within the 10s refresh skew of the 2-min expiry →
	// adapter refreshes EARLY (before the token actually expires).
	clk.advance(85 * time.Second) // now +1m55s
	if _, err := cb.Account(context.Background()); err != nil {
		t.Fatalf("account #3: %v", err)
	}
	if mintCount != 2 {
		t.Fatalf("at +1m55s mintCount=%d, want 2 (refreshed before expiry)", mintCount)
	}

	// The server saw two DISTINCT tokens — proving the bearer actually rotated.
	toks := fake.tokens()
	if len(toks) < 3 {
		t.Fatalf("server saw %d tokens, want >=3", len(toks))
	}
	first, last := toks[0], toks[len(toks)-1]
	if first == last {
		t.Fatalf("token never rotated on the wire: first=%q last=%q", first, last)
	}
}

func TestCoinbase_Account_Quote_Place(t *testing.T) {
	fake := newFakeCoinbase(t)
	cb, err := New(Config{APIKeyName: "k", APISecret: "s", BaseURL: fake.server.URL}, fake.server.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	acct, err := cb.Account(context.Background())
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if !acct.Cash.Equal(dec(t, "1500.25")) {
		t.Errorf("cash = %s, want 1500.25", acct.Cash)
	}

	q, err := cb.Quote(context.Background(), "BTC-USD")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if !q.Bid.Equal(dec(t, "64999.00")) || !q.Ask.Equal(dec(t, "65001.00")) {
		t.Errorf("quote bid/ask = %s/%s, want 64999.00/65001.00", q.Bid, q.Ask)
	}

	bo, err := cb.PlaceOrder(context.Background(), brokers.OrderRequest{
		ClOrdID: "clord-xyz", Symbol: "BTC-USD", Side: orders.SideBuy,
		Qty: dec(t, "0.01"), Kind: brokers.KindMarket,
	})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if bo.ClOrdID != "clord-xyz" {
		t.Errorf("ClOrdID = %q, want clord-xyz (idempotency key mapped)", bo.ClOrdID)
	}
	if bo.Status != brokers.StatusNew {
		t.Errorf("status = %v, want New", bo.Status)
	}
	// The idempotency key reached the venue verbatim.
	if len(fake.placedClOrd) != 1 || fake.placedClOrd[0] != "clord-xyz" {
		t.Errorf("venue saw client_order_ids %v, want [clord-xyz]", fake.placedClOrd)
	}
}

func TestCoinbase_Positions_ExcludesFiat(t *testing.T) {
	fake := newFakeCoinbase(t)
	cb, _ := New(Config{BaseURL: fake.server.URL}, fake.server.Client())
	pos, err := cb.Positions(context.Background())
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(pos) != 1 || pos[0].Symbol != "BTC" {
		t.Fatalf("positions = %+v, want one BTC position (fiat excluded)", pos)
	}
	if !pos[0].Qty.Equal(dec(t, "0.5")) {
		t.Errorf("BTC qty = %s, want 0.5", pos[0].Qty)
	}
}
