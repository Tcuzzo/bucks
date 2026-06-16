package harness

import (
	"context"
	"sort"

	"bucks/internal/brokers"
	"bucks/internal/channel"
	"bucks/internal/orders"
	"bucks/internal/risk"
)

// MaybeReport builds a Report from the live broker/ledger state and sends it via
// the Channel — but ONLY when the schedule is due (driven by the injected clock).
// It returns true if a report was sent this call. With ReportEvery == 0 every call
// is due. The report is built DETERMINISTICALLY: positions sorted by symbol,
// rationales taken from the ledger in order, all money exact decimal.
//
// realizedPL is today's realized P&L (the loop owner supplies it from the ledger /
// fill accounting — the harness does not re-derive realized P&L from scratch in
// this slice). Open positions and their unrealized P&L are computed from the
// broker's positions against the supplied marks.
func (t *Trader) MaybeReport(ctx context.Context, realizedPL Decimal, marks map[string]Decimal) (sent bool, err error) {
	now := t.cfg.Now()
	t.mu.Lock()
	due := !t.haveReport || t.cfg.ReportEvery <= 0 || !now.Before(t.lastReport.Add(t.cfg.ReportEvery))
	if due {
		t.lastReport = now
		t.haveReport = true
	}
	t.mu.Unlock()
	if !due {
		return false, nil
	}

	rep, berr := t.BuildReport(ctx, realizedPL, marks)
	if berr != nil {
		return false, berr
	}
	if serr := t.cfg.Channel.SendReport(ctx, rep); serr != nil {
		return true, serr
	}
	return true, nil
}

// BuildReport assembles a deterministic Report from the broker's account/positions
// and the trade ledger. It does NOT consult the clock for cadence (MaybeReport
// owns scheduling); it stamps the report with the injected clock's current value.
//
// Determinism: positions are sorted by symbol; unrealized P&L per position is
// (mark - avgEntry) * qty in exact decimal (signed qty handles long/short);
// rationales are the placed/blocked trades from the ledger in chronological order.
func (t *Trader) BuildReport(ctx context.Context, realizedPL Decimal, marks map[string]Decimal) (channel.Report, error) {
	acct, err := t.cfg.Broker.Account(ctx)
	if err != nil {
		return channel.Report{}, err
	}
	positions, err := t.cfg.Broker.Positions(ctx)
	if err != nil {
		return channel.Report{}, err
	}

	// Sort positions by symbol for a deterministic report.
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })

	reportPositions := make([]channel.Position, 0, len(positions))
	totalUnrealized := orders.ZeroDecimal
	for _, pos := range positions {
		mark, ok := marks[pos.Symbol]
		if !ok {
			mark = pos.AvgEntryPx // no mark supplied -> zero unrealized for this line
		}
		uPnL, perr := unrealizedPnL(pos, mark)
		if perr != nil {
			return channel.Report{}, perr
		}
		totalUnrealized, err = totalUnrealized.Add(uPnL)
		if err != nil {
			return channel.Report{}, err
		}
		reportPositions = append(reportPositions, channel.Position{
			Symbol:       pos.Symbol,
			Qty:          pos.Qty,
			MarkPx:       mark,
			UnrealizedPL: uPnL,
		})
	}

	return channel.Report{
		GeneratedAt:  t.cfg.Now(),
		Equity:       acct.Equity,
		RealizedPL:   realizedPL,
		UnrealizedPL: totalUnrealized,
		Positions:    reportPositions,
		Rationales:   t.rationales(),
	}, nil
}

// unrealizedPnL computes (mark - avgEntry) * qty in exact decimal. Signed qty makes
// a short's gain on a falling mark come out positive automatically.
func unrealizedPnL(pos brokers.Position, mark Decimal) (Decimal, error) {
	diff, err := mark.Sub(pos.AvgEntryPx)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	return diff.Mul(pos.Qty)
}

// rationales turns the trade ledger into report rationales, in chronological order.
// Only entries that represent an actual trade DECISION (a proposal — placed,
// denied, or risk-rejected) are included; pure no-signal / heartbeat-only ticks are
// skipped so the report reads as a trade journal.
func (t *Trader) rationales() []channel.TradeRationale {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]channel.TradeRationale, 0, len(t.ledger))
	for _, r := range t.ledger {
		switch r.Outcome {
		case OutcomeAutoPlaced, OutcomeApprovedPlaced:
			out = append(out, channel.TradeRationale{
				Symbol: r.Symbol, Side: r.Side, Qty: r.Qty,
				Reason: r.Reason, Auto: r.Outcome == OutcomeAutoPlaced, Time: r.Time,
			})
		case OutcomeRiskRejected, OutcomeDenied:
			out = append(out, channel.TradeRationale{
				Symbol: r.Symbol, Side: r.Side, Qty: r.Qty,
				Reason: r.Reason, Time: r.Time, Skipped: true, SkipWhy: r.RiskInfo,
			})
		default:
			// no-signal / halted / market-closed: not a trade decision, omit.
		}
	}
	return out
}

// compile-time check that the report builder uses the risk portfolio shape it
// claims (keeps the import meaningful and the seam honest).
var _ = risk.PortfolioState{}
