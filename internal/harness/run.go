package harness

import (
	"context"

	"bucks/internal/risk"
)

// TickInput is one pull from the loop's brain+market each iteration of Run: the
// trade decision to evaluate, the current portfolio state the risk gate checks
// against, the current equity for the drawdown/heartbeat path, the realized P&L and
// marks for the (scheduled) report. The loop owner's Source supplies it; Run wires
// it through Tick -> Heartbeat -> MaybeReport in that fixed order on a SINGLE
// goroutine (the loop's concurrency boundary — see the package doc).
type TickInput struct {
	Decision   TradeDecision
	Portfolio  risk.PortfolioState
	Equity     Decimal
	RealizedPL Decimal
	Marks      map[string]Decimal
}

// Source produces the next TickInput for the loop, or ok=false to stop. It is the
// seam where strategy/analyst signal generation, the data feed, and account reads
// plug in. Run calls Next on a single goroutine, so a Source need not be
// concurrency-safe with respect to Run.
type Source interface {
	Next(ctx context.Context) (in TickInput, ok bool)
}

// SourceFunc adapts a function to Source.
type SourceFunc func(ctx context.Context) (TickInput, bool)

// Next calls the wrapped function.
func (f SourceFunc) Next(ctx context.Context) (TickInput, bool) { return f(ctx) }

// Run drives the trade loop synchronously on the CALLING goroutine until the Source
// signals stop (ok=false) or the context is canceled. Each iteration runs, in this
// fixed order: Tick (the AUTHORITATIVE pre-trade drawdown gate THEN the trade
// decision through the hybrid-autonomy path — the breach halts before any
// placement on the same tick), Heartbeat (off-tick drawdown safety net + liveness
// pulse), then MaybeReport (scheduled report).
//
// This is the loop's concurrency boundary made concrete: ONE goroutine sequences
// all three so the Trader's state transitions are never interleaved. A caller that
// wants the loop in the background runs `go trader.Run(...)`; the Trader's own
// mutex still protects state from a concurrent reader (Ledger / PeakEquity /
// IsHalted — proven by the -race test).
//
// Run returns the first hard error from any stage (a durable halt-write failure, a
// broker/report error surfaced as fatal), or nil on a clean stop. Per-tick
// soft outcomes (risk-rejected, denied, market-closed) are NOT errors — they are
// recorded in the ledger and the loop continues.
func (t *Trader) Run(ctx context.Context, src Source) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		in, ok := src.Next(ctx)
		if !ok {
			return nil
		}
		if _, err := t.Tick(ctx, in.Decision, in.Portfolio, in.Equity); err != nil {
			return err
		}
		if _, err := t.Heartbeat(ctx, in.Equity); err != nil {
			return err
		}
		if _, err := t.MaybeReport(ctx, in.RealizedPL, in.Marks); err != nil {
			return err
		}
	}
}
