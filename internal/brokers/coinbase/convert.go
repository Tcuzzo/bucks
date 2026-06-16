package coinbase

import (
	"fmt"
	"strings"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

// All money crosses the Coinbase JSON boundary as decimal STRINGS and is parsed
// straight into orders.Decimal — never float64 — so no binary-float drift enters
// BUCKS. An empty string parses to zero (Coinbase omits fields when zero).

// parseDec parses a Coinbase decimal string into orders.Decimal. Empty/blank
// parses to zero (a missing/zero field is legitimately zero, not an error).
func parseDec(s string) (orders.Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return orders.ZeroDecimal, nil
	}
	v, err := orders.ParseDecimal(s)
	if err != nil {
		return orders.ZeroDecimal, fmt.Errorf("coinbase: parse decimal %q: %w", s, err)
	}
	return v, nil
}

// toCBSide maps our side to Coinbase's BUY/SELL strings.
func toCBSide(s orders.Side) string {
	if s == orders.SideSell {
		return "SELL"
	}
	return "BUY"
}

// fromCBSide maps Coinbase's side string back to ours.
func fromCBSide(s string) orders.Side {
	if strings.EqualFold(s, "SELL") {
		return orders.SideSell
	}
	return orders.SideBuy
}

// fromCBStatus maps Coinbase's order status vocabulary to our small enum.
// Unrecognized statuses map to StatusUnknown so reconcile never treats an unknown
// state as terminal.
func fromCBStatus(s string) brokers.OrderStatus {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "PENDING", "OPEN", "QUEUED":
		return brokers.StatusNew
	case "FILLED":
		return brokers.StatusFilled
	case "CANCELLED", "CANCELED", "EXPIRED":
		return brokers.StatusCanceled
	case "REJECTED", "FAILED":
		return brokers.StatusRejected
	default:
		return brokers.StatusUnknown
	}
}

// mapCBOrder converts a Coinbase order response into our typed BrokerOrder. Every
// money/qty field is parsed from its decimal string (never float64). A non-zero
// filled size with status still open maps to PartiallyFilled.
func mapCBOrder(o cbOrderResp) (brokers.BrokerOrder, error) {
	cum, err := parseDec(o.FilledSize)
	if err != nil {
		return brokers.BrokerOrder{}, fmt.Errorf("coinbase: order filled size: %w", err)
	}
	avg, err := parseDec(o.AvgFillPrice)
	if err != nil {
		return brokers.BrokerOrder{}, fmt.Errorf("coinbase: order avg fill price: %w", err)
	}
	oq, err := parseDec(o.OrderSize)
	if err != nil {
		return brokers.BrokerOrder{}, fmt.Errorf("coinbase: order size: %w", err)
	}
	status := fromCBStatus(o.Status)
	// If the venue still reports the order working but some fills landed, surface
	// PartiallyFilled so reconcile sees the in-flight state.
	if status == brokers.StatusNew && cum.IsPos() {
		status = brokers.StatusPartiallyFilled
	}
	return brokers.BrokerOrder{
		BrokerOrderID: o.OrderID,
		ClOrdID:       o.ClientOrderID,
		Symbol:        o.ProductID,
		Side:          fromCBSide(o.Side),
		Status:        status,
		OrderQty:      oq,
		CumQty:        cum,
		AvgPx:         avg,
	}, nil
}
