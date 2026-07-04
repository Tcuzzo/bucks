package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/brokers/mock"
	"bucks/internal/ledger"
	"bucks/internal/orders"
)

// positionsFailBroker embeds the mock but makes Positions() fail for its first
// `failFor` calls (then succeeds), to prove the boot-seed retries a transient blip
// but fails CLOSED on a persistent failure.
type positionsFailBroker struct {
	*mock.MockBroker
	err     error
	failFor int
	calls   int
}

func (b *positionsFailBroker) Positions(ctx context.Context) ([]brokers.Position, error) {
	b.calls++
	if b.calls <= b.failFor {
		return nil, b.err
	}
	return b.MockBroker.Positions(ctx)
}

func fastSeedRetry(t *testing.T) {
	t.Helper()
	orig := seedRetryDelay
	seedRetryDelay = 0
	t.Cleanup(func() { seedRetryDelay = orig })
}

func TestSeedLedger_FailsClosedOnUnreadablePositions(t *testing.T) {
	fastSeedRetry(t)
	rec := ledger.NewFillReconciler(nil, tempStore(t), time.Now())
	b := &positionsFailBroker{MockBroker: mock.New(), err: errors.New("broker unreachable"), failFor: 1000}
	if _, err := seedLedgerFromBroker(rec, b); err == nil {
		t.Fatal("a persistent Positions() failure must fail closed (return error), got nil — an unseeded ledger would hide a real loss")
	}
	if b.calls != seedRetryAttempts {
		t.Fatalf("expected %d bounded retries, got %d calls", seedRetryAttempts, b.calls)
	}
}

func TestSeedLedger_RetriesTransientPositionsBlip(t *testing.T) {
	fastSeedRetry(t)
	rec := ledger.NewFillReconciler(nil, tempStore(t), time.Now())
	// fails once, then succeeds (a flat account) — the seed must recover, not refuse.
	b := &positionsFailBroker{MockBroker: mock.New(), err: errors.New("blip"), failFor: 1}
	if _, err := seedLedgerFromBroker(rec, b); err != nil {
		t.Fatalf("a transient blip must be retried, not fail closed: %v", err)
	}
}

// A successful seed installs the broker cost basis, so the NEXT close realizes
// against it (not mis-accounted as a fresh opener).
func TestSeedLedger_SeedsCostBasisSoCloseRealizes(t *testing.T) {
	store := tempStore(t)
	b := mock.New()
	seedAt := time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC)
	closeAt := seedAt.Add(time.Minute)
	rec := ledger.NewFillReconciler(b, store, seedAt.Add(-time.Minute))
	b.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "10"), AvgEntryPx: dec(t, "100")})
	b.SetFills(nil)

	if _, err := seedLedgerFromBrokerAt(rec, b, seedAt); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b.SetFills([]brokers.Fill{{
		ID:     "seed-close",
		Symbol: "AAPL",
		Side:   orders.SideSell,
		Qty:    dec(t, "10"),
		Px:     dec(t, "95"),
		At:     closeAt,
	}})
	// close the seeded long at 95 -> realized 10*(95-100) = -50.
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile close: %v", err)
	}
	got, err := store.RealizedPnLSince(startOfUTCDay(closeAt))
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	if got.Cmp(dec(t, "-50")) != 0 {
		t.Fatalf("realized against seeded basis = %s, want -50 (seed was lost if this is 0)", got.String())
	}
}

func TestSeedLedger_FirstBootSeedBoundaryUsesVenueClockUnderHostLag(t *testing.T) {
	store := tempStore(t)
	b := mock.New()
	hostNow := time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC)
	openAtVenue := hostNow.Add(30 * time.Second)
	closeAtVenue := openAtVenue.Add(time.Minute)
	openFill := brokers.Fill{
		ID:     "venue-open-ahead-of-host",
		Symbol: "AAPL",
		Side:   orders.SideBuy,
		Qty:    dec(t, "10"),
		Px:     dec(t, "100"),
		At:     openAtVenue,
	}
	closeFill := brokers.Fill{
		ID:     "venue-close-after-seed",
		Symbol: "AAPL",
		Side:   orders.SideSell,
		Qty:    dec(t, "10"),
		Px:     dec(t, "90"),
		At:     closeAtVenue,
	}
	b.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "10"), AvgEntryPx: dec(t, "100")})
	b.SetFills([]brokers.Fill{openFill})
	rec := ledger.NewFillReconciler(b, store, startOfUTCDay(hostNow))

	if _, err := seedLedgerFromBrokerAt(rec, b, hostNow); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seen, err := store.BrokerFillSeen(openFill.ID)
	if err != nil {
		t.Fatalf("seeded fill seen check: %v", err)
	}
	if !seen {
		t.Fatalf("seeded opening fill %q was not marked seen", openFill.ID)
	}
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	pos, err := rec.Position("AAPL")
	if err != nil {
		t.Fatalf("position after first reconcile: %v", err)
	}
	if pos.Cmp(dec(t, "10")) != 0 {
		t.Fatalf("seeded position after first reconcile = %s, want 10; venue-clock opener was replayed", pos.String())
	}

	b.SetFills([]brokers.Fill{openFill, closeFill})
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile later close: %v", err)
	}
	pos, err = rec.Position("AAPL")
	if err != nil {
		t.Fatalf("position after close: %v", err)
	}
	if pos.Sign() != 0 {
		t.Fatalf("position after real close = %s, want flat; seeded opener was double-counted", pos.String())
	}
	got, err := store.RealizedPnLSince(startOfUTCDay(closeAtVenue))
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	if got.Cmp(dec(t, "-100")) != 0 {
		t.Fatalf("realized on later close = %s, want -100", got.String())
	}
}

func TestSeedLedger_PostSeedVenueFillStillApplies(t *testing.T) {
	store := tempStore(t)
	b := mock.New()
	seedFillAt := time.Date(2026, 7, 4, 15, 0, 30, 0, time.UTC)
	newFillAt := seedFillAt.Add(time.Minute)
	seededFill := brokers.Fill{
		ID:     "venue-open-before-seed",
		Symbol: "AAPL",
		Side:   orders.SideBuy,
		Qty:    dec(t, "10"),
		Px:     dec(t, "100"),
		At:     seedFillAt,
	}
	newFill := brokers.Fill{
		ID:     "venue-buy-after-seed",
		Symbol: "AAPL",
		Side:   orders.SideBuy,
		Qty:    dec(t, "5"),
		Px:     dec(t, "105"),
		At:     newFillAt,
	}
	b.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "10"), AvgEntryPx: dec(t, "100")})
	b.SetFills([]brokers.Fill{seededFill})
	rec := ledger.NewFillReconciler(b, store, startOfUTCDay(seedFillAt))

	if _, err := seedLedgerFromBrokerAt(rec, b, seedFillAt.Add(-30*time.Second)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b.SetFills([]brokers.Fill{seededFill, newFill})
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile new fill: %v", err)
	}
	pos, err := rec.Position("AAPL")
	if err != nil {
		t.Fatalf("position after new fill: %v", err)
	}
	if pos.Cmp(dec(t, "15")) != 0 {
		t.Fatalf("position after post-seed fill = %s, want 15", pos.String())
	}
	seen, err := store.BrokerFillSeen(newFill.ID)
	if err != nil {
		t.Fatalf("new fill seen check: %v", err)
	}
	if !seen {
		t.Fatalf("post-seed fill %q was not marked seen", newFill.ID)
	}
}

// A first-EVER boot whose broker fill stream contains SAME-DAY activity that predates
// the boot (here a round-trip that closed before boot, so Positions() is flat) must
// report coldStart=true: that pre-boot realized P&L is not in today's budget, and the
// operator must be told loudly.
func TestSeedLedger_FirstBootSameDayPreBootActivityIsColdStart(t *testing.T) {
	store := tempStore(t)
	b := mock.New()
	hostNow := time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC)
	b.SetFills([]brokers.Fill{
		{ID: "today-buy", Symbol: "AAPL", Side: orders.SideBuy, Qty: dec(t, "10"), Px: dec(t, "100"), At: hostNow.Add(-2 * time.Hour)},
		{ID: "today-sell", Symbol: "AAPL", Side: orders.SideSell, Qty: dec(t, "10"), Px: dec(t, "90"), At: hostNow.Add(-1 * time.Hour)},
	})
	rec := ledger.NewFillReconciler(b, store, startOfUTCDay(hostNow))
	coldStart, err := seedLedgerFromBrokerAt(rec, b, hostNow)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !coldStart {
		t.Fatalf("first boot with same-day pre-boot activity must report coldStart=true so the operator is told the day-1 budget starts fresh")
	}
}

// A first boot with only prior-day activity (and a currently-held position seeded from
// Positions) is NOT a cold start — no same-day pre-boot realized was lost, so no notice.
func TestSeedLedger_FirstBootNoSameDayActivityIsNotColdStart(t *testing.T) {
	store := tempStore(t)
	b := mock.New()
	hostNow := time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC)
	b.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "10"), AvgEntryPx: dec(t, "100")})
	b.SetFills([]brokers.Fill{
		{ID: "yesterday-buy", Symbol: "AAPL", Side: orders.SideBuy, Qty: dec(t, "10"), Px: dec(t, "100"), At: hostNow.Add(-48 * time.Hour)},
	})
	rec := ledger.NewFillReconciler(b, store, startOfUTCDay(hostNow))
	coldStart, err := seedLedgerFromBrokerAt(rec, b, hostNow)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if coldStart {
		t.Fatalf("no same-day pre-boot activity must report coldStart=false (no false notice)")
	}
}
