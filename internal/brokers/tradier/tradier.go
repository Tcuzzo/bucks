// Package tradier implements brokers.Broker against Tradier's brokerage API
// (equities + options, full sandbox/paper). Auth is an OAuth2 Bearer token. All
// money is orders.Decimal, parsed from the API's decimal strings (Tradier returns
// JSON numbers; we decode via json.Number and parse the exact text — never
// float64). The deterministic ClOrdID maps to Tradier's `tag` field (its client
// reference), used for idempotent place/lookup where supported.
//
// BaseURL + AccountID are configurable so the whole adapter runs against an
// httptest.Server in the default suite — no live network. A real, token-signed
// live test lives behind the `tradier_live` build tag and is excluded by default.
package tradier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

// Config configures the adapter. AccessToken is the OAuth2 bearer; BaseURL is the
// API root (sandbox: https://sandbox.tradier.com, in tests an httptest.Server);
// AccountID is the Tradier account number the calls operate on. Now is injectable
// for deterministic quote timestamps in tests.
type Config struct {
	AccessToken string
	BaseURL     string
	AccountID   string
	Now         func() time.Time
}

// TradierBroker is the Broker implementation backed by Tradier.
type TradierBroker struct {
	cfg     Config
	http    *http.Client
	baseURL string
	now     func() time.Time
}

// New builds a TradierBroker. BaseURL and AccountID are required.
func New(cfg Config, hc *http.Client) (*TradierBroker, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("tradier: BaseURL is required")
	}
	if cfg.AccountID == "" {
		return nil, fmt.Errorf("tradier: AccountID is required")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &TradierBroker{
		cfg:     cfg,
		http:    hc,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		now:     now,
	}, nil
}

// do issues a request with the OAuth2 bearer + JSON Accept header. For POST it
// sends form-encoded data (Tradier's order/cancel endpoints take form bodies).
// It decodes the JSON response into out and returns the status. Non-2xx returns
// an error carrying the body (401 distinct so callers/tests branch on auth).
func (t *TradierBroker) do(ctx context.Context, method, path string, form url.Values, out any) (int, error) {
	var body io.Reader
	if method == http.MethodPost && form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, body)
	if err != nil {
		return 0, fmt.Errorf("tradier: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+t.cfg.AccessToken)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost && form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("tradier: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("tradier: %s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("tradier: decode %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// --- wire shapes (subset of Tradier; money via json.Number, never float64) ---

type tdBalances struct {
	Balances struct {
		TotalCash   json.Number `json:"total_cash"`
		TotalEquity json.Number `json:"total_equity"`
		Margin      struct {
			StockBuyingPower json.Number `json:"stock_buying_power"`
		} `json:"margin"`
		Cash struct {
			CashAvailable json.Number `json:"cash_available"`
		} `json:"cash"`
	} `json:"balances"`
}

type tdQuoteResp struct {
	Quotes struct {
		Quote tdQuote `json:"quote"`
	} `json:"quotes"`
}

type tdQuote struct {
	Symbol string      `json:"symbol"`
	Bid    json.Number `json:"bid"`
	Ask    json.Number `json:"ask"`
	Last   json.Number `json:"last"`
}

type tdOrderResp struct {
	Order tdOrder `json:"order"`
}

type tdOrder struct {
	ID        json.Number `json:"id"`
	Symbol    string      `json:"symbol"`
	Side      string      `json:"side"`
	Quantity  json.Number `json:"quantity"`
	Status    string      `json:"status"`
	ExecQty   json.Number `json:"exec_quantity"`
	AvgFillPx json.Number `json:"avg_fill_price"`
	Tag       string      `json:"tag"`
}

// Account implements brokers.Broker. Tradier reports cash + equity + (margin)
// buying power; we prefer total_cash for Cash and stock_buying_power (falling back
// to cash_available) for BuyingPower.
func (t *TradierBroker) Account(ctx context.Context) (brokers.Account, error) {
	var resp tdBalances
	path := "/v1/accounts/" + t.cfg.AccountID + "/balances"
	if _, err := t.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return brokers.Account{}, err
	}
	cash, err := numDec(resp.Balances.TotalCash)
	if err != nil {
		return brokers.Account{}, fmt.Errorf("tradier: total_cash: %w", err)
	}
	equity, err := numDec(resp.Balances.TotalEquity)
	if err != nil {
		return brokers.Account{}, fmt.Errorf("tradier: total_equity: %w", err)
	}
	bp, err := numDec(resp.Balances.Margin.StockBuyingPower)
	if err != nil {
		return brokers.Account{}, fmt.Errorf("tradier: stock_buying_power: %w", err)
	}
	if bp.IsZero() {
		if cb, cerr := numDec(resp.Balances.Cash.CashAvailable); cerr == nil && !cb.IsZero() {
			bp = cb
		}
	}
	return brokers.Account{Cash: cash, Equity: equity, BuyingPower: bp}, nil
}

// Positions implements brokers.Broker.
func (t *TradierBroker) Positions(ctx context.Context) ([]brokers.Position, error) {
	var resp struct {
		Positions struct {
			// Tradier returns either a single object or an array under "position";
			// we decode into a flexible slice via json.RawMessage handling below.
			Position json.RawMessage `json:"position"`
		} `json:"positions"`
	}
	path := "/v1/accounts/" + t.cfg.AccountID + "/positions"
	if _, err := t.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	raw := resp.Positions.Position
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `"null"` {
		return nil, nil
	}
	type tdPos struct {
		Symbol    string      `json:"symbol"`
		Quantity  json.Number `json:"quantity"`
		CostBasis json.Number `json:"cost_basis"`
	}
	var list []tdPos
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("tradier: positions array: %w", err)
		}
	} else {
		var one tdPos
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("tradier: position object: %w", err)
		}
		list = []tdPos{one}
	}
	out := make([]brokers.Position, 0, len(list))
	for _, p := range list {
		qty, err := numDec(p.Quantity)
		if err != nil {
			return nil, fmt.Errorf("tradier: position qty for %s: %w", p.Symbol, err)
		}
		// Avg entry = cost_basis / quantity when quantity is non-zero.
		avg := orders.ZeroDecimal
		cost, cerr := numDec(p.CostBasis)
		if cerr == nil && !qty.IsZero() {
			if a, derr := cost.Quo(qty); derr == nil {
				avg = a.Abs()
			}
		}
		out = append(out, brokers.Position{Symbol: p.Symbol, Qty: qty, AvgEntryPx: avg})
	}
	return out, nil
}

// Quote implements brokers.Broker via the market-data quotes endpoint.
func (t *TradierBroker) Quote(ctx context.Context, symbol string) (brokers.Quote, error) {
	var resp tdQuoteResp
	path := "/v1/markets/quotes?symbols=" + url.QueryEscape(symbol)
	if _, err := t.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return brokers.Quote{}, err
	}
	q := resp.Quotes.Quote
	bid, err := numDec(q.Bid)
	if err != nil {
		return brokers.Quote{}, fmt.Errorf("tradier: quote bid: %w", err)
	}
	ask, err := numDec(q.Ask)
	if err != nil {
		return brokers.Quote{}, fmt.Errorf("tradier: quote ask: %w", err)
	}
	last, err := numDec(q.Last)
	if err != nil {
		return brokers.Quote{}, fmt.Errorf("tradier: quote last: %w", err)
	}
	return brokers.Quote{Symbol: symbol, Bid: bid, Ask: ask, Last: last, Time: t.now()}, nil
}

// PlaceOrder implements brokers.Broker. ClOrdID → Tradier's `tag` (client ref),
// used for idempotent lookup. Tradier order placement is a form POST.
func (t *TradierBroker) PlaceOrder(ctx context.Context, req brokers.OrderRequest) (brokers.BrokerOrder, error) {
	form := url.Values{}
	form.Set("class", "equity")
	form.Set("symbol", req.Symbol)
	form.Set("side", toTDSide(req.Side))
	form.Set("quantity", req.Qty.String())
	form.Set("tag", req.ClOrdID)
	if req.Kind == brokers.KindLimit {
		form.Set("type", "limit")
		form.Set("price", req.LimitPx.String())
	} else {
		form.Set("type", "market")
	}
	form.Set("duration", "day")

	var resp tdOrderResp
	path := "/v1/accounts/" + t.cfg.AccountID + "/orders"
	if _, err := t.do(ctx, http.MethodPost, path, form, &resp); err != nil {
		return brokers.BrokerOrder{}, err
	}
	bo, err := mapTDOrder(resp.Order)
	if err != nil {
		return brokers.BrokerOrder{}, err
	}
	// The place response may not echo symbol/side/tag; backfill from the request.
	if bo.ClOrdID == "" {
		bo.ClOrdID = req.ClOrdID
	}
	if bo.Symbol == "" {
		bo.Symbol = req.Symbol
	}
	if bo.OrderQty.IsZero() {
		bo.OrderQty = req.Qty
	}
	return bo, nil
}

// CancelOrder implements brokers.Broker (DELETE on the order, resolved by tag).
func (t *TradierBroker) CancelOrder(ctx context.Context, clOrdID string) error {
	bo, err := t.GetOrder(ctx, clOrdID)
	if err != nil {
		return err
	}
	path := "/v1/accounts/" + t.cfg.AccountID + "/orders/" + bo.BrokerOrderID
	// Tradier cancel is DELETE; we route it through a form-less request.
	if _, err := t.do(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return nil
}

// GetOrder implements brokers.Broker, looking the order up by our tag (ClOrdID).
// Tradier's orders list is filtered client-side by tag.
func (t *TradierBroker) GetOrder(ctx context.Context, clOrdID string) (brokers.BrokerOrder, error) {
	var resp struct {
		Orders struct {
			Order json.RawMessage `json:"order"`
		} `json:"orders"`
	}
	path := "/v1/accounts/" + t.cfg.AccountID + "/orders"
	if _, err := t.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return brokers.BrokerOrder{}, err
	}
	raw := resp.Orders.Order
	if len(raw) == 0 || string(raw) == "null" {
		return brokers.BrokerOrder{}, brokers.ErrOrderNotFound
	}
	var list []tdOrder
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return brokers.BrokerOrder{}, fmt.Errorf("tradier: orders array: %w", err)
		}
	} else {
		var one tdOrder
		if err := json.Unmarshal(raw, &one); err != nil {
			return brokers.BrokerOrder{}, fmt.Errorf("tradier: order object: %w", err)
		}
		list = []tdOrder{one}
	}
	for _, o := range list {
		if o.Tag == clOrdID {
			return mapTDOrder(o)
		}
	}
	return brokers.BrokerOrder{}, brokers.ErrOrderNotFound
}

// compile-time assertion that TradierBroker satisfies the interface.
var _ brokers.Broker = (*TradierBroker)(nil)
