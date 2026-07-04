package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/memory"
	"bucks/internal/orders"
)

type fakeFillReader struct {
	fills []brokers.Fill
	err   error
	calls int
}

func (f *fakeFillReader) FillsSince(ctx context.Context, after time.Time) ([]brokers.Fill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out []brokers.Fill
	for _, fill := range f.fills {
		if fill.At.After(after) {
			out = append(out, fill)
		}
	}
	return out, nil
}

func tempMemoryStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.Open(filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func fill(id, symbol string, side orders.Side, qty, px string, at time.Time) brokers.Fill {
	return brokers.Fill{
		ID:     id,
		Symbol: symbol,
		Side:   side,
		Qty:    orders.MustParseDecimal(qty),
		Px:     orders.MustParseDecimal(px),
		At:     at,
	}
}

type flakyBrokerFillStore struct {
	inner    *memory.Store
	failNext bool
}

func (s *flakyBrokerFillStore) BrokerFillSeen(id string) (bool, error) {
	return s.inner.BrokerFillSeen(id)
}

func (s *flakyBrokerFillStore) RememberBrokerFill(id, symbol string, side orders.Side, qty, px, realized Decimal, at, cursor time.Time, basis []byte) (bool, error) {
	if s.failNext {
		s.failNext = false
		return false, errors.New("transient durable write failure")
	}
	return s.inner.RememberBrokerFill(id, symbol, side, qty, px, realized, at, cursor, basis)
}

func (s *flakyBrokerFillStore) BrokerReconcileState() (time.Time, []byte, bool, error) {
	return s.inner.BrokerReconcileState()
}

func (s *flakyBrokerFillStore) SaveBrokerReconcileState(cursor time.Time, basis []byte) error {
	return s.inner.SaveBrokerReconcileState(cursor, basis)
}

func (s *flakyBrokerFillStore) RememberSeededBrokerFills(fills []brokers.Fill, cursor time.Time, basis []byte) error {
	return s.inner.RememberSeededBrokerFills(fills, cursor, basis)
}

func TestFillReconciler_RestoresFIFOAfterTransientPersistFailure(t *testing.T) {
	store := tempMemoryStore(t)
	flaky := &flakyBrokerFillStore{inner: store, failNext: true}
	start := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	reader := &fakeFillReader{fills: []brokers.Fill{
		fill("act-close-transient", "AAPL", orders.SideSell, "10", "90", start.Add(time.Minute)),
	}}
	reconciler := NewFillReconciler(reader, flaky, start)
	reconciler.Seed("AAPL", orders.MustParseDecimal("10"), orders.MustParseDecimal("100"))

	if err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("first reconcile should fail on the injected durable write error")
	}
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	got, err := store.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("realized after retry: %v", err)
	}
	if got.Cmp(orders.MustParseDecimal("-100")) != 0 {
		t.Fatalf("retry realized = %s, want -100; FIFO basis was corrupted by the failed persist", got.String())
	}
	pos, err := reconciler.Position("AAPL")
	if err != nil {
		t.Fatalf("position after retry: %v", err)
	}
	if pos.Sign() != 0 {
		t.Fatalf("position after retry = %s, want flat", pos.String())
	}
}

type duplicateAfterApplyStore struct{}

func (duplicateAfterApplyStore) BrokerFillSeen(string) (bool, error) { return false, nil }

func (duplicateAfterApplyStore) RememberBrokerFill(string, string, orders.Side, Decimal, Decimal, Decimal, time.Time, time.Time, []byte) (bool, error) {
	return false, nil
}

func (duplicateAfterApplyStore) BrokerReconcileState() (time.Time, []byte, bool, error) {
	return time.Time{}, nil, false, nil
}

func (duplicateAfterApplyStore) SaveBrokerReconcileState(time.Time, []byte) error {
	return nil
}

func (duplicateAfterApplyStore) RememberSeededBrokerFills([]brokers.Fill, time.Time, []byte) error {
	return nil
}

func TestFillReconciler_RestoresFIFOWhenInsertReportsDuplicate(t *testing.T) {
	start := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	reader := &fakeFillReader{fills: []brokers.Fill{
		fill("act-raced-duplicate", "AAPL", orders.SideSell, "10", "90", start.Add(time.Minute)),
	}}
	reconciler := NewFillReconciler(reader, duplicateAfterApplyStore{}, start)
	reconciler.Seed("AAPL", orders.MustParseDecimal("10"), orders.MustParseDecimal("100"))

	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("duplicate insert should be a clean no-op after FIFO restore, got %v", err)
	}
	pos, err := reconciler.Position("AAPL")
	if err != nil {
		t.Fatalf("position after duplicate: %v", err)
	}
	if pos.Cmp(orders.MustParseDecimal("10")) != 0 {
		t.Fatalf("position after duplicate = %s, want original long 10", pos.String())
	}
}

func TestFillReconciler_OutOfBandLosingCloseIsRecordedIdempotentlyAcrossReopen(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "memory.sqlite")
	store, err := memory.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	start := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	closeAt := start.Add(time.Minute)
	reader := &fakeFillReader{
		fills: []brokers.Fill{fill("act-close-1", "AAPL", orders.SideSell, "10", "90", closeAt)},
	}

	reconciler := NewFillReconciler(reader, store, start)
	reconciler.Seed("AAPL", orders.MustParseDecimal("10"), orders.MustParseDecimal("100"))
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("same-process duplicate reconcile: %v", err)
	}
	got, err := store.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("realized after first store: %v", err)
	}
	if got.Cmp(orders.MustParseDecimal("-100")) != 0 {
		t.Fatalf("realized = %s, want -100", got.String())
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := memory.Open(storePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := NewFillReconciler(reader, reopened, start)
	restarted.Seed("AAPL", orders.MustParseDecimal("10"), orders.MustParseDecimal("100"))
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatalf("restart duplicate reconcile: %v", err)
	}
	got, err = reopened.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("realized after reopen: %v", err)
	}
	if got.Cmp(orders.MustParseDecimal("-100")) != 0 {
		t.Fatalf("restart double-counted duplicate fill: realized = %s, want -100", got.String())
	}
}

func TestFillReconciler_AppliesDistinctFillsAtSameTimestampIdempotentlyAcrossRestart(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "memory.sqlite")
	store, err := memory.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	start := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	closeAt := start.Add(time.Minute)
	reader := &fakeFillReader{
		fills: []brokers.Fill{
			fill("act-same-ts-aapl", "AAPL", orders.SideSell, "10", "90", closeAt),
			fill("act-same-ts-msft", "MSFT", orders.SideSell, "10", "80", closeAt),
		},
	}

	reconciler := NewFillReconciler(reader, store, start)
	reconciler.Seed("AAPL", orders.MustParseDecimal("10"), orders.MustParseDecimal("100"))
	reconciler.Seed("MSFT", orders.MustParseDecimal("10"), orders.MustParseDecimal("100"))
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("same-process duplicate reconcile: %v", err)
	}
	got, err := store.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("realized after same-process reconcile: %v", err)
	}
	if got.Cmp(orders.MustParseDecimal("-300")) != 0 {
		t.Fatalf("realized = %s, want -300 from both same-timestamp closes", got.String())
	}
	for _, sym := range []string{"AAPL", "MSFT"} {
		pos, err := reconciler.Position(sym)
		if err != nil {
			t.Fatalf("position %s: %v", sym, err)
		}
		if pos.Sign() != 0 {
			t.Fatalf("%s position = %s, want flat", sym, pos.String())
		}
	}
	for _, id := range []string{"act-same-ts-aapl", "act-same-ts-msft"} {
		seen, err := store.BrokerFillSeen(id)
		if err != nil {
			t.Fatalf("fill seen %s: %v", id, err)
		}
		if !seen {
			t.Fatalf("fill %s was not durably marked seen", id)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := memory.Open(storePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := NewFillReconciler(reader, reopened, start)
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatalf("restart duplicate reconcile: %v", err)
	}
	again, err := reopened.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("realized after reopen: %v", err)
	}
	if again.Cmp(orders.MustParseDecimal("-300")) != 0 {
		t.Fatalf("restart changed realized = %s, want -300", again.String())
	}
}

func TestFillReconciler_BoundaryTimestampFillSurvivesRestart(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "memory.sqlite")
	store, err := memory.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cursor := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	seeder := NewFillReconciler(&fakeFillReader{}, store, cursor.Add(-time.Hour))
	seeder.Seed("AAPL", orders.MustParseDecimal("10"), orders.MustParseDecimal("100"))
	if err := seeder.SaveState(cursor); err != nil {
		t.Fatalf("save seed state: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	reopened, err := memory.Open(storePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reader := &fakeFillReader{fills: []brokers.Fill{
		fill("act-boundary-close", "AAPL", orders.SideSell, "10", "90", cursor),
	}}
	restarted := NewFillReconciler(reader, reopened, cursor.Add(time.Hour))
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatalf("restart reconcile boundary fill: %v", err)
	}
	got, err := reopened.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	if got.Cmp(orders.MustParseDecimal("-100")) != 0 {
		t.Fatalf("boundary fill realized = %s, want -100", got.String())
	}
	pos, err := restarted.Position("AAPL")
	if err != nil {
		t.Fatalf("position after boundary fill: %v", err)
	}
	if pos.Sign() != 0 {
		t.Fatalf("boundary fill left position = %s, want flat", pos.String())
	}
}

func TestFillReconciler_BuyThenSellComputesFIFORealizedPnL(t *testing.T) {
	store := tempMemoryStore(t)
	start := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	reader := &fakeFillReader{fills: []brokers.Fill{
		fill("act-open-1", "MSFT", orders.SideBuy, "5", "20", start.Add(time.Minute)),
		fill("act-close-1", "MSFT", orders.SideSell, "3", "18", start.Add(2*time.Minute)),
	}}
	reconciler := NewFillReconciler(reader, store, start)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := store.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	if got.Cmp(orders.MustParseDecimal("-6")) != 0 {
		t.Fatalf("realized = %s, want -6", got.String())
	}
	pos, err := reconciler.Position("MSFT")
	if err != nil {
		t.Fatalf("position: %v", err)
	}
	if pos.Cmp(orders.MustParseDecimal("2")) != 0 {
		t.Fatalf("remaining position = %s, want 2", pos.String())
	}
}

func TestFillReconciler_BootSeedCursorDoesNotReplayAlreadyReflectedOpenFill(t *testing.T) {
	store := tempMemoryStore(t)
	seedCursor := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	reader := &fakeFillReader{fills: []brokers.Fill{
		fill("act-open-before-seed", "AAPL", orders.SideBuy, "10", "100", seedCursor.Add(-time.Minute)),
		fill("act-close-after-seed", "AAPL", orders.SideSell, "10", "90", seedCursor.Add(time.Minute)),
	}}
	reconciler := NewFillReconciler(reader, store, seedCursor)
	reconciler.Seed("AAPL", orders.MustParseDecimal("10"), orders.MustParseDecimal("100"))
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := store.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	if got.Cmp(orders.MustParseDecimal("-100")) != 0 {
		t.Fatalf("realized = %s, want -100", got.String())
	}
	pos, err := reconciler.Position("AAPL")
	if err != nil {
		t.Fatalf("position: %v", err)
	}
	if pos.Sign() != 0 {
		t.Fatalf("boot-seed open fill was replayed into basis; position = %s, want flat", pos.String())
	}
}
