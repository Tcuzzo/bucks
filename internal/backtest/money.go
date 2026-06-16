package backtest

import (
	"math"

	"bucks/internal/orders"
	"bucks/internal/strategy"
)

// This file holds the engine's MONEY math — every function takes and returns
// orders.Decimal (or integer counts), with NO float64 in the cash/PnL/fee/slippage
// path. The single exception is profitFactor, which produces a DISPLAY ratio
// (float64) from two Decimal sums for human reporting; it is never fed back into a
// money calculation. Slippage uses a Decimal basis-point divisor (10000), so a
// price like 100.005 with 50bps slippage produces an exact 100.005*1.005 fill that
// would DRIFT under float64 — proving the ledger is decimal-exact.

// bpsDivisor is 10000 as a Decimal: basis points → fraction (bps / 10000). Parsed
// once; never float64.
func bpsDivisor() orders.Decimal { return orders.MustParseDecimal("10000") }

// entrySide maps an entry action to the order side that opens the position.
func entrySide(a strategy.Action) orders.Side {
	if a == strategy.EnterShort {
		return orders.SideSell
	}
	return orders.SideBuy
}

// exitSide returns the side of the fill that CLOSES a position of the given side:
// a long is closed by a sell, a short by a buy. Slippage is adverse to the fill
// side, so the exit's slippage direction follows the closing side.
func exitSide(positionSide orders.Side) orders.Side {
	if positionSide == orders.SideBuy {
		return orders.SideSell
	}
	return orders.SideBuy
}

// slippageFraction returns bps/10000 as an exact Decimal.
func slippageFraction(bps orders.Decimal) (orders.Decimal, error) {
	return bps.Quo(bpsDivisor())
}

// applySlippage adjusts a raw fill price ADVERSELY for the fill side: a buy fills
// HIGHER (price * (1 + frac)), a sell fills LOWER (price * (1 - frac)). Exact
// Decimal throughout. A zero bps leaves the price unchanged.
func applySlippage(raw orders.Decimal, side orders.Side, bps orders.Decimal) (orders.Decimal, error) {
	if bps.Sign() == 0 {
		return raw, nil
	}
	frac, err := slippageFraction(bps)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	adj, err := raw.Mul(frac)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	if side == orders.SideBuy {
		return raw.Add(adj) // buy pays up
	}
	return raw.Sub(adj) // sell receives less
}

// slippageAmount returns the per-leg slippage COST in money: refPx * (bps/10000) *
// qty. Used by PerTradeCost. Exact Decimal.
func slippageAmount(refPx, qty, bps orders.Decimal) (orders.Decimal, error) {
	if bps.Sign() == 0 {
		return orders.ZeroDecimal, nil
	}
	frac, err := slippageFraction(bps)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	perUnit, err := refPx.Mul(frac)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	return perUnit.Mul(qty)
}

// grossPnL computes the gross (pre-fee) P&L of a round trip in exact Decimal:
//   - long  (SideBuy):  (exit - entry) * qty
//   - short (SideSell): (entry - exit) * qty
//
// Both fill prices already include slippage.
func grossPnL(side orders.Side, entryPx, exitPx, qty orders.Decimal) (orders.Decimal, error) {
	var diff orders.Decimal
	var err error
	if side == orders.SideBuy {
		diff, err = exitPx.Sub(entryPx)
	} else {
		diff, err = entryPx.Sub(exitPx)
	}
	if err != nil {
		return orders.ZeroDecimal, err
	}
	return diff.Mul(qty)
}

// profitFactor is the ONLY float64 in this file: a DISPLAY ratio of summed GROSS
// (pre-fee) winning P&L to summed |GROSS losing P&L|. Both inputs are Decimal sums
// of per-trade GROSS P&L (NOT net) — this is deliberate: profit factor is the RAW
// SIGNAL-EDGE / overfit detector, so costs must not be able to soften an overfit
// ratio under the threshold. It is used only for the Result's reporting field + the
// overfit warning, never in a money calc.
//
//   - No gross losers but gross winners > 0  → +Inf  (flagged as overfit).
//   - No gross winners and no gross losers   → 0.
func profitFactor(grossProfitSum, grossLossSum orders.Decimal) float64 {
	if grossLossSum.Sign() == 0 {
		if grossProfitSum.Sign() > 0 {
			return math.Inf(1)
		}
		return 0
	}
	w, okw := grossProfitSum.Float64()
	l, okl := grossLossSum.Float64()
	if !okw || !okl || l == 0 {
		return math.Inf(1)
	}
	return w / l
}

// intDecimal renders a non-negative integer count as a Decimal (for exact
// Expectancy = NetPnL / NumTrades). It uses orders.NewDecimal(n, 0).
func intDecimal(n int) orders.Decimal {
	d, err := orders.NewDecimal(int64(n), 0)
	if err != nil {
		// n is a small trade count; this never errors in practice. Fall back to a
		// parsed literal so a money calc never silently uses a wrong value.
		return orders.MustParseDecimal("1")
	}
	return d
}
