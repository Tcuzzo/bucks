// Package coinbase implements brokers.Broker against Coinbase Advanced Trade
// (crypto). It models Coinbase's defining auth quirk: a JWT bearer token with a
// SHORT (2-minute) expiry that the adapter AUTO-REFRESHES before each call so a
// long-lived process never sends an expired token. All money is orders.Decimal,
// parsed from the API's decimal strings (never float64). The deterministic
// ClOrdID maps to Coinbase's client_order_id (the idempotency key), so a retried
// PlaceOrder dedupes at the venue.
//
// BaseURL is configurable so the whole adapter runs against an httptest.Server in
// the default test suite — no live network. A real, credential-signed live test
// lives behind the `coinbase_live` build tag and is excluded by default.
package coinbase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

// Config configures the adapter. APIKeyName/APISecret are the CDP credentials the
// token minter signs with. BaseURL is the Advanced Trade REST root (in tests, an
// httptest.Server). TokenTTL is the JWT lifetime (Coinbase uses 2 minutes); a
// zero value defaults to 2 minutes. Now/Mint are injectable for deterministic
// tests — production leaves them nil to use the real clock and the default
// minter.
type Config struct {
	APIKeyName string
	APISecret  string
	BaseURL    string
	TokenTTL   time.Duration

	// Now is the clock the refresher reads. nil => time.Now. Injected in tests to
	// drive expiry deterministically.
	Now func() time.Time
	// Mint produces a fresh bearer token valid until the returned expiry. nil =>
	// defaultMint (a stand-in that stamps the issue time; the real minter signs an
	// ES256 JWT). Injected in tests to count refreshes and assert timing.
	Mint func(cfg Config, issuedAt time.Time) (token string, expiresAt time.Time)
}

// CoinbaseBroker is the Broker implementation backed by Coinbase Advanced Trade.
type CoinbaseBroker struct {
	cfg     Config
	http    *http.Client
	baseURL string
	now     func() time.Time
	mint    func(cfg Config, issuedAt time.Time) (string, time.Time)
	ttl     time.Duration

	// token cache (guarded by the single-goroutine call pattern of the adapter;
	// the auth() method refreshes lazily on each request).
	token     string
	expiresAt time.Time
}

// New builds a CoinbaseBroker from cfg. BaseURL is required.
func New(cfg Config, hc *http.Client) (*CoinbaseBroker, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("coinbase: BaseURL is required")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	mint := cfg.Mint
	if mint == nil {
		mint = defaultMint
	}
	ttl := cfg.TokenTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &CoinbaseBroker{
		cfg:     cfg,
		http:    hc,
		now:     now,
		mint:    mint,
		ttl:     ttl,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
	}, nil
}

// bearer returns the current JWT, auto-refreshing it when missing or near expiry.
// This is Coinbase's defining quirk: a 2-minute token that a long-lived trading
// process must keep fresh on its own. We refresh a little EARLY (skew) so a token
// can't expire mid-flight.
func (c *CoinbaseBroker) bearer() string {
	// refreshIfNeeded mints a new token when the current one is missing or within
	// the refresh skew of expiry. Coinbase's 2-minute window is short, so we
	// refresh a little EARLY (skew) to avoid sending a token that expires mid-flight.
	const skew = 10 * time.Second
	now := c.now()
	if c.token == "" || !now.Add(skew).Before(c.expiresAt) {
		tok, exp := c.mint(c.cfg, now)
		c.token = tok
		c.expiresAt = exp
	}
	return c.token
}

// auth attaches the current (auto-refreshed) bearer token to a request.
func (c *CoinbaseBroker) auth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.bearer())
}

// defaultMint is a non-signing stand-in used when Config.Mint is nil. It returns
// an opaque token tied to the issue time and an expiry TTL ahead. The REAL minter
// (a later wiring slice) signs an ES256 JWT with the CDP secret; the adapter's
// refresh logic is identical either way, which is what these tests prove.
func defaultMint(cfg Config, issuedAt time.Time) (string, time.Time) {
	ttl := cfg.TokenTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	tok := fmt.Sprintf("cb-jwt.%s.%d", cfg.APIKeyName, issuedAt.UnixNano())
	return tok, issuedAt.Add(ttl)
}

// doJSON performs an authenticated request, attaching the auto-refreshed bearer.
// method is GET or POST; body (for POST) is JSON-encoded. It decodes the response
// into out (if non-nil) and returns the status code. Non-2xx returns an error
// carrying the body for diagnosis (401 distinct so callers/tests can branch).
func (c *CoinbaseBroker) doJSON(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("coinbase: marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, fmt.Errorf("coinbase: build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.auth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("coinbase: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("coinbase: %s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("coinbase: decode %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// --- wire shapes (subset of Coinbase Advanced Trade) ---

type cbAccount struct {
	Accounts []struct {
		Currency         string `json:"currency"`
		AvailableBalance struct {
			Value string `json:"value"`
		} `json:"available_balance"`
	} `json:"accounts"`
}

type cbProductQuote struct {
	ProductID string `json:"product_id"`
	Price     string `json:"price"`
	Bid       string `json:"best_bid"`
	Ask       string `json:"best_ask"`
}

type cbOrderResp struct {
	OrderID       string `json:"order_id"`
	ClientOrderID string `json:"client_order_id"`
	ProductID     string `json:"product_id"`
	Side          string `json:"side"`
	Status        string `json:"status"`
	FilledSize    string `json:"filled_size"`
	AvgFillPrice  string `json:"average_filled_price"`
	OrderSize     string `json:"order_size"`
}

// Account implements brokers.Broker. Coinbase reports per-currency balances; we
// fold the USD/cash currency into Cash and surface it as Equity/BuyingPower too
// (a richer equity calc — marking crypto to market — is a later slice).
func (c *CoinbaseBroker) Account(ctx context.Context) (brokers.Account, error) {
	var resp cbAccount
	if _, err := c.doJSON(ctx, http.MethodGet, "/api/v3/brokerage/accounts", nil, &resp); err != nil {
		return brokers.Account{}, err
	}
	cash := orders.ZeroDecimal
	for _, a := range resp.Accounts {
		if strings.EqualFold(a.Currency, "USD") || strings.EqualFold(a.Currency, "USDC") {
			v, err := parseDec(a.AvailableBalance.Value)
			if err != nil {
				return brokers.Account{}, fmt.Errorf("coinbase: account balance %q: %w", a.AvailableBalance.Value, err)
			}
			cash = v
			break
		}
	}
	return brokers.Account{Cash: cash, Equity: cash, BuyingPower: cash}, nil
}

// Positions implements brokers.Broker. Each non-fiat currency with a non-zero
// balance is a long crypto position (avg-entry is not part of the balances feed,
// so it is zero here; a fills-based cost-basis is a later slice).
func (c *CoinbaseBroker) Positions(ctx context.Context) ([]brokers.Position, error) {
	var resp cbAccount
	if _, err := c.doJSON(ctx, http.MethodGet, "/api/v3/brokerage/accounts", nil, &resp); err != nil {
		return nil, err
	}
	var out []brokers.Position
	for _, a := range resp.Accounts {
		if strings.EqualFold(a.Currency, "USD") || strings.EqualFold(a.Currency, "USDC") {
			continue
		}
		qty, err := parseDec(a.AvailableBalance.Value)
		if err != nil {
			return nil, fmt.Errorf("coinbase: position qty %q: %w", a.AvailableBalance.Value, err)
		}
		if qty.IsZero() {
			continue
		}
		out = append(out, brokers.Position{Symbol: a.Currency, Qty: qty, AvgEntryPx: orders.ZeroDecimal})
	}
	return out, nil
}

// Quote implements brokers.Broker via the product ticker endpoint.
func (c *CoinbaseBroker) Quote(ctx context.Context, symbol string) (brokers.Quote, error) {
	var resp cbProductQuote
	if _, err := c.doJSON(ctx, http.MethodGet, "/api/v3/brokerage/products/"+symbol, nil, &resp); err != nil {
		return brokers.Quote{}, err
	}
	bid, err := parseDec(resp.Bid)
	if err != nil {
		return brokers.Quote{}, fmt.Errorf("coinbase: quote bid %q: %w", resp.Bid, err)
	}
	ask, err := parseDec(resp.Ask)
	if err != nil {
		return brokers.Quote{}, fmt.Errorf("coinbase: quote ask %q: %w", resp.Ask, err)
	}
	last, err := parseDec(resp.Price)
	if err != nil {
		return brokers.Quote{}, fmt.Errorf("coinbase: quote price %q: %w", resp.Price, err)
	}
	return brokers.Quote{Symbol: symbol, Bid: bid, Ask: ask, Last: last, Time: c.now()}, nil
}

// cbPlaceReq is the create-order request body. ClientOrderID is the idempotency
// key (our ClOrdID verbatim), so a retried place dedupes at the venue.
type cbPlaceReq struct {
	ClientOrderID      string        `json:"client_order_id"`
	ProductID          string        `json:"product_id"`
	Side               string        `json:"side"`
	OrderConfiguration cbOrderConfig `json:"order_configuration"`
}

type cbOrderConfig struct {
	MarketIOC *cbMarketIOC `json:"market_market_ioc,omitempty"`
	LimitGTC  *cbLimitGTC  `json:"limit_limit_gtc,omitempty"`
}

type cbMarketIOC struct {
	BaseSize string `json:"base_size"`
}

type cbLimitGTC struct {
	BaseSize   string `json:"base_size"`
	LimitPrice string `json:"limit_price"`
}

// PlaceOrder implements brokers.Broker. ClOrdID → client_order_id (idempotency).
func (c *CoinbaseBroker) PlaceOrder(ctx context.Context, req brokers.OrderRequest) (brokers.BrokerOrder, error) {
	body := cbPlaceReq{
		ClientOrderID: req.ClOrdID,
		ProductID:     req.Symbol,
		Side:          toCBSide(req.Side),
	}
	if req.Kind == brokers.KindLimit {
		body.OrderConfiguration.LimitGTC = &cbLimitGTC{
			BaseSize:   req.Qty.String(),
			LimitPrice: req.LimitPx.String(),
		}
	} else {
		body.OrderConfiguration.MarketIOC = &cbMarketIOC{BaseSize: req.Qty.String()}
	}
	var resp cbOrderResp
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v3/brokerage/orders", body, &resp); err != nil {
		return brokers.BrokerOrder{}, err
	}
	return mapCBOrder(resp)
}

// CancelOrder implements brokers.Broker via the batch-cancel endpoint, keyed on
// the venue order id resolved from our ClOrdID.
func (c *CoinbaseBroker) CancelOrder(ctx context.Context, clOrdID string) error {
	bo, err := c.GetOrder(ctx, clOrdID)
	if err != nil {
		return err
	}
	body := map[string][]string{"order_ids": {bo.BrokerOrderID}}
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v3/brokerage/orders/batch_cancel", body, nil); err != nil {
		return err
	}
	return nil
}

// GetOrder implements brokers.Broker by looking up the order by client_order_id.
func (c *CoinbaseBroker) GetOrder(ctx context.Context, clOrdID string) (brokers.BrokerOrder, error) {
	var resp struct {
		Order cbOrderResp `json:"order"`
	}
	status, err := c.doJSON(ctx, http.MethodGet, "/api/v3/brokerage/orders/historical/"+clOrdID, nil, &resp)
	if err != nil {
		if status == http.StatusNotFound {
			return brokers.BrokerOrder{}, brokers.ErrOrderNotFound
		}
		return brokers.BrokerOrder{}, err
	}
	if resp.Order.OrderID == "" && resp.Order.ClientOrderID == "" {
		return brokers.BrokerOrder{}, brokers.ErrOrderNotFound
	}
	return mapCBOrder(resp.Order)
}

// compile-time assertion that CoinbaseBroker satisfies the interface.
var _ brokers.Broker = (*CoinbaseBroker)(nil)
