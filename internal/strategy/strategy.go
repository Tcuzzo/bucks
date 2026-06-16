package strategy

import (
	"math"
	"strconv"

	"bucks/internal/data"
	"bucks/internal/orders"
)

// Action is what a strategy wants done on the current bar.
type Action int

const (
	// Hold is "do nothing" — no new position, no exit.
	Hold Action = iota
	// EnterLong opens (or signals intent to open) a long position.
	EnterLong
	// EnterShort opens (or signals intent to open) a short position.
	EnterShort
	// Exit closes the current open position (long or short).
	Exit
)

// String renders the action for logs, reports, and tests.
func (a Action) String() string {
	switch a {
	case Hold:
		return "Hold"
	case EnterLong:
		return "EnterLong"
	case EnterShort:
		return "EnterShort"
	case Exit:
		return "Exit"
	default:
		return "Action(?)"
	}
}

// Signal is a strategy's decision for one bar.
//
// Stop is the protective ATR stop PRICE and is an orders.Decimal (money), NEVER a
// float64 — it crosses into the money ledger and into risk.CheckOrder, which
// requires a valid protective stop (long: stop strictly BELOW entry; short: stop
// strictly ABOVE entry — see risk.stopOnProtectiveSide). On an entry Action the
// Stop MUST be set on the correct protective side; on Hold/Exit it is unused
// (ZeroDecimal). Reason is plain-English provenance for reports.
type Signal struct {
	Action Action
	Stop   orders.Decimal
	Reason string
}

// Strategy is one stateful trading brain. It maintains its own indicator windows
// across bars (so OnBar is O(window), not a full rescan) and emits a Signal per
// bar. Name() is a stable identifier used in the trade log and the backtest
// fingerprint. Implementations must be deterministic: identical bar sequences in,
// identical signals out, with no wall clock and no RNG.
type Strategy interface {
	Name() string
	OnBar(bar data.Bar) Signal
}

// barFloats pulls a bar's OHLC out as float64 for the SIGNAL indicators. This is
// the single, explicit money→signal crossing for indicator math: indicators are
// float64 by design (see indicators.go). The values flow only into indicators,
// never back into the cash/PnL ledger. A bar price that cannot be represented as a
// float64 (govalues returns ok=false only for non-finite, which a real price never
// is) is treated as 0, which simply keeps the indicator un-ready rather than
// corrupting money.
func barFloats(b data.Bar) (open, high, low, close float64) {
	return decFloat(b.Open), decFloat(b.High), decFloat(b.Low), decFloat(b.Close)
}

// decFloat converts a Decimal to float64 for SIGNAL math only (never money).
func decFloat(d orders.Decimal) float64 {
	f, ok := d.Float64()
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// stopFromATR builds a protective stop PRICE as an exact Decimal from an entry
// price (Decimal) and an ATR distance (float64 signal). The ATR distance is
// formatted to a fixed precision string and parsed into a Decimal, so the
// float64→Decimal crossing happens exactly once, deterministically, and the
// resulting stop is exact money thereafter. mult is the ATR multiple (2 for the
// baseline 2*ATR stop). For a long the stop is entry - mult*ATR (below); for a
// short it is entry + mult*ATR (above).
//
// It returns ok=false if the computed distance is non-positive (a degenerate ATR),
// so a caller never emits an entry with an invalid/zero-distance stop.
func stopFromATR(entry orders.Decimal, atr float64, mult float64, long bool) (orders.Decimal, bool) {
	dist := atr * mult
	if dist <= 0 || math.IsNaN(dist) || math.IsInf(dist, 0) {
		return orders.ZeroDecimal, false
	}
	// Exact-once float→Decimal crossing: format the ATR distance to its shortest
	// round-trip decimal text, then parse it into an exact Decimal. Only the
	// EXPORTED orders surface (ParseDecimal) is used — money stays in one type.
	distDec, err := orders.ParseDecimal(strconv.FormatFloat(dist, 'f', -1, 64))
	if err != nil {
		return orders.ZeroDecimal, false
	}
	var stop orders.Decimal
	if long {
		stop, err = entry.Sub(distDec)
	} else {
		stop, err = entry.Add(distDec)
	}
	if err != nil {
		return orders.ZeroDecimal, false
	}
	// Guard the protective side explicitly: long stop must be < entry, short > entry.
	if long && stop.Cmp(entry) >= 0 {
		return orders.ZeroDecimal, false
	}
	if !long && stop.Cmp(entry) <= 0 {
		return orders.ZeroDecimal, false
	}
	return stop, true
}

// hold is the no-op signal.
func hold() Signal { return Signal{Action: Hold} }
