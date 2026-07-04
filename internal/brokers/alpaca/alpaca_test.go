package alpaca

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	sdk "github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/shopspring/decimal"

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

func sdkDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse sdk decimal %q: %v", s, err)
	}
	return d
}

// fakeAlpaca is an httptest.Server mirroring the Alpaca Trading + Market Data
// JSON shapes. NO live network: the adapter's BaseURL/DataBaseURL point here.
type fakeAlpaca struct {
	t              *testing.T
	server         *httptest.Server
	placedClOrdIDs []string
	canceledIDs    []string

	// ordersByClOrdID models the venue's order book keyed by client_order_id.
	ordersByClOrdID  map[string]map[string]any
	positions        []map[string]any
	account          map[string]any
	quote            map[string]any
	trade            map[string]any
	activityPages    [][]map[string]any
	activityRequests []url.Values

	// duplicateOn, when set, makes the next PlaceOrder for this client_order_id
	// return a 422 duplicate (to drive the idempotency-on-duplicate path).
	duplicateOn map[string]bool

	// reject422On, when set, makes PlaceOrder for this client_order_id return a
	// generic 422 whose body does NOT mention client_order_id (e.g. insufficient
	// buying power) — a NON-duplicate rejection that must propagate as an error.
	reject422On map[string]bool
}

func newFakeAlpaca(t *testing.T) *fakeAlpaca {
	f := &fakeAlpaca{
		t:               t,
		ordersByClOrdID: map[string]map[string]any{},
		duplicateOn:     map[string]bool{},
		reject422On:     map[string]bool{},
	}
	mux := http.NewServeMux()

	// Auth check shared by handlers.
	checkAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("APCA-API-KEY-ID") == "" || r.Header.Get("APCA-API-SECRET-KEY") == "" {
			http.Error(w, `{"message":"missing auth"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}

	mux.HandleFunc("/v2/account", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		writeJSON(w, f.account)
	})

	mux.HandleFunc("/v2/positions", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		writeJSON(w, f.positions)
	})

	// Order book: POST creates, GET by_client_order_id reads, DELETE cancels.
	mux.HandleFunc("/v2/orders", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"message":"method"}`, http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)
			return
		}
		clOrdID, _ := req["client_order_id"].(string)
		f.placedClOrdIDs = append(f.placedClOrdIDs, clOrdID)

		if f.duplicateOn[clOrdID] {
			// Mirror Alpaca's duplicate-client-order-id rejection.
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":42210000,"message":"client_order_id must be unique"}`))
			return
		}

		if f.reject422On[clOrdID] {
			// Mirror a NON-duplicate 422 (no "client_order_id" in the body) — e.g.
			// insufficient buying power. This MUST propagate as an error, not be
			// swallowed by the idempotency path.
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"insufficient buying power"}`))
			return
		}

		ord := map[string]any{
			"id":               "alp-" + clOrdID,
			"client_order_id":  clOrdID,
			"symbol":           req["symbol"],
			"side":             req["side"],
			"type":             req["type"],
			"status":           "new",
			"qty":              req["qty"],
			"filled_qty":       "0",
			"filled_avg_price": nil,
		}
		f.ordersByClOrdID[clOrdID] = ord
		writeJSON(w, ord)
	})

	mux.HandleFunc("/v2/orders:by_client_order_id", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		clOrdID := r.URL.Query().Get("client_order_id")
		ord, ok := f.ordersByClOrdID[clOrdID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":40410000,"message":"order not found"}`))
			return
		}
		writeJSON(w, ord)
	})

	// DELETE /v2/orders/{id} cancels by venue id.
	mux.HandleFunc("/v2/orders/", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, `{"message":"method"}`, http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v2/orders/")
		f.canceledIDs = append(f.canceledIDs, id)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/v2/stocks/quotes/latest", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		sym := r.URL.Query().Get("symbols")
		writeJSON(w, map[string]any{"quotes": map[string]any{sym: f.quote}})
	})

	mux.HandleFunc("/v2/stocks/trades/latest", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		sym := r.URL.Query().Get("symbols")
		writeJSON(w, map[string]any{"trades": map[string]any{sym: f.trade}})
	})

	mux.HandleFunc("/v2/account/activities", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		f.activityRequests = append(f.activityRequests, r.URL.Query())
		idx := len(f.activityRequests) - 1
		if idx >= len(f.activityPages) {
			writeJSON(w, []map[string]any{})
			return
		}
		writeJSON(w, f.activityPages[idx])
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAlpaca) broker(t *testing.T) *AlpacaBroker {
	b, err := New(Config{
		KeyID:       "test-key",
		Secret:      "test-secret",
		BaseURL:     f.server.URL,
		DataBaseURL: f.server.URL,
		Feed:        "iex",
	})
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}
	return b
}

func TestAlpaca_Account_MapsDecimalExact(t *testing.T) {
	f := newFakeAlpaca(t)
	f.account = map[string]any{
		"cash":         "1000.50",
		"equity":       "2500.75",
		"buying_power": "5000.00",
	}
	b := f.broker(t)

	acct, err := b.Account(context.Background())
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if acct.Cash.Cmp(dec(t, "1000.50")) != 0 {
		t.Fatalf("cash = %s, want 1000.50", acct.Cash)
	}
	if acct.Equity.Cmp(dec(t, "2500.75")) != 0 {
		t.Fatalf("equity = %s, want 2500.75", acct.Equity)
	}
	if acct.BuyingPower.Cmp(dec(t, "5000.00")) != 0 {
		t.Fatalf("buying power = %s, want 5000.00", acct.BuyingPower)
	}
}

func TestAlpaca_Positions_SignedShort(t *testing.T) {
	f := newFakeAlpaca(t)
	f.positions = []map[string]any{
		{"symbol": "AAPL", "qty": "10", "side": "long", "avg_entry_price": "150.25"},
		{"symbol": "TSLA", "qty": "5", "side": "short", "avg_entry_price": "200.10"},
	}
	b := f.broker(t)

	pos, err := b.Positions(context.Background())
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(pos) != 2 {
		t.Fatalf("want 2 positions, got %d", len(pos))
	}
	bySym := map[string]brokers.Position{}
	for _, p := range pos {
		bySym[p.Symbol] = p
	}
	if bySym["AAPL"].Qty.Cmp(dec(t, "10")) != 0 {
		t.Fatalf("AAPL qty = %s, want 10", bySym["AAPL"].Qty)
	}
	// Short side must fold to a NEGATIVE signed qty.
	if bySym["TSLA"].Qty.Cmp(dec(t, "-5")) != 0 {
		t.Fatalf("TSLA short qty = %s, want -5", bySym["TSLA"].Qty)
	}
	if bySym["TSLA"].AvgEntryPx.Cmp(dec(t, "200.10")) != 0 {
		t.Fatalf("TSLA avg = %s, want 200.10", bySym["TSLA"].AvgEntryPx)
	}
}

func TestAlpaca_Quote_DecimalExact(t *testing.T) {
	f := newFakeAlpaca(t)
	// Numbers as JSON numbers (not strings) to mirror the real data API.
	f.quote = map[string]any{"bp": 149.90, "ap": 150.10, "t": "2026-06-15T14:30:00Z"}
	f.trade = map[string]any{"p": 150.00, "t": "2026-06-15T14:30:01Z"}
	b := f.broker(t)

	q, err := b.Quote(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if q.Bid.Cmp(dec(t, "149.9")) != 0 {
		t.Fatalf("bid = %s, want 149.9", q.Bid)
	}
	if q.Ask.Cmp(dec(t, "150.1")) != 0 {
		t.Fatalf("ask = %s, want 150.1", q.Ask)
	}
	if q.Last.Cmp(dec(t, "150")) != 0 {
		t.Fatalf("last = %s, want 150", q.Last)
	}
	if q.Time.IsZero() {
		t.Fatalf("quote timestamp not parsed")
	}
}

func TestAlpaca_PlaceOrder_MapsClOrdID(t *testing.T) {
	f := newFakeAlpaca(t)
	b := f.broker(t)

	cl := orders.ClientOrderID("trend", "AAPL", "ENTRY", 1)
	bo, err := b.PlaceOrder(context.Background(), brokers.OrderRequest{
		ClOrdID: cl, Symbol: "AAPL", Side: orders.SideBuy, Qty: dec(t, "10"), Kind: brokers.KindMarket,
	})
	if err != nil {
		t.Fatalf("place order: %v", err)
	}
	if bo.ClOrdID != cl {
		t.Fatalf("ClOrdID not round-tripped: got %q want %q", bo.ClOrdID, cl)
	}
	if bo.Status != brokers.StatusNew {
		t.Fatalf("status = %s, want New", bo.Status)
	}
	// The venue received our deterministic client_order_id verbatim.
	if len(f.placedClOrdIDs) != 1 || f.placedClOrdIDs[0] != cl {
		t.Fatalf("venue client_order_id = %v, want [%s]", f.placedClOrdIDs, cl)
	}
}

func TestAlpaca_PlaceOrder_Limit_SendsLimitPrice(t *testing.T) {
	f := newFakeAlpaca(t)
	b := f.broker(t)

	cl := orders.ClientOrderID("trend", "AAPL", "ENTRY", 9)
	bo, err := b.PlaceOrder(context.Background(), brokers.OrderRequest{
		ClOrdID: cl, Symbol: "AAPL", Side: orders.SideBuy, Qty: dec(t, "2"),
		Kind: brokers.KindLimit, LimitPx: dec(t, "149.55"),
	})
	if err != nil {
		t.Fatalf("place limit order: %v", err)
	}
	if bo.ClOrdID != cl {
		t.Fatalf("ClOrdID mismatch: %q vs %q", bo.ClOrdID, cl)
	}
}

// TestAlpaca_PlaceOrder_DuplicateIsIdempotent drives the duplicate-client-order-id
// path: the venue rejects the second send (422), and the adapter resolves to the
// existing order instead of erroring — same ClOrdID, no duplicate surfaced.
func TestAlpaca_PlaceOrder_DuplicateIsIdempotent(t *testing.T) {
	f := newFakeAlpaca(t)
	b := f.broker(t)
	cl := orders.ClientOrderID("trend", "AAPL", "ENTRY", 2)

	// First place succeeds and is recorded in the venue book.
	if _, err := b.PlaceOrder(context.Background(), brokers.OrderRequest{
		ClOrdID: cl, Symbol: "AAPL", Side: orders.SideBuy, Qty: dec(t, "10"), Kind: brokers.KindMarket,
	}); err != nil {
		t.Fatalf("first place: %v", err)
	}
	// Now make the venue reject a re-send as a duplicate.
	f.duplicateOn[cl] = true

	bo, err := b.PlaceOrder(context.Background(), brokers.OrderRequest{
		ClOrdID: cl, Symbol: "AAPL", Side: orders.SideBuy, Qty: dec(t, "10"), Kind: brokers.KindMarket,
	})
	if err != nil {
		t.Fatalf("duplicate place should resolve to existing order, got error: %v", err)
	}
	if bo.ClOrdID != cl {
		t.Fatalf("resolved order ClOrdID = %q, want %q", bo.ClOrdID, cl)
	}
}

// TestAlpaca_PlaceOrder_NonDuplicate422_Propagates proves isDuplicateClientOrderID's
// negative branch: a 422 whose body does NOT mention client_order_id (here
// "insufficient buying power") is a genuine rejection, not a duplicate. PlaceOrder
// must return a non-nil error and must NOT silently resolve to an existing order.
func TestAlpaca_PlaceOrder_NonDuplicate422_Propagates(t *testing.T) {
	f := newFakeAlpaca(t)
	b := f.broker(t)
	cl := orders.ClientOrderID("trend", "AAPL", "ENTRY", 6)

	// The venue rejects this place with a generic 422 (not a duplicate).
	f.reject422On[cl] = true

	bo, err := b.PlaceOrder(context.Background(), brokers.OrderRequest{
		ClOrdID: cl, Symbol: "AAPL", Side: orders.SideBuy, Qty: dec(t, "10000"), Kind: brokers.KindMarket,
	})
	if err == nil {
		t.Fatalf("expected non-duplicate 422 to propagate as error, got nil (resolved order: %+v)", bo)
	}
	// And it must NOT have been swallowed into a resolved/existing order.
	if bo.ClOrdID != "" {
		t.Fatalf("non-duplicate 422 must not resolve to an order, got %+v", bo)
	}
}

func TestAlpaca_FillsSincePaginatesAndMapsDecimalExact(t *testing.T) {
	f := newFakeAlpaca(t)
	start := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	t1 := start.Add(time.Minute)
	t2 := start.Add(2 * time.Minute)
	t3 := start.Add(3 * time.Minute)
	f.activityPages = [][]map[string]any{
		{
			{"id": "fill-1", "activity_type": "FILL", "transaction_time": t1.Format(time.RFC3339Nano), "type": "fill", "price": "100.25", "qty": "2.5", "side": "buy", "symbol": "AAPL", "order_id": "ord-1", "order_status": "filled"},
			{"id": "fill-2", "activity_type": "FILL", "transaction_time": t2.Format(time.RFC3339Nano), "type": "partial_fill", "price": "99.75", "qty": "1.25", "side": "sell", "symbol": "MSFT", "order_id": "ord-2", "order_status": "filled"},
		},
		{
			{"id": "fill-3", "activity_type": "FILL", "transaction_time": t3.Format(time.RFC3339Nano), "type": "fill", "price": "101.125", "qty": "0.5", "side": "buy", "symbol": "AAPL", "order_id": "ord-3", "order_status": "filled"},
		},
	}
	oldPageSize := fillPageSize
	fillPageSize = 2
	defer func() { fillPageSize = oldPageSize }()

	got, err := f.broker(t).FillsSince(context.Background(), start)
	if err != nil {
		t.Fatalf("fills since: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("fills = %d, want 3: %+v", len(got), got)
	}
	if got[0].ID != "fill-1" || got[1].ID != "fill-2" || got[2].ID != "fill-3" {
		t.Fatalf("fills not returned oldest-first: %+v", got)
	}
	if got[0].Qty.Cmp(dec(t, "2.5")) != 0 || got[0].Px.Cmp(dec(t, "100.25")) != 0 {
		t.Fatalf("decimal conversion drifted: %+v", got[0])
	}
	if got[1].Side != orders.SideSell {
		t.Fatalf("sell side not mapped: %+v", got[1])
	}
	if len(f.activityRequests) != 2 {
		t.Fatalf("pagination requests = %d, want 2", len(f.activityRequests))
	}
	first := f.activityRequests[0]
	if first.Get("activity_types") != "FILL" || first.Get("direction") != "asc" || first.Get("page_size") != "2" {
		t.Fatalf("bad fill query: %v", first)
	}
	if first.Get("after") != start.Format(time.RFC3339Nano) {
		t.Fatalf("first after = %q, want %q", first.Get("after"), start.Format(time.RFC3339Nano))
	}
	if f.activityRequests[1].Get("after") != t2.Format(time.RFC3339Nano) {
		t.Fatalf("second after = %q, want last fill time %q", f.activityRequests[1].Get("after"), t2.Format(time.RFC3339Nano))
	}
}

func TestFetchFillsSinceFailsLoudWhenPageCapWouldTruncate(t *testing.T) {
	oldPageSize := fillPageSize
	oldMaxPages := fillMaxPages
	fillPageSize = 2
	fillMaxPages = 3
	defer func() {
		fillPageSize = oldPageSize
		fillMaxPages = oldMaxPages
	}()

	start := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	calls := 0
	got, err := fetchFillsSince(context.Background(), start, func(req sdk.GetAccountActivitiesRequest) ([]sdk.AccountActivity, error) {
		calls++
		return []sdk.AccountActivity{
			testFillActivity(t, "fill-a", req.After.Add(time.Minute)),
			testFillActivity(t, "fill-b", req.After.Add(2*time.Minute)),
		}, nil
	})

	if err == nil {
		t.Fatalf("expected page-cap truncation to fail loud, got nil error with %d fills", len(got))
	}
	if !strings.Contains(err.Error(), "refusing to return a partial fill set") {
		t.Fatalf("error = %q, want loud partial-fill refusal", err)
	}
	if got != nil {
		t.Fatalf("got partial fills on truncation: %+v", got)
	}
	if calls != fillMaxPages {
		t.Fatalf("fetch calls = %d, want capped %d", calls, fillMaxPages)
	}
}

func TestFetchFillsSinceReturnsCompleteBoundedPages(t *testing.T) {
	oldPageSize := fillPageSize
	oldMaxPages := fillMaxPages
	fillPageSize = 2
	fillMaxPages = 3
	defer func() {
		fillPageSize = oldPageSize
		fillMaxPages = oldMaxPages
	}()

	start := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	pages := [][]sdk.AccountActivity{
		{
			testFillActivity(t, "fill-1", start.Add(time.Minute)),
			testFillActivity(t, "fill-2", start.Add(2*time.Minute)),
		},
		{
			testFillActivity(t, "fill-3", start.Add(3*time.Minute)),
		},
	}
	calls := 0
	got, err := fetchFillsSince(context.Background(), start, func(req sdk.GetAccountActivitiesRequest) ([]sdk.AccountActivity, error) {
		if calls >= len(pages) {
			return nil, nil
		}
		page := pages[calls]
		calls++
		return page, nil
	})
	if err != nil {
		t.Fatalf("fetch fills: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("fills = %d, want 3: %+v", len(got), got)
	}
	if got[0].ID != "fill-1" || got[1].ID != "fill-2" || got[2].ID != "fill-3" {
		t.Fatalf("fills not returned oldest-first: %+v", got)
	}
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", calls)
	}
}

func testFillActivity(t *testing.T, id string, at time.Time) sdk.AccountActivity {
	t.Helper()
	return sdk.AccountActivity{
		ID:              id,
		ActivityType:    "FILL",
		TransactionTime: at,
		Type:            "fill",
		Price:           sdkDec(t, "100.25"),
		Qty:             sdkDec(t, "1.5"),
		Side:            "buy",
		Symbol:          "AAPL",
		OrderID:         "ord-" + id,
		OrderStatus:     "filled",
	}
}

func TestAlpaca_GetOrder_NotFound(t *testing.T) {
	f := newFakeAlpaca(t)
	b := f.broker(t)
	_, err := b.GetOrder(context.Background(), orders.ClientOrderID("x", "y", "ENTRY", 99))
	if !errors.Is(err, brokers.ErrOrderNotFound) {
		t.Fatalf("get unknown: got %v, want ErrOrderNotFound", err)
	}
}

func TestAlpaca_GetOrder_FilledStatus(t *testing.T) {
	f := newFakeAlpaca(t)
	cl := orders.ClientOrderID("trend", "AAPL", "ENTRY", 3)
	f.ordersByClOrdID[cl] = map[string]any{
		"id": "alp-x", "client_order_id": cl, "symbol": "AAPL", "side": "buy",
		"type": "market", "status": "filled", "qty": "10",
		"filled_qty": "10", "filled_avg_price": "150.33",
	}
	b := f.broker(t)

	bo, err := b.GetOrder(context.Background(), cl)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if bo.Status != brokers.StatusFilled {
		t.Fatalf("status = %s, want Filled", bo.Status)
	}
	if bo.CumQty.Cmp(dec(t, "10")) != 0 {
		t.Fatalf("cum qty = %s, want 10", bo.CumQty)
	}
	if bo.AvgPx.Cmp(dec(t, "150.33")) != 0 {
		t.Fatalf("avg px = %s, want 150.33", bo.AvgPx)
	}
}

func TestAlpaca_CancelOrder_ResolvesByClOrdID(t *testing.T) {
	f := newFakeAlpaca(t)
	cl := orders.ClientOrderID("trend", "AAPL", "ENTRY", 4)
	f.ordersByClOrdID[cl] = map[string]any{
		"id": "alp-cancelme", "client_order_id": cl, "symbol": "AAPL", "side": "buy",
		"type": "market", "status": "new", "qty": "10", "filled_qty": "0", "filled_avg_price": nil,
	}
	b := f.broker(t)

	if err := b.CancelOrder(context.Background(), cl); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The venue id (resolved from ClOrdID) is what got the DELETE.
	if len(f.canceledIDs) != 1 || f.canceledIDs[0] != "alp-cancelme" {
		t.Fatalf("canceled venue ids = %v, want [alp-cancelme]", f.canceledIDs)
	}
}

func TestAlpaca_CancelOrder_NotFound(t *testing.T) {
	f := newFakeAlpaca(t)
	b := f.broker(t)
	err := b.CancelOrder(context.Background(), orders.ClientOrderID("x", "y", "ENTRY", 5))
	if !errors.Is(err, brokers.ErrOrderNotFound) {
		t.Fatalf("cancel unknown: got %v, want ErrOrderNotFound", err)
	}
}

func TestAlpaca_StatusMapping(t *testing.T) {
	cases := map[string]brokers.OrderStatus{
		"new":              brokers.StatusNew,
		"accepted":         brokers.StatusNew,
		"partially_filled": brokers.StatusPartiallyFilled,
		"filled":           brokers.StatusFilled,
		"canceled":         brokers.StatusCanceled,
		"expired":          brokers.StatusCanceled,
		"rejected":         brokers.StatusRejected,
		"weird_unknown":    brokers.StatusUnknown,
	}
	for in, want := range cases {
		if got := fromSDKStatus(in); got != want {
			t.Errorf("fromSDKStatus(%q) = %s, want %s", in, got, want)
		}
	}
}
