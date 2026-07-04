// Package alpaca implements brokers.Broker against Alpaca's Trading API
// (paper + live). It wraps the official Go SDK
// (github.com/alpacahq/alpaca-trade-api-go/v3) for account / positions / orders,
// and uses a small in-package REST client for the latest-quote path so that
// price/qty values are parsed straight into orders.Decimal and NEVER pass
// through float64 (the SDK's market-data Quote type uses float64, which would
// introduce binary-float drift into money math — forbidden by BUCKS).
//
// All money is orders.Decimal. The deterministic ClOrdID is mapped to Alpaca's
// client_order_id (the idempotency key) verbatim, so a retried PlaceOrder dedupes
// at the venue. BaseURL/DataBaseURL are configurable so the entire adapter runs
// against an httptest.Server in tests — no live network in the default suite.
package alpaca

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

// httpTimeout bounds every Alpaca SDK HTTP call at the transport level. The SDK's
// methods do not accept a context, so this is what actually stops a hung venue read
// from freezing the single trade-loop goroutine (and with it the drawdown / kill-
// switch checks). A GetOrder that times out returns an error, which ENDS the fill-
// settle poll — so a degraded venue costs one timeout, not an unbounded stall.
const httpTimeout = 10 * time.Second

var (
	fillPageSize = 100
	fillMaxPages = 50
)

type fillActivityFetcher func(sdk.GetAccountActivitiesRequest) ([]sdk.AccountActivity, error)

// Config configures the adapter. KeyID/Secret are the Alpaca API credentials.
// BaseURL is the Trading API root (e.g. https://paper-api.alpaca.markets);
// DataBaseURL is the Market Data API root (e.g. https://data.alpaca.markets).
// In tests both point at an httptest.Server. Feed selects the data feed for
// quotes ("iex" is the free default; "sip" requires a paid subscription).
type Config struct {
	KeyID       string
	Secret      string
	BaseURL     string
	DataBaseURL string
	Feed        string
}

// AlpacaBroker is the Broker implementation backed by Alpaca.
type AlpacaBroker struct {
	trading *sdk.Client
	data    *dataClient
}

// New builds an AlpacaBroker from cfg. The Trading API base URL is required.
func New(cfg Config) (*AlpacaBroker, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("alpaca: BaseURL is required")
	}
	feed := cfg.Feed
	if feed == "" {
		feed = "iex"
	}
	trading := sdk.NewClient(sdk.ClientOpts{
		APIKey:     cfg.KeyID,
		APISecret:  cfg.Secret,
		BaseURL:    cfg.BaseURL,
		HTTPClient: &http.Client{Timeout: httpTimeout},
	})
	return &AlpacaBroker{
		trading: trading,
		data: newDataClient(dataConfig{
			keyID:   cfg.KeyID,
			secret:  cfg.Secret,
			baseURL: cfg.DataBaseURL,
			feed:    feed,
		}),
	}, nil
}

// Account implements brokers.Broker.
func (a *AlpacaBroker) Account(ctx context.Context) (brokers.Account, error) {
	if err := ctx.Err(); err != nil {
		return brokers.Account{}, err
	}
	acct, err := a.trading.GetAccount()
	if err != nil {
		return brokers.Account{}, fmt.Errorf("alpaca: get account: %w", err)
	}
	cash, err := fromSDKDecimal(acct.Cash)
	if err != nil {
		return brokers.Account{}, fmt.Errorf("alpaca: account cash: %w", err)
	}
	equity, err := fromSDKDecimal(acct.Equity)
	if err != nil {
		return brokers.Account{}, fmt.Errorf("alpaca: account equity: %w", err)
	}
	bp, err := fromSDKDecimal(acct.BuyingPower)
	if err != nil {
		return brokers.Account{}, fmt.Errorf("alpaca: account buying power: %w", err)
	}
	return brokers.Account{
		Cash:        cash,
		Equity:      equity,
		BuyingPower: bp,
	}, nil
}

// Positions implements brokers.Broker. Alpaca reports qty as a positive number
// plus a side string ("long"/"short"); we fold that into a signed qty.
func (a *AlpacaBroker) Positions(ctx context.Context) ([]brokers.Position, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := a.trading.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("alpaca: get positions: %w", err)
	}
	out := make([]brokers.Position, 0, len(raw))
	for _, p := range raw {
		qty, err := fromSDKDecimal(p.Qty)
		if err != nil {
			return nil, fmt.Errorf("alpaca: position qty for %s: %w", p.Symbol, err)
		}
		// Alpaca reports qty as a positive magnitude plus a side string; fold to a
		// signed inventory (- for short). If qty already carries the sign, respect
		// it (only negate a positive magnitude on the short side).
		if strings.EqualFold(p.Side, "short") && qty.IsPos() {
			qty = qty.Neg()
		}
		avg, err := fromSDKDecimal(p.AvgEntryPrice)
		if err != nil {
			return nil, fmt.Errorf("alpaca: position avg px for %s: %w", p.Symbol, err)
		}
		out = append(out, brokers.Position{
			Symbol:     p.Symbol,
			Qty:        qty,
			AvgEntryPx: avg,
		})
	}
	return out, nil
}

// Quote implements brokers.Broker via the in-package REST data client (decimal-
// exact, no float64).
func (a *AlpacaBroker) Quote(ctx context.Context, symbol string) (brokers.Quote, error) {
	return a.data.latestQuote(ctx, symbol)
}

// PlaceOrder implements brokers.Broker. The deterministic ClOrdID is sent as
// Alpaca's client_order_id (the idempotency key), so a retried place dedupes at
// the venue. Idempotency note: if Alpaca rejects a duplicate client_order_id
// (409/422), we resolve to the existing order via GetOrder so the caller still
// gets the original BrokerOrder — placing the same ClOrdID twice never creates a
// duplicate.
func (a *AlpacaBroker) PlaceOrder(ctx context.Context, req brokers.OrderRequest) (brokers.BrokerOrder, error) {
	if err := ctx.Err(); err != nil {
		return brokers.BrokerOrder{}, err
	}

	qty, err := toSDKDecimal(req.Qty)
	if err != nil {
		return brokers.BrokerOrder{}, fmt.Errorf("alpaca: place order qty: %w", err)
	}
	pr := sdk.PlaceOrderRequest{
		Symbol:        req.Symbol,
		Qty:           &qty,
		Side:          toSDKSide(req.Side),
		Type:          toSDKType(req.Kind),
		TimeInForce:   sdk.Day,
		ClientOrderID: req.ClOrdID,
	}
	if req.Kind == brokers.KindLimit {
		lp, lerr := toSDKDecimal(req.LimitPx)
		if lerr != nil {
			return brokers.BrokerOrder{}, fmt.Errorf("alpaca: place order limit px: %w", lerr)
		}
		pr.LimitPrice = &lp
	}

	ord, err := a.trading.PlaceOrder(pr)
	if err != nil {
		// Duplicate client_order_id => the order already exists at the venue.
		// Resolve to the original rather than surfacing an error (idempotency).
		if isDuplicateClientOrderID(err) {
			return a.GetOrder(ctx, req.ClOrdID)
		}
		return brokers.BrokerOrder{}, fmt.Errorf("alpaca: place order: %w", err)
	}
	return mapOrder(ord)
}

// CancelOrder implements brokers.Broker. Alpaca's cancel endpoint keys on the
// venue order id, so we resolve ClOrdID -> broker order id first.
func (a *AlpacaBroker) CancelOrder(ctx context.Context, clOrdID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ord, err := a.trading.GetOrderByClientOrderID(clOrdID)
	if err != nil {
		if isNotFound(err) {
			return brokers.ErrOrderNotFound
		}
		return fmt.Errorf("alpaca: resolve order for cancel %s: %w", clOrdID, err)
	}
	if err := a.trading.CancelOrder(ord.ID); err != nil {
		return fmt.Errorf("alpaca: cancel order %s: %w", clOrdID, err)
	}
	return nil
}

// GetOrder implements brokers.Broker, looking the order up by client_order_id.
func (a *AlpacaBroker) GetOrder(ctx context.Context, clOrdID string) (brokers.BrokerOrder, error) {
	if err := ctx.Err(); err != nil {
		return brokers.BrokerOrder{}, err
	}
	ord, err := a.trading.GetOrderByClientOrderID(clOrdID)
	if err != nil {
		if isNotFound(err) {
			return brokers.BrokerOrder{}, brokers.ErrOrderNotFound
		}
		return brokers.BrokerOrder{}, fmt.Errorf("alpaca: get order %s: %w", clOrdID, err)
	}
	return mapOrder(ord)
}

// FillsSince implements brokers.FillReader using Alpaca's account activity
// stream. The SDK returns only a page body (no next-token metadata), so pagination
// advances the After cursor to the last fill's transaction time and stops on a
// short/empty page, with fillMaxPages as a hard bound.
func (a *AlpacaBroker) FillsSince(ctx context.Context, after time.Time) ([]brokers.Fill, error) {
	return fetchFillsSince(ctx, after, a.trading.GetAccountActivities)
}

func fetchFillsSince(ctx context.Context, after time.Time, fetch fillActivityFetcher) ([]brokers.Fill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fillPageSize <= 0 {
		return nil, fmt.Errorf("alpaca: fill page size must be positive")
	}
	cursor := after.UTC()
	out := make([]brokers.Fill, 0)
	for page := 0; page < fillMaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		acts, err := fetch(sdk.GetAccountActivitiesRequest{
			ActivityTypes: []string{"FILL"},
			After:         cursor,
			Direction:     "asc",
			PageSize:      fillPageSize,
		})
		if err != nil {
			return nil, fmt.Errorf("alpaca: get fill activities: %w", err)
		}
		if len(acts) == 0 {
			return out, nil
		}
		for _, act := range acts {
			if !strings.EqualFold(act.ActivityType, "FILL") {
				continue
			}
			if !strings.EqualFold(act.Type, "fill") && !strings.EqualFold(act.Type, "partial_fill") {
				continue
			}
			qty, err := fromSDKDecimal(act.Qty)
			if err != nil {
				return nil, fmt.Errorf("alpaca: fill qty for %s: %w", act.ID, err)
			}
			px, err := fromSDKDecimal(act.Price)
			if err != nil {
				return nil, fmt.Errorf("alpaca: fill price for %s: %w", act.ID, err)
			}
			out = append(out, brokers.Fill{
				ID:      act.ID,
				Symbol:  act.Symbol,
				Side:    fromActivitySide(act.Side),
				Qty:     qty,
				Px:      px,
				At:      act.TransactionTime.UTC(),
				OrderID: act.OrderID,
				Status:  act.OrderStatus,
			})
		}
		last := acts[len(acts)-1].TransactionTime.UTC()
		if len(acts) < fillPageSize || !last.After(cursor) {
			return out, nil
		}
		cursor = last
	}
	return nil, fmt.Errorf("alpaca: fill activity backlog exceeds %d pages; refusing to return a partial fill set that would under-count realized P&L", fillMaxPages)
}

// compile-time assertion that AlpacaBroker satisfies the interface.
var _ brokers.Broker = (*AlpacaBroker)(nil)
var _ brokers.FillReader = (*AlpacaBroker)(nil)

// mapOrder converts an Alpaca SDK order into our typed BrokerOrder. The SDK's
// money fields are shopspring decimals; every one is converted into our
// govalues-backed orders.Decimal via exact decimal STRING (never float64), so no
// binary-float drift enters BUCKS. nil pointers (no qty / no fills) map to zero.
func mapOrder(o *sdk.Order) (brokers.BrokerOrder, error) {
	cum, err := fromSDKDecimal(o.FilledQty)
	if err != nil {
		return brokers.BrokerOrder{}, fmt.Errorf("alpaca: order filled qty: %w", err)
	}
	bo := brokers.BrokerOrder{
		BrokerOrderID: o.ID,
		ClOrdID:       o.ClientOrderID,
		Symbol:        o.Symbol,
		Side:          fromSDKSide(o.Side),
		Status:        fromSDKStatus(o.Status),
		OrderQty:      orders.ZeroDecimal,
		CumQty:        cum,
		AvgPx:         orders.ZeroDecimal,
	}
	if o.Qty != nil {
		oq, qerr := fromSDKDecimal(*o.Qty)
		if qerr != nil {
			return brokers.BrokerOrder{}, fmt.Errorf("alpaca: order qty: %w", qerr)
		}
		bo.OrderQty = oq
	}
	if o.FilledAvgPrice != nil {
		ap, aerr := fromSDKDecimal(*o.FilledAvgPrice)
		if aerr != nil {
			return brokers.BrokerOrder{}, fmt.Errorf("alpaca: order avg px: %w", aerr)
		}
		bo.AvgPx = ap
	}
	return bo, nil
}

// toSDKSide maps our side to Alpaca's.
func toSDKSide(s orders.Side) sdk.Side {
	if s == orders.SideSell {
		return sdk.Sell
	}
	return sdk.Buy
}

// fromSDKSide maps Alpaca's side back to ours (defaults to Buy on anything else).
func fromSDKSide(s sdk.Side) orders.Side {
	if s == sdk.Sell {
		return orders.SideSell
	}
	return orders.SideBuy
}

func fromActivitySide(s string) orders.Side {
	if strings.EqualFold(s, "sell") {
		return orders.SideSell
	}
	return orders.SideBuy
}

// toSDKType maps our order kind to Alpaca's order type.
func toSDKType(k brokers.OrderKind) sdk.OrderType {
	if k == brokers.KindLimit {
		return sdk.Limit
	}
	return sdk.Market
}

// fromSDKStatus maps Alpaca's order status string to our enum. Alpaca's status
// vocabulary is mapped to the small terminal/working set BUCKS reasons about.
// Anything unrecognized maps to StatusUnknown so reconcile never silently treats
// an unknown state as done.
func fromSDKStatus(s string) brokers.OrderStatus {
	switch strings.ToLower(s) {
	case "new", "accepted", "pending_new", "accepted_for_bidding",
		"held", "pending_replace", "replaced", "calculated":
		return brokers.StatusNew
	case "partially_filled":
		return brokers.StatusPartiallyFilled
	case "filled":
		return brokers.StatusFilled
	case "canceled", "cancelled", "pending_cancel", "expired",
		"done_for_day", "stopped", "suspended":
		return brokers.StatusCanceled
	case "rejected":
		return brokers.StatusRejected
	default:
		return brokers.StatusUnknown
	}
}

// isNotFound reports whether an SDK error indicates the order does not exist.
func isNotFound(err error) bool {
	var apiErr *sdk.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 404
	}
	return false
}

// isDuplicateClientOrderID reports whether an SDK error indicates the
// client_order_id has already been used (Alpaca returns 409/422). Used to make
// PlaceOrder idempotent on retries.
func isDuplicateClientOrderID(err error) bool {
	var apiErr *sdk.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 409 {
			return true
		}
		if apiErr.StatusCode == 422 &&
			strings.Contains(strings.ToLower(apiErr.Message), "client_order_id") {
			return true
		}
	}
	return false
}
