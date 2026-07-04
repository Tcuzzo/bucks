// Package brokers defines BUCKS's typed broker boundary: a single Broker
// interface every venue adapter satisfies, the request/response value types that
// cross it (all money in orders.Decimal — never float64), and the
// reconcile-on-boot logic that makes the broker the source of truth for fills
// and positions while the WAL/journal stays the source of truth for intent.
//
// Two adapters live under this package:
//
//   - mock    — an in-memory, deterministic Broker for tests and the rest of the
//     build. PlaceOrder is idempotent on ClOrdID.
//   - alpaca  — a real adapter over Alpaca's Trading API (paper + live).
//
// The whole design point of the interface is that strategy/risk code depends on
// Broker, not on any single venue, so a new venue drops in without touching the
// callers.
package brokers

import (
	"context"
	"errors"
	"time"

	"bucks/internal/orders"
)

// OrderStatus is the venue-agnostic lifecycle position of a broker order as the
// broker reports it. It is deliberately small: BUCKS only needs to know whether
// an order is still working, terminally done (and how), or unknown to the broker.
type OrderStatus int

const (
	// StatusUnknown is the zero value: the broker has no record of this order, or
	// it could not be classified. Reconcile treats unknown distinctly from
	// canceled/rejected so it never silently drops a working order.
	StatusUnknown OrderStatus = iota
	// StatusNew is accepted/working at the broker with no fills yet.
	StatusNew
	// StatusPartiallyFilled has at least one fill but is not complete.
	StatusPartiallyFilled
	// StatusFilled is fully filled. Terminal.
	StatusFilled
	// StatusCanceled was canceled before fully filling. Terminal.
	StatusCanceled
	// StatusRejected was rejected by the venue/risk layer. Terminal.
	StatusRejected
)

// String renders the status for logs, reconcile reports, and tests.
func (s OrderStatus) String() string {
	switch s {
	case StatusNew:
		return "New"
	case StatusPartiallyFilled:
		return "PartiallyFilled"
	case StatusFilled:
		return "Filled"
	case StatusCanceled:
		return "Canceled"
	case StatusRejected:
		return "Rejected"
	default:
		return "Unknown"
	}
}

// IsTerminal reports whether the broker considers the order finished (no further
// fills possible). StatusUnknown is NOT terminal — the order's fate is undecided.
func (s OrderStatus) IsTerminal() bool {
	switch s {
	case StatusFilled, StatusCanceled, StatusRejected:
		return true
	default:
		return false
	}
}

// OrderKind is market vs limit. BUCKS v1 places these two types; stops/brackets
// are a later slice and intentionally excluded from the boundary for now.
type OrderKind int

const (
	// KindMarket is a market order (no limit price).
	KindMarket OrderKind = iota
	// KindLimit is a limit order (LimitPx must be set).
	KindLimit
)

// String renders the order kind.
func (k OrderKind) String() string {
	if k == KindLimit {
		return "Limit"
	}
	return "Market"
}

// Account is the venue-agnostic cash/equity snapshot. All money is exact decimal.
type Account struct {
	// Cash is settled cash.
	Cash orders.Decimal
	// Equity is total account value (cash + positions market value).
	Equity orders.Decimal
	// BuyingPower is the amount available to open new positions (may exceed cash
	// on a margin account).
	BuyingPower orders.Decimal
}

// Position is one open position. Qty is SIGNED: positive = long, negative =
// short. AvgEntryPx is the average entry price (always non-negative).
type Position struct {
	Symbol     string
	Qty        orders.Decimal // signed: + long / - short
	AvgEntryPx orders.Decimal
}

// Quote is a top-of-book snapshot. Bid/Ask/Last are exact decimal; Time is the
// venue timestamp of the quote.
type Quote struct {
	Symbol string
	Bid    orders.Decimal
	Ask    orders.Decimal
	Last   orders.Decimal
	Time   time.Time
}

// OrderRequest is an intent to place an order. ClOrdID is the deterministic
// client-order-ID from orders.ClientOrderID — it is the IDEMPOTENCY KEY: an
// adapter MUST send the SAME ClOrdID verbatim on every retry so the broker
// dedupes a duplicate send (and so PlaceOrder is safe to call again after a
// crash between intent and ack). Limit orders MUST set LimitPx; for market
// orders LimitPx is ignored.
type OrderRequest struct {
	ClOrdID string // idempotency key — reused verbatim on retries
	Symbol  string
	Side    orders.Side
	Qty     orders.Decimal
	Kind    OrderKind
	LimitPx orders.Decimal // required for KindLimit; ignored for KindMarket
}

// BrokerOrder is the broker's view of an order after place/get. BrokerOrderID is
// the venue's own id (distinct from ClOrdID); CumQty/AvgPx report fills so far so
// reconcile can resolve an in-flight intent against the broker's truth.
type BrokerOrder struct {
	BrokerOrderID string
	ClOrdID       string
	Symbol        string
	Side          orders.Side
	Status        OrderStatus
	OrderQty      orders.Decimal
	CumQty        orders.Decimal // filled quantity so far
	AvgPx         orders.Decimal // average fill price (zero if no fills)
}

// Fill is one authoritative venue fill from the broker's account-activity stream.
// ID is the durable venue activity id and is the de-duplication key for realized
// P&L accounting. Qty and Px are exact decimals; never float64.
type Fill struct {
	ID      string
	Symbol  string
	Side    orders.Side
	Qty     orders.Decimal
	Px      orders.Decimal
	At      time.Time
	OrderID string
	Status  string
}

// FillReader is an OPTIONAL broker capability for venues that can expose their
// authoritative fill stream. The core Broker interface stays minimal; the live
// ledger reconciler depends only on this narrow seam.
type FillReader interface {
	FillsSince(ctx context.Context, after time.Time) ([]Fill, error)
}

// ErrOrderNotFound is returned by GetOrder/CancelOrder when the broker has no
// order for the given ClOrdID. Callers use errors.Is to branch (e.g. reconcile
// treats not-found as "the intent never reached the broker").
var ErrOrderNotFound = errors.New("brokers: order not found")

// Broker is the typed boundary every venue adapter implements. Every method
// takes a context as its first argument for cancellation/timeout. Money values
// are orders.Decimal throughout — never float64.
type Broker interface {
	// Account returns the cash/equity/buying-power snapshot.
	Account(ctx context.Context) (Account, error)

	// Positions returns all open positions (signed qty: + long / - short).
	Positions(ctx context.Context) ([]Position, error)

	// Quote returns the latest top-of-book quote for symbol.
	Quote(ctx context.Context, symbol string) (Quote, error)

	// PlaceOrder submits an order. It is idempotent on req.ClOrdID: placing the
	// SAME ClOrdID twice returns the SAME BrokerOrder (no duplicate at the venue).
	PlaceOrder(ctx context.Context, req OrderRequest) (BrokerOrder, error)

	// CancelOrder cancels the order identified by its ClOrdID.
	CancelOrder(ctx context.Context, clOrdID string) error

	// GetOrder fetches the broker's current view of an order by ClOrdID, for
	// reconciliation. It returns ErrOrderNotFound if the broker has no such order.
	GetOrder(ctx context.Context, clOrdID string) (BrokerOrder, error)
}
