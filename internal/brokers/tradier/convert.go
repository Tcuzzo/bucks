package tradier

import (
	"encoding/json"
	"fmt"
	"strings"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

// Tradier returns money/qty as JSON numbers. We decode them as json.Number and
// parse the EXACT text into orders.Decimal — never through float64 — so no
// binary-float drift enters BUCKS. An empty/zero json.Number parses to zero.

// numDec parses a json.Number into orders.Decimal. An empty number is zero.
func numDec(n json.Number) (orders.Decimal, error) {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return orders.ZeroDecimal, nil
	}
	v, err := orders.ParseDecimal(s)
	if err != nil {
		return orders.ZeroDecimal, fmt.Errorf("tradier: parse number %q: %w", s, err)
	}
	return v, nil
}

// toTDSide maps our side to Tradier's equity side strings.
func toTDSide(s orders.Side) string {
	if s == orders.SideSell {
		return "sell"
	}
	return "buy"
}

// fromTDSide maps Tradier's side back to ours (buy/buy_to_cover => buy;
// sell/sell_short => sell).
func fromTDSide(s string) orders.Side {
	ls := strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(ls, "sell") {
		return orders.SideSell
	}
	return orders.SideBuy
}

// fromTDStatus maps Tradier's order status vocabulary to our small enum.
// Unrecognized statuses map to StatusUnknown so reconcile never treats an unknown
// state as terminal.
func fromTDStatus(s string) brokers.OrderStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "open", "pending", "accepted", "calculated", "received":
		return brokers.StatusNew
	case "partially_filled":
		return brokers.StatusPartiallyFilled
	case "filled":
		return brokers.StatusFilled
	case "canceled", "cancelled", "expired", "rejected_cancel":
		return brokers.StatusCanceled
	case "rejected", "error":
		return brokers.StatusRejected
	default:
		return brokers.StatusUnknown
	}
}

// mapTDOrder converts a Tradier order into our typed BrokerOrder. Every qty/price
// field is parsed from its JSON number text (never float64). A non-zero executed
// quantity under an open status maps to PartiallyFilled.
func mapTDOrder(o tdOrder) (brokers.BrokerOrder, error) {
	oq, err := numDec(o.Quantity)
	if err != nil {
		return brokers.BrokerOrder{}, fmt.Errorf("tradier: order quantity: %w", err)
	}
	cum, err := numDec(o.ExecQty)
	if err != nil {
		return brokers.BrokerOrder{}, fmt.Errorf("tradier: order exec quantity: %w", err)
	}
	avg, err := numDec(o.AvgFillPx)
	if err != nil {
		return brokers.BrokerOrder{}, fmt.Errorf("tradier: order avg fill price: %w", err)
	}
	status := fromTDStatus(o.Status)
	if status == brokers.StatusNew && cum.IsPos() {
		status = brokers.StatusPartiallyFilled
	}
	return brokers.BrokerOrder{
		BrokerOrderID: o.ID.String(),
		ClOrdID:       o.Tag,
		Symbol:        o.Symbol,
		Side:          fromTDSide(o.Side),
		Status:        status,
		OrderQty:      oq,
		CumQty:        cum,
		AvgPx:         avg,
	}, nil
}
