package harness

import (
	"context"
	"sync"
	"testing"

	"bucks/internal/orders"
	"bucks/internal/risk"
)

// TestTrader_ConcurrentReadersAndTick documents and proves the loop's concurrency
// boundary: the loop sequences Tick/Heartbeat on its driving goroutine, while
// concurrent READERS (a dashboard calling Ledger / PeakEquity / IsHalted) may run
// on other goroutines. Run under `-race` this must report no data race — the
// Trader's mutex protects all mutable state (ledger, peak, halt latch).
//
// We drive one goroutine issuing Ticks + Heartbeats and several goroutines reading
// the snapshot accessors concurrently. The mock dependencies are each internally
// synchronized, so the only shared mutable state under test is the Trader's own.
func TestTrader_ConcurrentReadersAndTick(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	ps := emptyPortfolio(t, "100000")
	p := longProposal(t, "AAPL", "10", "100", "99", "100000")

	const ticks = 200
	var wg sync.WaitGroup

	// Writer: sequential ticks + heartbeats on ONE goroutine (the loop boundary).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ticks; i++ {
			d := TradeDecision{HasProposal: true, Proposal: p, Reason: "MA", Seq: uint64(i)}
			_, _ = f.trader.Tick(ctx, d, ps, dec(t, "100000"))
			_, _ = f.trader.Heartbeat(ctx, dec(t, "100000"))
		}
	}()

	// Readers: many goroutines hammering the snapshot accessors concurrently.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ticks; i++ {
				_ = f.trader.Ledger()
				_, _ = f.trader.PeakEquity()
				_ = f.trader.IsHalted()
			}
		}()
	}

	wg.Wait()

	// Sanity: the deterministic clOrdID means all 200 ticks share the SAME id
	// across seq only if seq differs — here seq differs each tick, so the broker
	// saw up to `ticks` distinct placements (within band -> auto). At least one
	// placement happened and the ledger captured every tick.
	if got := len(f.trader.Ledger()); got != ticks {
		t.Fatalf("ledger must record every tick: want %d, got %d", ticks, got)
	}
	if _, ok := f.trader.PeakEquity(); !ok {
		t.Fatalf("peak equity must be set after heartbeats")
	}
	_ = orders.ZeroDecimal
	_ = risk.PortfolioState{}
}
