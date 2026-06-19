package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bucks/internal/analyst"
	"bucks/internal/brokers"
	"bucks/internal/harness"
	"bucks/internal/orders"
	"bucks/internal/playbook"
	"bucks/internal/risk"
)

// sectorSymbols maps an owner's playbook SECTOR tag to a few liquid symbols the bot will
// consider. This is how the loop builds the strategy FROM THE PLAYBOOK: the owner's sectors
// become the trading universe — the operator never hand-picks tickers. Deliberately small +
// liquid and expandable; an unmapped sector falls back to a liquid default so the bot always
// has something to reason about.
var sectorSymbols = map[string][]string{
	"tech":           {"AAPL", "MSFT", "NVDA", "GOOGL"},
	"technology":     {"AAPL", "MSFT", "NVDA", "GOOGL"},
	"ai":             {"NVDA", "MSFT", "GOOGL"},
	"semis":          {"NVDA", "AMD", "AVGO"},
	"semiconductors": {"NVDA", "AMD", "AVGO"},
	"crypto":         {"BTC/USD", "ETH/USD"},
	"finance":        {"JPM", "BAC", "GS"},
	"financials":     {"JPM", "BAC", "GS"},
	"energy":         {"XOM", "CVX"},
	"healthcare":     {"JNJ", "UNH", "PFE"},
	"consumer":       {"AMZN", "WMT", "COST"},
}

// defaultUniverse is the liquid fallback when the playbook's sectors don't map to anything —
// the bot always has a universe, built from the playbook, never from the operator.
var defaultUniverse = []string{"AAPL", "MSFT", "NVDA"}

const maxUniverse = 8

// playbookUniverse derives the trading universe from the owner's playbook sectors (deduped,
// capped). With no recognized sectors it returns the liquid default.
func playbookUniverse(pb playbook.Playbook) []string {
	seen := map[string]bool{}
	var out []string
	for _, sec := range pb.Sectors {
		for _, sym := range sectorSymbols[strings.ToLower(strings.TrimSpace(sec))] {
			if seen[sym] {
				continue
			}
			seen[sym] = true
			out = append(out, sym)
			if len(out) >= maxUniverse {
				return out
			}
		}
	}
	if len(out) == 0 {
		return defaultUniverse
	}
	return out
}

// stopPctFor returns the protective-stop distance (a fraction of entry) from the owner's risk
// tolerance: conservative gets a tighter stop, aggressive a wider one — so an aggressive
// playbook genuinely trades more aggressively.
func stopPctFor(pb playbook.Playbook) orders.Decimal {
	switch pb.RiskTolerance {
	case playbook.Conservative:
		return orders.MustParseDecimal("0.02")
	case playbook.Aggressive:
		return orders.MustParseDecimal("0.05")
	default:
		return orders.MustParseDecimal("0.03")
	}
}

// riskSizedQty sizes a position from the playbook's per-trade risk budget:
// qty = (equity * riskPct) / |entry - stop|. The risk engine still clamps the budget to its 2%
// hard cap, so this can never exceed the invariant. A zero stop distance is an error.
func riskSizedQty(equity, riskPct, entry, stop orders.Decimal) (orders.Decimal, error) {
	budget, err := equity.Mul(riskPct)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	dist, err := entry.Sub(stop)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	dist = dist.Abs()
	if dist.Sign() <= 0 {
		return orders.ZeroDecimal, errors.New("zero stop distance")
	}
	return budget.Quo(dist)
}

// playbookDecider is the autonomous, PLAYBOOK-DRIVEN trade decider: each tick it takes one
// symbol from the playbook-derived universe, reads its quote, prompts the configured brain
// (the analyst — wired with the owner's playbook as its mandate), and turns a bullish lean into
// a risk-sized long entry. The owner picks NOTHING by hand — the playbook (sectors, risk
// tolerance, capital, drawdown, goals) drives the universe, the reasoning, the stop distance,
// and the size. v1 is long-only entries, bounded by the risk engine's concentration /
// max-position limits and the portfolio drawdown halt; brain-driven exits + venue stop-loss
// orders are the next refinement.
type playbookDecider struct {
	an       *analyst.Analyst
	broker   brokers.Broker
	pb       playbook.Playbook
	universe []string
	stopPct  orders.Decimal
	idx      int
	seq      uint64
}

// newPlaybookDecider builds the decider from the owner's playbook + the configured brain.
func newPlaybookDecider(an *analyst.Analyst, broker brokers.Broker, pb playbook.Playbook) *playbookDecider {
	return &playbookDecider{
		an:       an,
		broker:   broker,
		pb:       pb,
		universe: playbookUniverse(pb),
		stopPct:  stopPctFor(pb),
	}
}

// holdDecision is a no-trade tick with a plain-English reason (for the ledger/reports).
func holdDecision(reason string) harness.TradeDecision {
	return harness.TradeDecision{HasProposal: false, Reason: reason}
}

// Decide implements Decider: one playbook-driven symbol evaluation per tick.
func (d *playbookDecider) Decide(ctx context.Context, snap AccountSnapshot) harness.TradeDecision {
	if len(d.universe) == 0 {
		return holdDecision("no trading universe from the playbook")
	}
	sym := d.universe[d.idx%len(d.universe)]
	d.idx++

	q, err := d.broker.Quote(ctx, sym)
	if err != nil || q.Ask.Sign() <= 0 {
		return holdDecision("no quote for " + sym)
	}

	view, err := d.an.Analyze(ctx, analyst.MarketContext{Symbol: sym, Summary: quoteSummary(sym, q)}, nil)
	if err != nil {
		return holdDecision("brain unavailable: " + err.Error())
	}
	if view.Lean != analyst.LeanBullish {
		return holdDecision(sym + ": brain not bullish (" + string(view.Lean) + ")")
	}
	// Long-only v1: never stack a second long on a symbol already held.
	if held, ok := snap.Portfolio.Positions[sym]; ok && held.Qty.Sign() > 0 {
		return holdDecision(sym + ": already long")
	}

	entry := q.Ask
	stop, err := stopBelow(entry, d.stopPct)
	if err != nil {
		return holdDecision(sym + ": stop calc failed")
	}
	qty, err := riskSizedQty(snap.Equity, d.pb.MaxRiskPerTradePct, entry, stop)
	if err != nil || qty.Sign() <= 0 {
		return holdDecision(sym + ": position size too small")
	}
	d.seq++
	return harness.TradeDecision{
		HasProposal: true,
		Proposal: risk.OrderProposal{
			Symbol: sym, Side: orders.SideBuy,
			Qty: qty, EntryPx: entry, StopPx: stop,
			AccountEquity: snap.Equity,
		},
		Reason: fmt.Sprintf("playbook brain: bullish %s — %s", sym, view.Rationale),
		Seq:    d.seq,
	}
}

// stopBelow returns entry * (1 - pct): the protective stop for a long, below the entry.
func stopBelow(entry, pct orders.Decimal) (orders.Decimal, error) {
	one := orders.MustParseDecimal("1")
	factor, err := one.Sub(pct)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	return entry.Mul(factor)
}

// quoteSummary builds a plain market-context line from a real quote for the brain to read.
func quoteSummary(sym string, q brokers.Quote) string {
	return fmt.Sprintf("%s quote — bid %s, ask %s, last %s", sym, q.Bid.String(), q.Ask.String(), q.Last.String())
}
