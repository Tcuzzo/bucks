package alpaca

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

// dataConfig configures the thin market-data REST client.
type dataConfig struct {
	keyID   string
	secret  string
	baseURL string // e.g. https://data.alpaca.markets (or the test server URL)
	feed    string // "iex" (free) or "sip" (paid)
}

// dataClient is a minimal Alpaca Market Data REST client for the latest-quote
// path. It deliberately does NOT use the SDK's market-data client, whose Quote
// type stores prices as float64 — BUCKS parses prices straight into
// orders.Decimal so no money value ever round-trips through a binary float.
type dataClient struct {
	cfg  dataConfig
	http *http.Client
}

func newDataClient(cfg dataConfig) *dataClient {
	return &dataClient{
		cfg:  cfg,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// latestQuoteResponse mirrors Alpaca's GET /v2/stocks/quotes/latest envelope.
// Prices are decoded as json.Number to preserve exact decimal text, never float.
type latestQuoteResponse struct {
	Quotes map[string]struct {
		BidPrice  json.Number `json:"bp"`
		AskPrice  json.Number `json:"ap"`
		Timestamp time.Time   `json:"t"`
	} `json:"quotes"`
}

// latestTradeResponse mirrors Alpaca's GET /v2/stocks/trades/latest envelope.
type latestTradeResponse struct {
	Trades map[string]struct {
		Price     json.Number `json:"p"`
		Timestamp time.Time   `json:"t"`
	} `json:"trades"`
}

// latestQuote fetches the top-of-book quote for symbol plus the latest trade
// (for the "last" price), mapping every price into orders.Decimal exactly.
func (d *dataClient) latestQuote(ctx context.Context, symbol string) (brokers.Quote, error) {
	if d.cfg.baseURL == "" {
		return brokers.Quote{}, fmt.Errorf("alpaca data: DataBaseURL is required")
	}

	var qresp latestQuoteResponse
	if err := d.get(ctx, "/v2/stocks/quotes/latest", symbol, &qresp); err != nil {
		return brokers.Quote{}, err
	}
	q, ok := qresp.Quotes[symbol]
	if !ok {
		return brokers.Quote{}, fmt.Errorf("alpaca data: no quote for %q", symbol)
	}
	bid, err := numToDecimal(q.BidPrice)
	if err != nil {
		return brokers.Quote{}, fmt.Errorf("alpaca data: bid for %s: %w", symbol, err)
	}
	ask, err := numToDecimal(q.AskPrice)
	if err != nil {
		return brokers.Quote{}, fmt.Errorf("alpaca data: ask for %s: %w", symbol, err)
	}

	// Last trade price (best-effort; an empty trades map leaves Last at zero).
	last := orders.ZeroDecimal
	var tresp latestTradeResponse
	if err := d.get(ctx, "/v2/stocks/trades/latest", symbol, &tresp); err != nil {
		return brokers.Quote{}, err
	}
	if t, ok := tresp.Trades[symbol]; ok {
		last, err = numToDecimal(t.Price)
		if err != nil {
			return brokers.Quote{}, fmt.Errorf("alpaca data: last for %s: %w", symbol, err)
		}
	}

	return brokers.Quote{
		Symbol: symbol,
		Bid:    bid,
		Ask:    ask,
		Last:   last,
		Time:   q.Timestamp,
	}, nil
}

// get performs an authenticated GET against path?symbols=symbol&feed=... and
// decodes the JSON body into out. A non-2xx response is surfaced as an error.
func (d *dataClient) get(ctx context.Context, path, symbol string, out any) error {
	u, err := url.Parse(d.cfg.baseURL + path)
	if err != nil {
		return fmt.Errorf("alpaca data: parse url: %w", err)
	}
	q := u.Query()
	q.Set("symbols", symbol)
	if d.cfg.feed != "" {
		q.Set("feed", d.cfg.feed)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("alpaca data: build request: %w", err)
	}
	req.Header.Set("APCA-API-KEY-ID", d.cfg.keyID)
	req.Header.Set("APCA-API-SECRET-KEY", d.cfg.secret)
	req.Header.Set("Accept", "application/json")

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("alpaca data: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("alpaca data: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("alpaca data: %s -> %d: %s", path, resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("alpaca data: decode %s: %w", path, err)
	}
	return nil
}

// numToDecimal converts a JSON number (exact decimal text) into orders.Decimal.
// An empty number is treated as zero (a missing optional price field).
func numToDecimal(n json.Number) (orders.Decimal, error) {
	s := n.String()
	if s == "" {
		return orders.ZeroDecimal, nil
	}
	return orders.ParseDecimal(s)
}
