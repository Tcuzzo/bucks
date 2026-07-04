package main

import (
	"context"
	"fmt"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/channel"
	"bucks/internal/harness"
	"bucks/internal/orders"
	"bucks/internal/risk"
)

const reconcileFailureAlertEvery = 5

// realizedReader reports realized P&L since an instant. It is the LIVE source for
// the risk engine's daily-loss breaker (which was fed a hardcoded zero and so could
// never fire). *memory.Store satisfies it via RealizedPnLSince.
type realizedReader interface {
	RealizedPnLSince(since time.Time) (orders.Decimal, error)
}

type fillReconciler interface {
	Reconcile(ctx context.Context) error
}

// startOfUTCDay is the daily-loss budget boundary: 00:00 UTC of the instant's day.
// US equity/crypto sessions do not cross UTC midnight, so a UTC day cleanly bounds
// "today's realized loss".
func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

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
func newAccountSource(broker brokers.Broker, decider Decider, paceC <-chan time.Time, realized realizedReader, reconciler fillReconciler, now func() time.Time) harness.Source {
	return newAccountSourceWithAlerts(broker, decider, paceC, realized, reconciler, now, nil, nil)
}

func newAccountSourceWithAlerts(broker brokers.Broker, decider Decider, paceC <-chan time.Time, realized realizedReader, reconciler fillReconciler, now func() time.Time, alerts channel.Channel, logf func(string, ...any)) harness.Source {
	if now == nil {
		now = time.Now
	}
	var consecutiveReconcileFailures int
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
			if reconciler != nil {
				if err := reconciler.Reconcile(ctx); err != nil {
					consecutiveReconcileFailures++
					surfaceReconcileFailure(ctx, alerts, logf, now(), consecutiveReconcileFailures, err)
					continue // bad fill stream — skip; never feed false zero realized P&L
				}
				consecutiveReconcileFailures = 0
			}
			acct, err := broker.Account(ctx)
			if err != nil {
				continue // bad read — skip this tick; never feed a false equity to the safety gate
			}
			positions, _ := broker.Positions(ctx) // positions failure -> flat; equity-based safety still runs
			// Realized P&L today drives the daily-loss breaker. A FAILED read must
			// never feed a false zero (which would silently disable the breaker) —
			// skip the tick, exactly like a bad account read. A nil reader (tests /
			// no ledger) means the breaker sees zero, which is the safe legacy path.
			realizedToday := orders.ZeroDecimal
			if realized != nil {
				rp, rerr := realized.RealizedPnLSince(startOfUTCDay(now()))
				if rerr != nil {
					continue
				}
				realizedToday = rp
			}
			ps := buildPortfolioState(acct, positions, realizedToday)
			decision := decider.Decide(ctx, AccountSnapshot{Equity: acct.Equity, Cash: acct.Cash, Portfolio: ps})
			return harness.TickInput{
				Decision:   decision,
				Portfolio:  ps,
				Equity:     acct.Equity,
				RealizedPL: realizedToday,
			}, true
		}
	})
}

func surfaceReconcileFailure(ctx context.Context, alerts channel.Channel, logf func(string, ...any), at time.Time, consecutive int, err error) {
	if consecutive != 1 && consecutive%reconcileFailureAlertEvery != 0 {
		return
	}
	text := fmt.Sprintf("Fill reconciliation failing (%d consecutive): %v - skipping this tick; daily-loss breaker cannot see new realized losses until reconciliation recovers.", consecutive, err)
	if logf != nil {
		logf("trade loop: %s", text)
	}
	if alerts == nil {
		return
	}
	_ = alerts.SendAlert(ctx, channel.Alert{
		Level: channel.AlertCritical,
		Text:  text,
		Time:  at.UTC(),
	})
}

// buildPortfolioState turns a broker account + positions into the risk engine's PortfolioState.
// Positions are marked at their average entry until a live per-symbol mark is wired (the
// equity-based drawdown gate is unaffected by that). OpenPositionCount/GrossExposure are left
// for the risk engine to DERIVE from Positions (-1 / nil) so they can never disagree.
func buildPortfolioState(acct brokers.Account, positions []brokers.Position, realizedToday orders.Decimal) risk.PortfolioState {
	held := make(map[string]risk.HeldPosition, len(positions))
	for _, p := range positions {
		held[p.Symbol] = risk.HeldPosition{Qty: p.Qty, MarkPx: p.AvgEntryPx}
	}
	return risk.PortfolioState{
		Equity:            acct.Equity,
		Cash:              acct.Cash,
		Positions:         held,
		RealizedPnLToday:  realizedToday, // real, persisted realized P&L — the daily-loss breaker's input
		OpenPositionCount: -1,            // derive from Positions
	}
}
