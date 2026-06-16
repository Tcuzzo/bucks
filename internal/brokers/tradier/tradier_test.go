package tradier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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

// fakeTradier mirrors Tradier's brokerage JSON shapes over httptest. It enforces
// the OAuth2 Bearer (401 without it) and records placed orders by tag. NO live
// network.
type fakeTradier struct {
	t          *testing.T
	server     *httptest.Server
	accountID  string
	wantToken  string
	mu         sync.Mutex
	placedTags []string
}

func newFakeTradier(t *testing.T, accountID, token string) *fakeTradier {
	f := &fakeTradier{t: t, accountID: accountID, wantToken: token}
	mux := http.NewServeMux()

	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"invalid bearer"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}

	base := "/v1/accounts/" + accountID

	mux.HandleFunc(base+"/balances", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, map[string]any{
			"balances": map[string]any{
				"total_cash":   "5000.00",
				"total_equity": "12000.00",
				"margin":       map[string]any{"stock_buying_power": "20000.00"},
				"cash":         map[string]any{"cash_available": "5000.00"},
			},
		})
	})

	mux.HandleFunc(base+"/positions", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, map[string]any{
			"positions": map[string]any{
				"position": map[string]any{
					"symbol": "AAPL", "quantity": "10", "cost_basis": "1500.00",
				},
			},
		})
	})

	mux.HandleFunc("/v1/markets/quotes", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		// Money is sent as DECIMAL TEXT (string literals), exactly how a real
		// broker serializes it on the wire — so the adapter's json.Number ->
		// ParseDecimal path is actually exercised. `last` is a drift-prone value
		// (100.003) that is NOT exactly representable in binary float64: had the
		// adapter parsed via float64 it would drift off 100.003; asserting the exact
		// decimal below proves the no-float64 guarantee.
		writeJSON(w, map[string]any{
			"quotes": map[string]any{
				"quote": map[string]any{
					"symbol": r.URL.Query().Get("symbols"),
					"bid":    "150.10", "ask": "150.20", "last": "100.003",
				},
			},
		})
	})

	mux.HandleFunc(base+"/orders", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		switch r.Method {
		case http.MethodPost:
			_ = r.ParseForm()
			tag := r.PostForm.Get("tag")
			f.mu.Lock()
			f.placedTags = append(f.placedTags, tag)
			f.mu.Unlock()
			writeJSON(w, map[string]any{
				"order": map[string]any{"id": 12345, "status": "ok", "tag": tag},
			})
		case http.MethodGet:
			writeJSON(w, map[string]any{
				"orders": map[string]any{
					"order": map[string]any{
						"id": 12345, "symbol": "AAPL", "side": "buy",
						"quantity": "10", "status": "open", "exec_quantity": "0",
						"avg_fill_price": "0", "tag": "clord-td",
					},
				},
			})
		}
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newBroker(t *testing.T, f *fakeTradier, token string) *TradierBroker {
	t.Helper()
	b, err := New(Config{AccessToken: token, BaseURL: f.server.URL, AccountID: f.accountID}, f.server.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return b
}

func TestTradier_OAuthBearer_AccountAndQuote(t *testing.T) {
	f := newFakeTradier(t, "VA1234", "tok-good")
	b := newBroker(t, f, "tok-good")

	acct, err := b.Account(context.Background())
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if !acct.Cash.Equal(dec(t, "5000.00")) {
		t.Errorf("cash = %s, want 5000.00", acct.Cash)
	}
	if !acct.Equity.Equal(dec(t, "12000.00")) {
		t.Errorf("equity = %s, want 12000.00", acct.Equity)
	}
	if !acct.BuyingPower.Equal(dec(t, "20000.00")) {
		t.Errorf("buying power = %s, want 20000.00", acct.BuyingPower)
	}

	q, err := b.Quote(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if !q.Bid.Equal(dec(t, "150.10")) {
		t.Errorf("quote bid = %s, want 150.10", q.Bid)
	}
	// Drift proof: the wire sent last="100.003", which is NOT exactly
	// representable in binary float64. If the adapter parsed via float64 the value
	// would drift (e.g. 100.00299999999999...); asserting the EXACT decimal
	// 100.003 proves the json.Number -> ParseDecimal (decimal-text) path with no
	// float64 in the middle.
	if !q.Last.Equal(dec(t, "100.003")) {
		t.Errorf("quote last = %s, want EXACT 100.003 (no float64 drift)", q.Last)
	}
	if q.Last.String() != "100.003" {
		t.Errorf("quote last string = %q, want exact %q — decimal-text path not exercised", q.Last.String(), "100.003")
	}
}

func TestTradier_BadBearer_Unauthorized(t *testing.T) {
	f := newFakeTradier(t, "VA1234", "tok-good")
	b := newBroker(t, f, "tok-WRONG")
	if _, err := b.Account(context.Background()); err == nil {
		t.Fatalf("expected auth error with a wrong bearer, got nil")
	}
}

func TestTradier_PlaceOrder_MapsTag(t *testing.T) {
	f := newFakeTradier(t, "VA1234", "tok-good")
	b := newBroker(t, f, "tok-good")

	bo, err := b.PlaceOrder(context.Background(), brokers.OrderRequest{
		ClOrdID: "clord-td", Symbol: "AAPL", Side: orders.SideBuy,
		Qty: dec(t, "10"), Kind: brokers.KindLimit, LimitPx: dec(t, "149.50"),
	})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if bo.BrokerOrderID != "12345" {
		t.Errorf("broker order id = %q, want 12345", bo.BrokerOrderID)
	}
	if bo.ClOrdID != "clord-td" {
		t.Errorf("ClOrdID = %q, want clord-td", bo.ClOrdID)
	}
	// Our ClOrdID reached the venue as the `tag` verbatim.
	if len(f.placedTags) != 1 || f.placedTags[0] != "clord-td" {
		t.Errorf("venue saw tags %v, want [clord-td]", f.placedTags)
	}
}

func TestTradier_GetOrder_ByTag(t *testing.T) {
	f := newFakeTradier(t, "VA1234", "tok-good")
	b := newBroker(t, f, "tok-good")

	bo, err := b.GetOrder(context.Background(), "clord-td")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if bo.Symbol != "AAPL" || bo.Side != orders.SideBuy {
		t.Errorf("got %+v, want AAPL buy", bo)
	}

	// A tag that doesn't exist returns ErrOrderNotFound.
	if _, err := b.GetOrder(context.Background(), "nope"); err != brokers.ErrOrderNotFound {
		t.Errorf("get unknown order = %v, want ErrOrderNotFound", err)
	}
}

func TestTradier_Positions_CostBasisAvg(t *testing.T) {
	f := newFakeTradier(t, "VA1234", "tok-good")
	b := newBroker(t, f, "tok-good")
	pos, err := b.Positions(context.Background())
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(pos) != 1 || pos[0].Symbol != "AAPL" {
		t.Fatalf("positions = %+v, want one AAPL", pos)
	}
	// cost_basis 1500 / qty 10 = 150.00 avg entry.
	if !pos[0].AvgEntryPx.Equal(dec(t, "150.00")) {
		t.Errorf("avg entry = %s, want 150.00", pos[0].AvgEntryPx)
	}
}
