package harness

import (
	"context"
	"strings"
	"testing"

	"bucks/internal/orders"
	"bucks/internal/risk"
)

// TestPlaceOnBrokerRefusesRealMoneyWithoutLiveEnabled is the PLACEMENT-TIME layer of the
// real-money enforcement: even if every upstream gate were bypassed and a Trader ended up
// wired to a real-money venue (RealMoneyVenue=true) WITHOUT live trading enabled
// (LiveEnabled=false), placement must refuse LOUDLY — PlaceOrder never reaches the broker,
// and the error states the exact reason. The spy broker records every placement, so an
// empty Placed() is the proof no real order was sent.
func TestPlaceOnBrokerRefusesRealMoneyWithoutLiveEnabled(t *testing.T) {
	f := newFixture(t, func(cfg *TraderConfig) {
		cfg.RealMoneyVenue = true // wired to a venue that moves actual funds...
		cfg.LiveEnabled = false   // ...but live trading is NOT enabled
	})
	// Within band and risk-approved — nothing upstream of placement rejects it.
	p := longProposal(t, "AAPL", "10", "100", "99", "100000")
	ps := emptyPortfolio(t, "100000")

	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Reason: "MA cross up", Seq: 1}, ps, dec(t, "100000"))
	if placed := f.broker.Placed(); len(placed) != 0 {
		t.Fatalf("SAFETY VIOLATION: PlaceOrder reached a real-money venue without LiveEnabled; placed=%v", placed)
	}
	if err == nil {
		t.Fatal("refusing a real-money placement must surface a LOUD error, got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "real-money") || !strings.Contains(msg, "live") {
		t.Errorf("the refusal must be unambiguous about the reason (real-money venue, live not enabled), got %q", err)
	}
	if rec.Outcome == OutcomeAutoPlaced || rec.Outcome == OutcomeApprovedPlaced {
		t.Errorf("a refused placement must never be recorded as placed, got %s", rec.Outcome)
	}
}

// TestMockBrokerCanOpenAndCloseWithStops proves the placement layer does not
// trap a position by accepting an entry but refusing its stop-valid close. The
// front-door construction gate prevents this configuration with a real adapter;
// this test uses only the in-memory broker and never touches a network.
func TestMockBrokerCanOpenAndCloseWithStops(t *testing.T) {
	f := newFixture(t, func(cfg *TraderConfig) {
		cfg.RealMoneyVenue = true
		cfg.LiveEnabled = true
	})
	entry := longProposal(t, "AAPL", "10", "100", "99", "100000")
	flat := emptyPortfolio(t, "100000")

	entryRec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: entry, Reason: "open", Seq: 2}, flat, dec(t, "100000"))
	if err != nil {
		t.Fatalf("open on mock broker: %v", err)
	}
	if !entryRec.Outcome.Placed() {
		t.Fatalf("open was not placed on mock broker: %+v", entryRec)
	}

	closeProposal := risk.OrderProposal{
		Symbol:        "AAPL",
		Side:          orders.SideSell,
		Qty:           dec(t, "10"),
		EntryPx:       dec(t, "101"),
		StopPx:        dec(t, "102"),
		AccountEquity: dec(t, "100000"),
	}
	open := risk.PortfolioState{
		Equity:            dec(t, "100000"),
		Positions:         map[string]risk.HeldPosition{"AAPL": {Qty: dec(t, "10"), MarkPx: dec(t, "101")}},
		OpenPositionCount: -1,
	}
	closeRec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: closeProposal, Reason: "close", Seq: 3}, open, dec(t, "100000"))
	if err != nil {
		t.Fatalf("close on mock broker: %v", err)
	}
	if !closeRec.Outcome.Placed() {
		t.Fatalf("close was not placed on mock broker: %+v", closeRec)
	}
	if placed := f.broker.Placed(); len(placed) != 2 {
		t.Fatalf("mock broker placed %d orders, want entry and close: %v", len(placed), placed)
	}
}
