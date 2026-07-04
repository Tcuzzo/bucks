package memory

import (
	"testing"
	"time"
)

// The daily-loss breaker was dead because RealizedPnLToday was hardcoded 0.
// RealizedPnLSince sums the persisted realized P&L so the breaker sees a real
// number that survives a restart.
func TestRealizedPnLSinceSumsOnlyTradesAtOrAfterCutoff(t *testing.T) {
	s := openTempStore(t)
	now := time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC)
	startOfDay := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)

	// yesterday's loss must NOT count toward today's budget.
	mustRemember(t, s, "AAPL", "-500", now.AddDate(0, 0, -1))
	// today's realized: -300 and +120 -> net -180.
	mustRemember(t, s, "MSFT", "-300", now.Add(-2*time.Hour))
	mustRemember(t, s, "NVDA", "120", now.Add(-1*time.Hour))

	got, err := s.RealizedPnLSince(startOfDay)
	if err != nil {
		t.Fatalf("RealizedPnLSince: %v", err)
	}
	want := mustDecimal(t, "-180")
	if got.Cmp(want) != 0 {
		t.Fatalf("today realized = %s, want %s", got.String(), want.String())
	}
}

func TestRealizedPnLSinceEmptyIsZero(t *testing.T) {
	s := openTempStore(t)
	got, err := s.RealizedPnLSince(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("RealizedPnLSince: %v", err)
	}
	if got.Sign() != 0 {
		t.Fatalf("empty store realized = %s, want 0", got.String())
	}
}

func TestRealizedPnLSinceIncludesTradeExactlyAtCutoff(t *testing.T) {
	s := openTempStore(t)
	cut := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	mustRemember(t, s, "AAPL", "-75", cut) // exactly at the cutoff -> counts
	got, err := s.RealizedPnLSince(cut)
	if err != nil {
		t.Fatalf("RealizedPnLSince: %v", err)
	}
	if got.Cmp(mustDecimal(t, "-75")) != 0 {
		t.Fatalf("at-cutoff realized = %s, want -75", got.String())
	}
}

func mustRemember(t *testing.T, s *Store, sym, pnl string, ts time.Time) {
	t.Helper()
	if _, err := s.RememberTrade(TradeMemory{
		Symbol: sym,
		Setup:  "test",
		Entry:  mustDecimal(t, "100"),
		Exit:   mustDecimal(t, "100"),
		PnL:    mustDecimal(t, pnl),
		Lesson: "t",
		TS:     ts,
	}); err != nil {
		t.Fatalf("remember %s: %v", sym, err)
	}
}
