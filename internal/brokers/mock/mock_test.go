package mock

import (
	"context"
	"errors"
	"testing"
	"time"

	"bucks/internal/brokers"
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

// TestPlaceOrderIdempotent_SameClOrdIDTwice is the headline idempotency proof:
// placing the SAME ClOrdID twice returns the SAME BrokerOrder and creates NO
// duplicate at the broker.
func TestPlaceOrderIdempotent_SameClOrdIDTwice(t *testing.T) {
	ctx := context.Background()
	m := New()

	req := brokers.OrderRequest{
		ClOrdID: "DETERMINISTICCLORDID0000000000AA",
		Symbol:  "AAPL",
		Side:    orders.SideBuy,
		Qty:     dec(t, "10"),
		Kind:    brokers.KindMarket,
	}

	first, err := m.PlaceOrder(ctx, req)
	if err != nil {
		t.Fatalf("first place: %v", err)
	}
	second, err := m.PlaceOrder(ctx, req)
	if err != nil {
		t.Fatalf("second place: %v", err)
	}

	// Same broker order id back — not a new order.
	if first.BrokerOrderID != second.BrokerOrderID {
		t.Fatalf("idempotency broken: first id=%q second id=%q (a duplicate was created)",
			first.BrokerOrderID, second.BrokerOrderID)
	}
	if second.ClOrdID != req.ClOrdID {
		t.Fatalf("ClOrdID not preserved: got %q want %q", second.ClOrdID, req.ClOrdID)
	}

	// Exactly ONE order recorded for that ClOrdID.
	placed := m.Placed()
	if len(placed) != 1 {
		t.Fatalf("expected exactly 1 placed order, got %d: %v", len(placed), placed)
	}
	if placed[0] != req.ClOrdID {
		t.Fatalf("placed[0]=%q want %q", placed[0], req.ClOrdID)
	}
}

// TestPlaceOrder_DistinctClOrdIDs_AreDistinct guards against the idempotency
// logic collapsing genuinely different orders.
func TestPlaceOrder_DistinctClOrdIDs_AreDistinct(t *testing.T) {
	ctx := context.Background()
	m := New()

	a, err := m.PlaceOrder(ctx, brokers.OrderRequest{
		ClOrdID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Symbol:  "AAPL", Side: orders.SideBuy, Qty: dec(t, "1"), Kind: brokers.KindMarket,
	})
	if err != nil {
		t.Fatalf("place a: %v", err)
	}
	b, err := m.PlaceOrder(ctx, brokers.OrderRequest{
		ClOrdID: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		Symbol:  "MSFT", Side: orders.SideSell, Qty: dec(t, "2"), Kind: brokers.KindMarket,
	})
	if err != nil {
		t.Fatalf("place b: %v", err)
	}
	if a.BrokerOrderID == b.BrokerOrderID {
		t.Fatalf("distinct ClOrdIDs collapsed to one broker id: %q", a.BrokerOrderID)
	}
	if got := m.Placed(); len(got) != 2 {
		t.Fatalf("expected 2 placed, got %d", len(got))
	}
}

func TestAccountPositionsQuote_Seed(t *testing.T) {
	ctx := context.Background()
	m := New()

	m.SetAccount(brokers.Account{
		Cash:        dec(t, "1000.50"),
		Equity:      dec(t, "2500.75"),
		BuyingPower: dec(t, "5000"),
	})
	acct, err := m.Account(ctx)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if acct.Cash.Cmp(dec(t, "1000.50")) != 0 || acct.Equity.Cmp(dec(t, "2500.75")) != 0 {
		t.Fatalf("account mismatch: %+v", acct)
	}

	m.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "10"), AvgEntryPx: dec(t, "150")})
	m.SetPosition(brokers.Position{Symbol: "TSLA", Qty: dec(t, "-5"), AvgEntryPx: dec(t, "200")})
	pos, err := m.Positions(ctx)
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(pos) != 2 {
		t.Fatalf("expected 2 positions, got %d", len(pos))
	}
	// Symbol-sorted: AAPL then TSLA.
	if pos[0].Symbol != "AAPL" || pos[1].Symbol != "TSLA" {
		t.Fatalf("positions not symbol-sorted: %v %v", pos[0].Symbol, pos[1].Symbol)
	}
	if pos[1].Qty.Cmp(dec(t, "-5")) != 0 {
		t.Fatalf("short qty sign lost: %s", pos[1].Qty)
	}

	m.SetQuote(brokers.Quote{Symbol: "AAPL", Bid: dec(t, "149.90"), Ask: dec(t, "150.10"), Last: dec(t, "150.00")})
	q, err := m.Quote(ctx, "AAPL")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if q.Bid.Cmp(dec(t, "149.90")) != 0 || q.Ask.Cmp(dec(t, "150.10")) != 0 {
		t.Fatalf("quote mismatch: %+v", q)
	}
}

func TestCancelOrder_KnownAndUnknown(t *testing.T) {
	ctx := context.Background()
	m := New()
	req := brokers.OrderRequest{
		ClOrdID: "CANCELME000000000000000000000000",
		Symbol:  "AAPL", Side: orders.SideBuy, Qty: dec(t, "1"), Kind: brokers.KindMarket,
	}
	if _, err := m.PlaceOrder(ctx, req); err != nil {
		t.Fatalf("place: %v", err)
	}
	if err := m.CancelOrder(ctx, req.ClOrdID); err != nil {
		t.Fatalf("cancel known: %v", err)
	}
	if got := m.Canceled(); len(got) != 1 || got[0] != req.ClOrdID {
		t.Fatalf("canceled record wrong: %v", got)
	}
	bo, err := m.GetOrder(ctx, req.ClOrdID)
	if err != nil {
		t.Fatalf("get after cancel: %v", err)
	}
	if bo.Status != brokers.StatusCanceled {
		t.Fatalf("status after cancel = %s, want Canceled", bo.Status)
	}

	if err := m.CancelOrder(ctx, "NOSUCHORDER000000000000000000000"); !errors.Is(err, brokers.ErrOrderNotFound) {
		t.Fatalf("cancel unknown: got %v, want ErrOrderNotFound", err)
	}
}

func TestGetOrder_Unknown(t *testing.T) {
	ctx := context.Background()
	m := New()
	if _, err := m.GetOrder(ctx, "MISSING0000000000000000000000000"); !errors.Is(err, brokers.ErrOrderNotFound) {
		t.Fatalf("get unknown: got %v, want ErrOrderNotFound", err)
	}
}

func TestFillsSince_ReturnsSeededFillsAfterCursor(t *testing.T) {
	ctx := context.Background()
	m := New()
	cursor := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	m.SetFills([]brokers.Fill{
		{ID: "before", Symbol: "AAPL", Side: orders.SideBuy, Qty: dec(t, "10"), Px: dec(t, "100"), At: cursor.Add(-time.Second)},
		{ID: "after-2", Symbol: "MSFT", Side: orders.SideSell, Qty: dec(t, "2"), Px: dec(t, "50"), At: cursor.Add(2 * time.Second)},
		{ID: "after-1", Symbol: "AAPL", Side: orders.SideSell, Qty: dec(t, "1"), Px: dec(t, "90"), At: cursor.Add(time.Second)},
	})

	got, err := m.FillsSince(ctx, cursor)
	if err != nil {
		t.Fatalf("fills since: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("fills after cursor = %d, want 2: %+v", len(got), got)
	}
	if got[0].ID != "after-1" || got[1].ID != "after-2" {
		t.Fatalf("fills not filtered and sorted by time: %+v", got)
	}
}

func TestContextCanceled_ShortCircuits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := New()
	if _, err := m.Account(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("account with canceled ctx: got %v", err)
	}
	if _, err := m.PlaceOrder(ctx, brokers.OrderRequest{ClOrdID: "X"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("place with canceled ctx: got %v", err)
	}
}

// TestConcurrentPlace_NoDuplicate exercises the mutex + idempotency under the
// race detector: many goroutines placing the SAME ClOrdID must yield exactly one
// order.
func TestConcurrentPlace_NoDuplicate(t *testing.T) {
	ctx := context.Background()
	m := New()
	req := brokers.OrderRequest{
		ClOrdID: "CONCURRENT0000000000000000000000",
		Symbol:  "AAPL", Side: orders.SideBuy, Qty: dec(t, "1"), Kind: brokers.KindMarket,
	}
	const n = 50
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := m.PlaceOrder(ctx, req); err != nil {
				t.Errorf("concurrent place: %v", err)
			}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	if got := m.Placed(); len(got) != 1 {
		t.Fatalf("concurrent idempotency broken: %d orders placed, want 1", len(got))
	}
}
