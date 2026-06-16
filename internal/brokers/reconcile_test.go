package brokers_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"bucks/internal/brokers"
	"bucks/internal/brokers/mock"
	"bucks/internal/orders"
)

func dec(t *testing.T, s string) orders.Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return d
}

func tmpJournal(t *testing.T) (string, *orders.Journal) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orders.wal")
	j, err := orders.Open(path)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return path, j
}

// clordID returns a deterministic, valid client-order-id for a sequence number.
func clordID(strategy, symbol string, seq uint64) string {
	return orders.ClientOrderID(strategy, symbol, "ENTRY", seq)
}

// findResolved returns the ResolvedOrder for clOrdID, or fails.
func findResolved(t *testing.T, rs []brokers.ResolvedOrder, clOrdID string) brokers.ResolvedOrder {
	t.Helper()
	for _, r := range rs {
		if r.ClOrdID == clOrdID {
			return r
		}
	}
	t.Fatalf("no resolved order for %q in %+v", clOrdID, rs)
	return brokers.ResolvedOrder{}
}

// TestReconcile_Agree_Clean: a fully-filled order in the journal and a matching
// broker position => clean, zero discrepancies, nothing in-flight.
func TestReconcile_Agree_Clean(t *testing.T) {
	ctx := context.Background()
	path, j := tmpJournal(t)

	cl := clordID("trend", "AAPL", 1)
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: cl, Strategy: "trend", Symbol: "AAPL", Side: orders.SideBuy,
		Qty: dec(t, "10"), Px: dec(t, "150"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendFill(cl, "fill-1", dec(t, "10"), dec(t, "150")); err != nil {
		t.Fatal(err)
	}

	b := mock.New()
	// Broker holds the matching +10 AAPL position.
	b.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "10"), AvgEntryPx: dec(t, "150")})

	res, err := brokers.ReconcileOnBoot(ctx, b, path)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Clean {
		t.Fatalf("expected clean, got discrepancies: %+v", res.Discrepancies)
	}
	if len(res.Discrepancies) != 0 {
		t.Fatalf("expected 0 discrepancies, got %d", len(res.Discrepancies))
	}
	if len(res.Resolved) != 0 {
		t.Fatalf("terminal order should not be in-flight; resolved=%+v", res.Resolved)
	}
}

// TestReconcile_InFlight_ResolvedFilled: the headline crash case. The journal has
// an INTENT with no terminal event (in-flight), but the broker reports it FILLED.
// Reconcile must surface it as ResolvedFilled, and (because the broker also holds
// the resulting position that the journal doesn't yet know about) it surfaces the
// position as a discrepancy => dirty.
func TestReconcile_InFlight_ResolvedFilled(t *testing.T) {
	ctx := context.Background()
	path, j := tmpJournal(t)

	cl := clordID("trend", "AAPL", 2)
	// Only the INTENT was journaled (crash before the fill/terminal landed).
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: cl, Strategy: "trend", Symbol: "AAPL", Side: orders.SideBuy,
		Qty: dec(t, "10"), Px: dec(t, "150"),
	}); err != nil {
		t.Fatal(err)
	}

	b := mock.New()
	// Broker truth: the order actually FILLED while we were down.
	b.SeedBrokerOrder(brokers.BrokerOrder{
		BrokerOrderID: "alp-1", ClOrdID: cl, Symbol: "AAPL", Side: orders.SideBuy,
		Status: brokers.StatusFilled, OrderQty: dec(t, "10"), CumQty: dec(t, "10"), AvgPx: dec(t, "150"),
	})
	// ...and the resulting position is now on the books.
	b.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "10"), AvgEntryPx: dec(t, "150")})

	res, err := brokers.ReconcileOnBoot(ctx, b, path)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	r := findResolved(t, res.Resolved, cl)
	if r.Status != brokers.ResolvedFilled {
		t.Fatalf("expected ResolvedFilled, got %s", r.Status)
	}
	if r.Broker.CumQty.Cmp(dec(t, "10")) != 0 {
		t.Fatalf("expected broker cum qty 10, got %s", r.Broker.CumQty)
	}

	// Journal implied 0 AAPL (no terminal fill recorded); broker holds 10 => dirty.
	if res.Clean {
		t.Fatalf("expected dirty: journal implies 0 AAPL but broker holds 10")
	}
	if len(res.Discrepancies) != 1 {
		t.Fatalf("expected 1 discrepancy, got %d: %+v", len(res.Discrepancies), res.Discrepancies)
	}
	d := res.Discrepancies[0]
	if d.Symbol != "AAPL" || d.JournalQty.Cmp(dec(t, "0")) != 0 || d.BrokerQty.Cmp(dec(t, "10")) != 0 {
		t.Fatalf("discrepancy wrong: %+v", d)
	}
}

// TestReconcile_PositionMismatch_Discrepancy: journal and broker terminal state
// agree on order lifecycle, but the position QUANTITIES disagree (broker has more
// than the journal implies) => surfaced as a discrepancy with both qtys.
func TestReconcile_PositionMismatch_Discrepancy(t *testing.T) {
	ctx := context.Background()
	path, j := tmpJournal(t)

	cl := clordID("trend", "MSFT", 3)
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: cl, Strategy: "trend", Symbol: "MSFT", Side: orders.SideBuy,
		Qty: dec(t, "5"), Px: dec(t, "300"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendFill(cl, "fill-1", dec(t, "5"), dec(t, "300")); err != nil {
		t.Fatal(err)
	}

	b := mock.New()
	// Broker shows 8 MSFT (journal implies 5) — a qty mismatch.
	b.SetPosition(brokers.Position{Symbol: "MSFT", Qty: dec(t, "8"), AvgEntryPx: dec(t, "300")})

	res, err := brokers.ReconcileOnBoot(ctx, b, path)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Clean {
		t.Fatalf("expected dirty for 5 vs 8 MSFT")
	}
	if len(res.Discrepancies) != 1 {
		t.Fatalf("expected 1 discrepancy, got %d: %+v", len(res.Discrepancies), res.Discrepancies)
	}
	d := res.Discrepancies[0]
	if d.Symbol != "MSFT" || d.JournalQty.Cmp(dec(t, "5")) != 0 || d.BrokerQty.Cmp(dec(t, "8")) != 0 {
		t.Fatalf("discrepancy wrong: got %+v want MSFT 5/8", d)
	}
}

// TestReconcile_InFlight_Missing: an INTENT was journaled but the broker has no
// record of it (the send never reached the broker). Reconcile resolves it as
// Missing and, since no position resulted, stays clean.
func TestReconcile_InFlight_Missing(t *testing.T) {
	ctx := context.Background()
	path, j := tmpJournal(t)

	cl := clordID("trend", "NVDA", 4)
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: cl, Strategy: "trend", Symbol: "NVDA", Side: orders.SideBuy,
		Qty: dec(t, "3"), Px: dec(t, "900"),
	}); err != nil {
		t.Fatal(err)
	}
	// Broker knows nothing; no positions.
	b := mock.New()

	res, err := brokers.ReconcileOnBoot(ctx, b, path)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	r := findResolved(t, res.Resolved, cl)
	if r.Status != brokers.ResolvedMissing {
		t.Fatalf("expected ResolvedMissing, got %s", r.Status)
	}
	if !res.Clean {
		t.Fatalf("missing order took no position; expected clean, got %+v", res.Discrepancies)
	}
}

// TestReconcile_InFlight_StillOpen: broker reports the in-flight order is still
// working (New). It resolves Open; with no positions on either side it's clean.
func TestReconcile_InFlight_StillOpen(t *testing.T) {
	ctx := context.Background()
	path, j := tmpJournal(t)

	cl := clordID("trend", "AMD", 5)
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: cl, Strategy: "trend", Symbol: "AMD", Side: orders.SideBuy,
		Qty: dec(t, "4"), Px: dec(t, "120"),
	}); err != nil {
		t.Fatal(err)
	}
	b := mock.New()
	b.SeedBrokerOrder(brokers.BrokerOrder{
		BrokerOrderID: "alp-2", ClOrdID: cl, Symbol: "AMD", Side: orders.SideBuy,
		Status: brokers.StatusNew, OrderQty: dec(t, "4"), CumQty: dec(t, "0"),
	})

	res, err := brokers.ReconcileOnBoot(ctx, b, path)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	r := findResolved(t, res.Resolved, cl)
	if r.Status != brokers.ResolvedOpen {
		t.Fatalf("expected ResolvedOpen, got %s", r.Status)
	}
	if !res.Clean {
		t.Fatalf("expected clean (no positions), got %+v", res.Discrepancies)
	}
}

// TestReconcile_ShortPosition_SignedAgree: a SELL fill implies a short (-) and
// the broker reports the matching short => clean (sign handled exactly).
func TestReconcile_ShortPosition_SignedAgree(t *testing.T) {
	ctx := context.Background()
	path, j := tmpJournal(t)

	cl := clordID("mr", "SPY", 6)
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: cl, Strategy: "mr", Symbol: "SPY", Side: orders.SideSell,
		Qty: dec(t, "7"), Px: dec(t, "500"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendFill(cl, "fill-1", dec(t, "7"), dec(t, "500")); err != nil {
		t.Fatal(err)
	}
	b := mock.New()
	b.SetPosition(brokers.Position{Symbol: "SPY", Qty: dec(t, "-7"), AvgEntryPx: dec(t, "500")})

	res, err := brokers.ReconcileOnBoot(ctx, b, path)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Clean {
		t.Fatalf("expected clean short agreement, got %+v", res.Discrepancies)
	}
}

// TestReconcile_GetOrderError_Propagates: a non-not-found broker error during
// resolution is surfaced, not swallowed.
func TestReconcile_GetOrderError_Propagates(t *testing.T) {
	ctx := context.Background()
	path, j := tmpJournal(t)

	cl := clordID("trend", "AAPL", 7)
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: cl, Strategy: "trend", Symbol: "AAPL", Side: orders.SideBuy,
		Qty: dec(t, "1"), Px: dec(t, "150"),
	}); err != nil {
		t.Fatal(err)
	}
	b := mock.New()
	boom := errors.New("broker 500")
	b.FailGetOrder(cl, boom)

	_, err := brokers.ReconcileOnBoot(ctx, b, path)
	if !errors.Is(err, boom) {
		t.Fatalf("expected propagated broker error, got %v", err)
	}
}

// TestReconcile_InFlightPartialFill_NoSpuriousDiscrepancy guards the in-flight-
// contamination bug. A crash can leave an order with an INTENT + one PARTIAL FILL
// journaled but NO terminal event: it replays in-flight with a nonzero CumQty.
// That unsettled CumQty must NOT fold into the journal-implied position — it is
// reconciled separately via GetOrder. Here a SEPARATE terminal order on AAPL
// legitimately settled 100 shares and the broker holds exactly +100 (broker
// truth); the in-flight order's 40 is still working at the broker (Open) and is
// not yet in the broker's position. Reconcile must report ZERO discrepancies.
//
// Bite: this test FAILS if `if rr.InFlight { continue }` is removed from the
// journalPos loop — without it the in-flight 40 inflates journalPos to 140 while
// the broker holds 100, manufacturing a phantom +40 discrepancy.
func TestReconcile_InFlightPartialFill_NoSpuriousDiscrepancy(t *testing.T) {
	ctx := context.Background()
	path, j := tmpJournal(t)

	// Terminal, fully-settled order: 100 AAPL filled. This is the only position
	// the broker actually holds.
	clDone := clordID("trend", "AAPL", 10)
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: clDone, Strategy: "trend", Symbol: "AAPL", Side: orders.SideBuy,
		Qty: dec(t, "100"), Px: dec(t, "150"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendFill(clDone, "fill-done", dec(t, "100"), dec(t, "150")); err != nil {
		t.Fatal(err)
	}

	// In-flight order: ordered 100, ONE partial fill of 40 journaled, then a crash
	// before any terminal event. Replays in-flight (PartiallyFilled) with CumQty 40.
	clInFlight := clordID("trend", "AAPL", 11)
	if err := j.AppendIntent(orders.IntentRecord{
		ClOrdID: clInFlight, Strategy: "trend", Symbol: "AAPL", Side: orders.SideBuy,
		Qty: dec(t, "100"), Px: dec(t, "150"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendFill(clInFlight, "fill-partial-40", dec(t, "40"), dec(t, "150")); err != nil {
		t.Fatal(err)
	}

	b := mock.New()
	// Broker truth: the in-flight order is still partially filled / working (Open),
	// reflecting the 40 it has filled so far at the venue.
	b.SeedBrokerOrder(brokers.BrokerOrder{
		BrokerOrderID: "alp-inflight", ClOrdID: clInFlight, Symbol: "AAPL", Side: orders.SideBuy,
		Status: brokers.StatusPartiallyFilled, OrderQty: dec(t, "100"), CumQty: dec(t, "40"), AvgPx: dec(t, "150"),
	})
	// Broker position is exactly the settled +100 from the terminal order; the
	// in-flight order's 40 is still working and not yet folded into the position.
	b.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "100"), AvgEntryPx: dec(t, "150")})

	res, err := brokers.ReconcileOnBoot(ctx, b, path)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The in-flight order resolves Open (broker still shows it partially filled).
	r := findResolved(t, res.Resolved, clInFlight)
	if r.Status != brokers.ResolvedOpen {
		t.Fatalf("expected in-flight order ResolvedOpen, got %s", r.Status)
	}

	// No spurious discrepancy: journalPos excludes the in-flight 40, so the
	// journal implies exactly the settled 100, matching the broker's +100.
	if !res.Clean {
		t.Fatalf("expected clean: in-flight 40 must not contaminate journalPos; got %+v", res.Discrepancies)
	}
	if len(res.Discrepancies) != 0 {
		t.Fatalf("expected 0 discrepancies, got %d: %+v", len(res.Discrepancies), res.Discrepancies)
	}
}

// TestReconcile_EmptyJournal_Clean: no journal file => nothing to reconcile,
// clean.
func TestReconcile_EmptyJournal_Clean(t *testing.T) {
	ctx := context.Background()
	b := mock.New()
	res, err := brokers.ReconcileOnBoot(ctx, b, filepath.Join(t.TempDir(), "does-not-exist.wal"))
	if err != nil {
		t.Fatalf("reconcile empty: %v", err)
	}
	if !res.Clean || len(res.Resolved) != 0 || len(res.Discrepancies) != 0 {
		t.Fatalf("expected clean/empty, got %+v", res)
	}
}
