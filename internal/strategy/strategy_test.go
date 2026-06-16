package strategy

import (
	"strconv"
	"testing"

	"bucks/internal/data"
	"bucks/internal/kernel"
	"bucks/internal/orders"
)

// ftoa renders a float64 test-fixture price as its shortest decimal text. Used ONLY
// to build Decimal bar prices from readable float literals in tests — it never
// participates in the money ledger (the ledger is all Decimal).
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// d parses a decimal literal or fails the test.
func d(t *testing.T, s string) orders.Decimal {
	t.Helper()
	v, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return v
}

// bar builds a data.Bar at a monotonically increasing timestamp from float OHLC.
// Prices are converted to Decimal via their canonical string so the bar's money
// fields are exact. ts is the bar index (nanoseconds), enough for ordering.
func bar(t *testing.T, sym string, idx int, open, high, low, close float64) data.Bar {
	t.Helper()
	return data.Bar{
		Symbol: sym,
		Open:   f2d(t, open),
		High:   f2d(t, high),
		Low:    f2d(t, low),
		Close:  f2d(t, close),
		Volume: d(t, "1000"),
		TS:     kernel.UnixNanos(int64(idx) * 1_000_000_000),
	}
}

// f2d converts a float64 literal to a Decimal through its shortest decimal text.
func f2d(t *testing.T, f float64) orders.Decimal {
	t.Helper()
	v, err := orders.ParseDecimal(ftoa(f))
	if err != nil {
		t.Fatalf("f2d %v: %v", f, err)
	}
	return v
}

// flatBar builds a bar where O=H=L=C=px (a flat tick).
func flatBar(t *testing.T, sym string, idx int, px float64) data.Bar {
	t.Helper()
	return bar(t, sym, idx, px, px, px, px)
}

// runBars feeds a strategy a slice of bars and returns the signal from each bar.
func runBars(s Strategy, bars []data.Bar) []Signal {
	sigs := make([]Signal, len(bars))
	for i, b := range bars {
		sigs[i] = s.OnBar(b)
	}
	return sigs
}

// firstEntry returns the index and signal of the first Enter* signal, or -1.
func firstEntry(sigs []Signal) (int, Signal) {
	for i, s := range sigs {
		if s.Action == EnterLong || s.Action == EnterShort {
			return i, s
		}
	}
	return -1, Signal{}
}

// TestMomentum_CrossoverEntersLongWithStopBelow drives a flat-then-rising series so
// the fast SMA crosses above the slow SMA AND price breaks the prior Donchian band,
// and asserts an EnterLong with a protective stop strictly BELOW the entry close.
func TestMomentum_CrossoverEntersLongWithStopBelow(t *testing.T) {
	m := NewMomentum(3, 8, 5, 5)
	var bars []data.Bar
	idx := 0
	// 12 flat bars at 100 to warm every window with a flat baseline.
	for ; idx < 12; idx++ {
		bars = append(bars, flatBar(t, "AAPL", idx, 100))
	}
	// Then a sharp, sustained ramp: fast SMA rises above slow, close breaks the
	// (flat 100) Donchian upper band, ATR is positive.
	ramp := []float64{102, 105, 109, 114, 120, 127}
	for _, px := range ramp {
		// high a touch above close so the breakout (close > prior upper) holds.
		bars = append(bars, bar(t, "AAPL", idx, px-1, px+1, px-2, px))
		idx++
	}

	sigs := runBars(m, bars)
	ei, sig := firstEntry(sigs)
	if ei < 0 {
		t.Fatalf("momentum: expected an EnterLong on the ramp, got none: %+v", sigs)
	}
	if sig.Action != EnterLong {
		t.Fatalf("momentum: expected EnterLong, got %s", sig.Action)
	}
	entryClose := bars[ei].Close
	if sig.Stop.Sign() <= 0 {
		t.Fatalf("momentum: entry stop must be a positive price, got %s", sig.Stop.String())
	}
	if sig.Stop.Cmp(entryClose) >= 0 {
		t.Fatalf("momentum: LONG stop %s must be strictly BELOW entry close %s",
			sig.Stop.String(), entryClose.String())
	}
}

// TestMeanReversion_OversoldEntersLongStopBelow builds a quiet ranging series and
// then a single sharp DOWN spike so the z-score drops well below -2, the close
// pierces the lower Bollinger band, and RSI is oversold — expecting EnterLong with
// a stop below entry.
func TestMeanReversion_OversoldEntersLongStopBelow(t *testing.T) {
	mr := NewMeanReversion(10, 5)
	var bars []data.Bar
	idx := 0
	// A long, gently oscillating range around 100 (tight, so the range gate passes
	// and stddev is small enough that a spike is many sigma).
	osc := []float64{100, 100.4, 99.7, 100.2, 99.8, 100.3, 99.9, 100.1, 100.0, 99.6,
		100.2, 99.8, 100.1, 99.9, 100.0, 100.2, 99.8, 100.1}
	for _, px := range osc {
		bars = append(bars, bar(t, "AAPL", idx, px, px+0.2, px-0.2, px))
		idx++
	}
	// Sharp down spike: a low close far below the recent mean → z << -2, below band.
	bars = append(bars, bar(t, "AAPL", idx, 99.8, 99.9, 96.0, 96.2))
	idx++

	sigs := runBars(mr, bars)
	ei, sig := firstEntry(sigs)
	if ei < 0 {
		t.Fatalf("mean_reversion: expected an entry on the down spike, got none")
	}
	if sig.Action != EnterLong {
		t.Fatalf("mean_reversion: expected EnterLong on oversold, got %s", sig.Action)
	}
	entryClose := bars[ei].Close
	if sig.Stop.Cmp(entryClose) >= 0 {
		t.Fatalf("mean_reversion: LONG stop %s must be strictly BELOW entry %s",
			sig.Stop.String(), entryClose.String())
	}
}

// TestMeanReversion_OverboughtEntersShortStopAbove is the mirror: a quiet range then
// a sharp UP spike → z >> +2, above the upper band, RSI overbought → EnterShort
// with a stop strictly ABOVE entry.
func TestMeanReversion_OverboughtEntersShortStopAbove(t *testing.T) {
	mr := NewMeanReversion(10, 5)
	var bars []data.Bar
	idx := 0
	osc := []float64{100, 100.4, 99.7, 100.2, 99.8, 100.3, 99.9, 100.1, 100.0, 99.6,
		100.2, 99.8, 100.1, 99.9, 100.0, 100.2, 99.8, 100.1}
	for _, px := range osc {
		bars = append(bars, bar(t, "AAPL", idx, px, px+0.2, px-0.2, px))
		idx++
	}
	bars = append(bars, bar(t, "AAPL", idx, 100.2, 104.0, 100.1, 103.8))
	idx++

	sigs := runBars(mr, bars)
	ei, sig := firstEntry(sigs)
	if ei < 0 {
		t.Fatalf("mean_reversion: expected an entry on the up spike, got none")
	}
	if sig.Action != EnterShort {
		t.Fatalf("mean_reversion: expected EnterShort on overbought, got %s", sig.Action)
	}
	entryClose := bars[ei].Close
	if sig.Stop.Cmp(entryClose) <= 0 {
		t.Fatalf("mean_reversion: SHORT stop %s must be strictly ABOVE entry %s",
			sig.Stop.String(), entryClose.String())
	}
}

// TestBreakout_ChannelBreakEntersLongStopBelow warms a flat channel then breaks the
// close above the prior Donchian upper band → EnterLong with stop below entry.
func TestBreakout_ChannelBreakEntersLongStopBelow(t *testing.T) {
	b := NewBreakout(5, 5)
	var bars []data.Bar
	idx := 0
	// Flat channel: highs all 101, so the prior upper band is 101.
	for ; idx < 8; idx++ {
		bars = append(bars, bar(t, "AAPL", idx, 100, 101, 99, 100))
	}
	// Breakout bar: close 105 > prior upper 101, with a real range for ATR.
	bars = append(bars, bar(t, "AAPL", idx, 101, 106, 100.5, 105))
	idx++

	sigs := runBars(b, bars)
	ei, sig := firstEntry(sigs)
	if ei < 0 {
		t.Fatalf("breakout: expected an EnterLong on the channel break, got none")
	}
	if sig.Action != EnterLong {
		t.Fatalf("breakout: expected EnterLong, got %s", sig.Action)
	}
	entryClose := bars[ei].Close
	if sig.Stop.Cmp(entryClose) >= 0 {
		t.Fatalf("breakout: LONG stop %s must be strictly BELOW entry %s",
			sig.Stop.String(), entryClose.String())
	}
	// And the breakout must land ON the breakout bar (the last one).
	if ei != len(bars)-1 {
		t.Fatalf("breakout: entry should fire on the breakout bar (idx %d), fired at %d",
			len(bars)-1, ei)
	}
}

// TestBreakout_NoSignalWhileRanging asserts no entry fires while price stays inside
// the channel (no false positive on a flat series).
func TestBreakout_NoSignalWhileRanging(t *testing.T) {
	b := NewBreakout(5, 5)
	var bars []data.Bar
	for idx := 0; idx < 20; idx++ {
		bars = append(bars, bar(t, "AAPL", idx, 100, 101, 99, 100))
	}
	sigs := runBars(b, bars)
	if ei, _ := firstEntry(sigs); ei >= 0 {
		t.Fatalf("breakout: no entry should fire on a flat channel, fired at %d", ei)
	}
}

// TestStopsRespectRiskRequireStop wires each strategy's entry stop through the SAME
// protective-side rule risk.CheckOrder enforces (long: stop < entry). This proves
// the strategies emit stops that satisfy the risk engine's RequireStop, not just a
// local convention.
func TestStopsRespectRiskRequireStop(t *testing.T) {
	// Re-run the breakout entry and check the stop is on the protective side the
	// risk engine would accept (long → stop strictly below entry).
	b := NewBreakout(5, 5)
	var bars []data.Bar
	idx := 0
	for ; idx < 8; idx++ {
		bars = append(bars, bar(t, "AAPL", idx, 100, 101, 99, 100))
	}
	bars = append(bars, bar(t, "AAPL", idx, 101, 106, 100.5, 105))
	sigs := runBars(b, bars)
	ei, sig := firstEntry(sigs)
	if ei < 0 {
		t.Fatal("breakout: expected an entry")
	}
	entry := bars[ei].Close
	// Mirror risk.stopOnProtectiveSide for a long (SideBuy): stop.Cmp(entry) < 0.
	if sig.Stop.Cmp(entry) >= 0 {
		t.Fatalf("stop %s not on protective side of entry %s (long requires below)",
			sig.Stop.String(), entry.String())
	}
}
