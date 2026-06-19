package main

import (
	"context"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/harness"
	"bucks/internal/orders"
	"bucks/internal/risk"
)

// AccountSnapshot is the real, broker-read account state handed to a Decider each poll: total
// equity, settled cash, and the current portfolio. Every value came from the broker — nothing
// is fabricated.
type AccountSnapshot struct {
	Equity    orders.Decimal
	Cash      orders.Decimal
	Portfolio risk.PortfolioState
}

// Decider is the trade loop's POLICY seam: given the real account snapshot, it returns this
// tick's decision (a Hold or a concrete proposal). It is where strategy/analyst-driven alpha
// plugs in. The production default is monitorOnlyDecider — it always Holds, so the loop runs
// the full safety + monitoring + reporting machinery on the real account but invents NO trade
// until the owner chooses a trading policy (watchlist + strategy/brain).
type Decider interface {
	Decide(ctx context.Context, snap AccountSnapshot) harness.TradeDecision
}

// DeciderFunc adapts a function to Decider.
type DeciderFunc func(ctx context.Context, snap AccountSnapshot) harness.TradeDecision

// Decide calls the wrapped function.
func (f DeciderFunc) Decide(ctx context.Context, snap AccountSnapshot) harness.TradeDecision {
	return f(ctx, snap)
}

// monitorOnlyDecider always Holds: the safe default. The loop still reads real equity, runs
// the drawdown gate + kill switch, and sends heartbeats/reports — it just never opens a trade.
func monitorOnlyDecider() Decider {
	return DeciderFunc(func(context.Context, AccountSnapshot) harness.TradeDecision {
		return harness.TradeDecision{HasProposal: false, Reason: "monitor-only (no trading policy configured yet)"}
	})
}

// newAccountSource builds the trade loop's harness.Source: on each pace tick it reads REAL
// account equity + positions from the broker, asks the decider for this tick's decision, and
// returns the TickInput. A failed account read is SKIPPED (never fed as zero equity, which
// would falsely trip the drawdown halt) — the loop waits for the next tick. When paceC closes
// or ctx ends, the loop stops cleanly. paceC is a real ticker in production and a manual
// channel in tests (so the loop is driven deterministically with no wall-clock sleeps).
func newAccountSource(broker brokers.Broker, decider Decider, paceC <-chan time.Time) harness.Source {
	return harness.SourceFunc(func(ctx context.Context) (harness.TickInput, bool) {
		for {
			select {
			case <-ctx.Done():
				return harness.TickInput{}, false
			case _, ok := <-paceC:
				if !ok {
					return harness.TickInput{}, false
				}
			}
			acct, err := broker.Account(ctx)
			if err != nil {
				continue // bad read — skip this tick; never feed a false equity to the safety gate
			}
			positions, _ := broker.Positions(ctx) // positions failure -> flat; equity-based safety still runs
			ps := buildPortfolioState(acct, positions)
			decision := decider.Decide(ctx, AccountSnapshot{Equity: acct.Equity, Cash: acct.Cash, Portfolio: ps})
			return harness.TickInput{
				Decision:   decision,
				Portfolio:  ps,
				Equity:     acct.Equity,
				RealizedPL: orders.ZeroDecimal, // intraday realized P&L tracking is a later refinement
			}, true
		}
	})
}

// buildPortfolioState turns a broker account + positions into the risk engine's PortfolioState.
// Positions are marked at their average entry until a live per-symbol mark is wired (the
// equity-based drawdown gate is unaffected by that). OpenPositionCount/GrossExposure are left
// for the risk engine to DERIVE from Positions (-1 / nil) so they can never disagree.
func buildPortfolioState(acct brokers.Account, positions []brokers.Position) risk.PortfolioState {
	held := make(map[string]risk.HeldPosition, len(positions))
	for _, p := range positions {
		held[p.Symbol] = risk.HeldPosition{Qty: p.Qty, MarkPx: p.AvgEntryPx}
	}
	return risk.PortfolioState{
		Equity:            acct.Equity,
		Cash:              acct.Cash,
		Positions:         held,
		RealizedPnLToday:  orders.ZeroDecimal,
		OpenPositionCount: -1, // derive from Positions
	}
}
