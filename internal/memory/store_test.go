package memory

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bucks/internal/orders"
)

func mustDecimal(t *testing.T, s string) orders.Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return d
}

func openTempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRememberAndRecallTrade(t *testing.T) {
	s := openTempStore(t)

	in := TradeMemory{
		Symbol: "AAPL",
		Setup:  "breakout",
		Entry:  mustDecimal(t, "187.42"),
		Exit:   mustDecimal(t, "191.10"),
		PnL:    mustDecimal(t, "3.68"),
		Lesson: "held through the morning chop, paid off",
		TS:     time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC),
	}
	saved, err := s.RememberTrade(in)
	if err != nil {
		t.Fatalf("remember trade: %v", err)
	}
	if saved.ID == 0 {
		t.Fatalf("expected assigned ID, got 0")
	}

	got, err := s.RecallTrades(TradeFilter{Symbol: "AAPL"})
	if err != nil {
		t.Fatalf("recall trades: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(got))
	}
	r := got[0]
	if r.Symbol != "AAPL" || r.Setup != "breakout" || r.Lesson != in.Lesson {
		t.Fatalf("recalled fields mismatch: %+v", r)
	}
	if !r.Entry.Equal(in.Entry) || !r.Exit.Equal(in.Exit) || !r.PnL.Equal(in.PnL) {
		t.Fatalf("money mismatch: entry=%s exit=%s pnl=%s", r.Entry, r.Exit, r.PnL)
	}
	if !r.TS.Equal(in.TS) {
		t.Fatalf("ts mismatch: got %v want %v", r.TS, in.TS)
	}
}

func TestRememberAndRecallMarket(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.RememberMarket(MarketMemory{Symbol: "BTC-USD", Observation: "funding flipped negative"}); err != nil {
		t.Fatalf("remember market: %v", err)
	}
	if _, err := s.RememberMarket(MarketMemory{Symbol: "ETH-USD", Observation: "thin book overnight"}); err != nil {
		t.Fatalf("remember market: %v", err)
	}

	got, err := s.RecallMarket(MarketFilter{Symbol: "BTC-USD"})
	if err != nil {
		t.Fatalf("recall market: %v", err)
	}
	if len(got) != 1 || got[0].Observation != "funding flipped negative" {
		t.Fatalf("unexpected market recall: %+v", got)
	}

	all, err := s.RecallMarket(MarketFilter{})
	if err != nil {
		t.Fatalf("recall all market: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 market memories, got %d", len(all))
	}
}

// TestForgetRemovesFromRecall proves the spec's delete-truth: a forgotten memory is
// GONE from every subsequent recall — no soft-delete/FTS path keeps serving it.
func TestForgetRemovesFromRecall(t *testing.T) {
	s := openTempStore(t)

	keep, err := s.RememberTrade(TradeMemory{
		Symbol: "TSLA", Setup: "momentum",
		Entry: mustDecimal(t, "240"), Exit: mustDecimal(t, "245"), PnL: mustDecimal(t, "5"),
		Lesson: "keep this one",
	})
	if err != nil {
		t.Fatalf("remember keep: %v", err)
	}
	burn, err := s.RememberTrade(TradeMemory{
		Symbol: "TSLA", Setup: "momentum",
		Entry: mustDecimal(t, "250"), Exit: mustDecimal(t, "242"), PnL: mustDecimal(t, "-8"),
		Lesson: "this one burned him",
	})
	if err != nil {
		t.Fatalf("remember burn: %v", err)
	}

	// Both present before forget.
	before, err := s.RecallTrades(TradeFilter{Symbol: "TSLA"})
	if err != nil {
		t.Fatalf("recall before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected 2 before forget, got %d", len(before))
	}

	// Forget the burned trade.
	if err := s.Forget(burn.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}

	// It must be gone from EVERY recall path: symbol filter, setup filter, and unfiltered.
	for _, f := range []TradeFilter{
		{Symbol: "TSLA"},
		{Setup: "momentum"},
		{}, // unfiltered = all rows
	} {
		got, err := s.RecallTrades(f)
		if err != nil {
			t.Fatalf("recall after forget %+v: %v", f, err)
		}
		for _, r := range got {
			if r.ID == burn.ID {
				t.Fatalf("forgotten trade id=%d STILL served by recall %+v (soft-delete bug)", burn.ID, f)
			}
		}
	}

	// The kept trade survives; exactly one row remains.
	remaining, err := s.RecallTrades(TradeFilter{})
	if err != nil {
		t.Fatalf("recall remaining: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != keep.ID {
		t.Fatalf("expected only kept id=%d to remain, got %+v", keep.ID, remaining)
	}

	// Forgetting a non-existent id reports not-found (no silent no-op masking a bug).
	if err := s.Forget(burn.ID); err != ErrNotFound {
		t.Fatalf("re-forget should be ErrNotFound, got %v", err)
	}
}

// TestForgetMarketRemovesFromRecall proves delete-truth symmetry for market memory:
// a forgotten market observation is GONE from every RecallMarket path (filtered and
// unfiltered), the other observation survives, and re-forgetting reports ErrNotFound.
func TestForgetMarketRemovesFromRecall(t *testing.T) {
	s := openTempStore(t)

	keep, err := s.RememberMarket(MarketMemory{Symbol: "BTC-USD", Observation: "keep this read"})
	if err != nil {
		t.Fatalf("remember keep: %v", err)
	}
	burn, err := s.RememberMarket(MarketMemory{Symbol: "BTC-USD", Observation: "forget this read"})
	if err != nil {
		t.Fatalf("remember burn: %v", err)
	}

	// Both present before forget (same symbol, so both appear in the filtered path).
	before, err := s.RecallMarket(MarketFilter{Symbol: "BTC-USD"})
	if err != nil {
		t.Fatalf("recall before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected 2 market memories before forget, got %d", len(before))
	}

	// Forget one market memory.
	if err := s.ForgetMarket(burn.ID); err != nil {
		t.Fatalf("forget market: %v", err)
	}

	// It must be gone from EVERY recall path: symbol-filtered and unfiltered.
	for _, f := range []MarketFilter{
		{Symbol: "BTC-USD"},
		{}, // unfiltered = all rows
	} {
		got, err := s.RecallMarket(f)
		if err != nil {
			t.Fatalf("recall after forget %+v: %v", f, err)
		}
		for _, r := range got {
			if r.ID == burn.ID {
				t.Fatalf("forgotten market id=%d STILL served by recall %+v (soft-delete bug)", burn.ID, f)
			}
		}
	}

	// The kept observation survives; exactly one row remains.
	remaining, err := s.RecallMarket(MarketFilter{})
	if err != nil {
		t.Fatalf("recall remaining: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != keep.ID {
		t.Fatalf("expected only kept id=%d to remain, got %+v", keep.ID, remaining)
	}

	// Forgetting a non-existent id reports not-found (no silent no-op masking a bug).
	if err := s.ForgetMarket(burn.ID); err != ErrNotFound {
		t.Fatalf("re-forget market should be ErrNotFound, got %v", err)
	}
}

// TestDecimalMoneyExactRoundTrip proves a drift-prone monetary value survives SQLite
// TEXT storage with zero loss (it would not, stored as float64).
func TestDecimalMoneyExactRoundTrip(t *testing.T) {
	s := openTempStore(t)

	// 0.1 + 0.2 has no exact binary float; satoshi-scale precision is required.
	drifty := mustDecimal(t, "0.30000000")
	tiny := mustDecimal(t, "0.00000001")
	big := mustDecimal(t, "1234567.89")

	saved, err := s.RememberTrade(TradeMemory{
		Symbol: "BTC-USD", Setup: "scalp",
		Entry: drifty, Exit: tiny, PnL: big, Lesson: "precision",
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}

	got, err := s.RecallTrades(TradeFilter{Symbol: "BTC-USD"})
	if err != nil || len(got) != 1 {
		t.Fatalf("recall: %v (n=%d)", err, len(got))
	}
	r := got[0]
	if r.ID != saved.ID {
		t.Fatalf("id mismatch: %d vs %d", r.ID, saved.ID)
	}
	// Exact string equality is the strongest proof of no float drift.
	if r.Entry.String() != "0.30000000" {
		t.Fatalf("entry drifted: %q", r.Entry.String())
	}
	if r.Exit.String() != "0.00000001" {
		t.Fatalf("exit drifted: %q", r.Exit.String())
	}
	if r.PnL.String() != "1234567.89" {
		t.Fatalf("pnl drifted: %q", r.PnL.String())
	}
	if !r.Entry.Equal(drifty) || !r.Exit.Equal(tiny) || !r.PnL.Equal(big) {
		t.Fatalf("money not equal by value after round trip")
	}
}

func TestRememberTradeRejectsEmptySymbol(t *testing.T) {
	s := openTempStore(t)
	if _, err := s.RememberTrade(TradeMemory{Setup: "x"}); err == nil {
		t.Fatalf("expected error for empty symbol")
	}
}

func TestRecallLimitNewestFirst(t *testing.T) {
	s := openTempStore(t)
	for i := 0; i < 5; i++ {
		if _, err := s.RememberTrade(TradeMemory{
			Symbol: "SPY", Setup: "mr",
			Entry: mustDecimal(t, "500"), Exit: mustDecimal(t, "501"), PnL: mustDecimal(t, "1"),
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	got, err := s.RecallTrades(TradeFilter{Symbol: "SPY", Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit not honored: got %d", len(got))
	}
	if got[0].ID < got[1].ID {
		t.Fatalf("expected newest-first ordering, got %d then %d", got[0].ID, got[1].ID)
	}
}

// copyFile copies src to dst byte-for-byte (used to simulate zip -> unpack of the
// SQLite file).
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open src %q: %v", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create dst %q: %v", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := out.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// copyDir recursively copies a directory tree (used to simulate zip -> unpack of the
// Obsidian vault).
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir dst %q: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("readdir %q: %v", src, err)
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyDir(t, sp, dp)
			continue
		}
		copyFile(t, sp, dp)
	}
}
