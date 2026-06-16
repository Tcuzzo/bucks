package backtest

import (
	"math"
	"strconv"
	"testing"

	"bucks/internal/data"
	"bucks/internal/kernel"
	"bucks/internal/orders"
	"bucks/internal/strategy"
)

// d parses a decimal literal or fails.
func d(t *testing.T, s string) orders.Decimal {
	t.Helper()
	v, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return v
}

// ftoa renders a float64 fixture price as shortest decimal text (test fixtures only;
// the money ledger is all Decimal).
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// bar builds a Bar with O=H=L=C unless overridden; prices via canonical string.
func bar(t *testing.T, sym string, idx int, open, high, low, close float64) data.Bar {
	t.Helper()
	mk := func(f float64) orders.Decimal {
		v, err := orders.ParseDecimal(ftoa(f))
		if err != nil {
			t.Fatalf("bar price %v: %v", f, err)
		}
		return v
	}
	return data.Bar{
		Symbol: sym, Open: mk(open), High: mk(high), Low: mk(low), Close: mk(close),
		Volume: d(t, "100"),
		TS:     kernel.UnixNanos(int64(idx) * 1_000_000_000),
	}
}

// scriptedStrategy is a deterministic test strategy: it emits the action scheduled
// for each bar index from a map, so a test controls EXACTLY when entries/exits
// happen and can hand-compute the resulting money. Indicators are irrelevant here —
// this isolates the ENGINE's fee/slippage/ledger math from indicator behavior.
type scriptedStrategy struct {
	name    string
	actions map[int]strategy.Signal
	i       int
}

func (s *scriptedStrategy) Name() string { return s.name }
func (s *scriptedStrategy) OnBar(b data.Bar) strategy.Signal {
	sig, ok := s.actions[s.i]
	s.i++
	if !ok {
		return strategy.Signal{Action: strategy.Hold}
	}
	return sig
}

// baseConfig: $100k cash, qty 100, no fees/slippage, fill at signal-bar close.
func baseConfig(t *testing.T) Config {
	return Config{
		StartingCash:       d(t, "100000"),
		Qty:                d(t, "100"),
		CommissionPerTrade: orders.ZeroDecimal,
		SlippageBps:        orders.ZeroDecimal,
		EnterAtNextOpen:    false,
	}
}

// ---------------------------------------------------------------------------
// (a) DETERMINISM: fingerprint run1 == run2 over a real strategy on >=100 bars.
// ---------------------------------------------------------------------------

// buildTrendBars makes a deterministic series with several up-trends and pullbacks
// so a real breakout strategy actually trades several times over ~120 bars.
func buildTrendBars(t *testing.T) []data.Bar {
	t.Helper()
	var bars []data.Bar
	idx := 0
	px := 100.0
	// Several alternating regimes so the strategy enters and exits multiple times.
	for cycle := 0; cycle < 6; cycle++ {
		// flat base (builds the channel)
		for k := 0; k < 8; k++ {
			bars = append(bars, bar(t, "AAPL", idx, px, px+1, px-1, px))
			idx++
		}
		// breakout ramp up
		for k := 0; k < 7; k++ {
			px += 4
			bars = append(bars, bar(t, "AAPL", idx, px-3, px+1, px-4, px))
			idx++
		}
		// pullback down (triggers the lower-channel exit)
		for k := 0; k < 7; k++ {
			px -= 5
			bars = append(bars, bar(t, "AAPL", idx, px+3, px+4, px-1, px))
			idx++
		}
	}
	return bars
}

func TestBacktest_DeterministicFingerprint(t *testing.T) {
	bars := buildTrendBars(t)
	if len(bars) < 100 {
		t.Fatalf("need >=100 bars for the determinism gate, built %d", len(bars))
	}
	cfg := Config{
		StartingCash:       d(t, "100000"),
		Qty:                d(t, "100"),
		CommissionPerTrade: d(t, "1.50"),
		SlippageBps:        d(t, "5"),
		EnterAtNextOpen:    true,
	}
	eng := NewEngine(cfg)

	// Two independent runs with FRESH strategy state each time.
	r1, err := eng.Run(strategy.NewBreakout(5, 5), bars)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	r2, err := eng.Run(strategy.NewBreakout(5, 5), bars)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}

	if r1.NumTrades < 3 {
		t.Fatalf("determinism gate wants several trades, got %d", r1.NumTrades)
	}
	fp1, fp2 := r1.Fingerprint(), r2.Fingerprint()
	if fp1 != fp2 {
		t.Fatalf("backtest NOT deterministic: fp1=%s fp2=%s", fp1, fp2)
	}
	// Sanity: the metrics themselves match exactly too.
	if r1.NetPnL.Cmp(r2.NetPnL) != 0 || r1.NumTrades != r2.NumTrades {
		t.Fatalf("results diverged: net %s vs %s, n %d vs %d",
			r1.NetPnL.String(), r2.NetPnL.String(), r1.NumTrades, r2.NumTrades)
	}
	t.Logf("determinism OK: %d trades, net=%s, fingerprint=%s",
		r1.NumTrades, r1.NetPnL.String(), fp1[:16])
}

// ---------------------------------------------------------------------------
// (fees+slippage exactness) — a known round trip; NetPnL = Gross - fees - slippage,
// with values chosen to DRIFT under float64 to prove the Decimal money ledger.
// ---------------------------------------------------------------------------

func TestBacktest_FeesAndSlippageExact(t *testing.T) {
	// One long round trip: enter at close 100.005, exit at close 110.015, qty 100.
	// Slippage 30 bps = 0.003 fraction. Commission 0.07 per fill (x2).
	//
	// Entry fill (buy, slips UP):  100.005 * (1 + 0.003) = 100.005 + 0.300015 = 100.305015
	// Exit  fill (sell, slips DOWN): 110.015 * (1 - 0.003) = 110.015 - 0.330045 = 109.684955
	// Gross = (exit - entry) * qty = (109.684955 - 100.305015) * 100
	//       = 9.37994 * 100 = 937.994
	// Fees  = 0.07 + 0.07 = 0.14
	// Net   = 937.994 - 0.14 = 937.854
	//
	// These exact decimals (…015, …045, …994) cannot be represented in binary
	// float64 and would drift in the last digits; the Decimal ledger keeps them exact.
	cfg := Config{
		StartingCash:       d(t, "100000"),
		Qty:                d(t, "100"),
		CommissionPerTrade: d(t, "0.07"),
		SlippageBps:        d(t, "30"),
		EnterAtNextOpen:    false,
	}
	eng := NewEngine(cfg)

	bars := []data.Bar{
		bar(t, "AAPL", 0, 100.005, 100.005, 100.005, 100.005), // enter here
		bar(t, "AAPL", 1, 105, 105, 105, 105),                 // hold
		bar(t, "AAPL", 2, 110.015, 110.015, 110.015, 110.015), // exit here
	}
	script := &scriptedStrategy{
		name: "scripted",
		actions: map[int]strategy.Signal{
			0: {Action: strategy.EnterLong, Stop: d(t, "98"), Reason: "test-enter"},
			2: {Action: strategy.Exit, Reason: "test-exit"},
		},
	}

	res, err := eng.Run(script, bars)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.NumTrades != 1 {
		t.Fatalf("expected exactly 1 trade, got %d", res.NumTrades)
	}
	tr := res.Trades[0]

	wantEntry := d(t, "100.305015")
	wantExit := d(t, "109.684955")
	wantGross := d(t, "937.994")
	wantFees := d(t, "0.14")
	wantNet := d(t, "937.854")

	if tr.EntryPx.Cmp(wantEntry) != 0 {
		t.Fatalf("entry fill: got %s want %s", tr.EntryPx.String(), wantEntry.String())
	}
	if tr.ExitPx.Cmp(wantExit) != 0 {
		t.Fatalf("exit fill: got %s want %s", tr.ExitPx.String(), wantExit.String())
	}
	if tr.GrossPnL.Cmp(wantGross) != 0 {
		t.Fatalf("gross: got %s want %s", tr.GrossPnL.String(), wantGross.String())
	}
	if tr.Fees.Cmp(wantFees) != 0 {
		t.Fatalf("fees: got %s want %s", tr.Fees.String(), wantFees.String())
	}
	if tr.NetPnL.Cmp(wantNet) != 0 {
		t.Fatalf("net: got %s want %s", tr.NetPnL.String(), wantNet.String())
	}
	// NetPnL must equal GrossPnL - Fees exactly.
	grossMinusFees, err := tr.GrossPnL.Sub(tr.Fees)
	if err != nil {
		t.Fatal(err)
	}
	if tr.NetPnL.Cmp(grossMinusFees) != 0 {
		t.Fatalf("NetPnL %s != GrossPnL-Fees %s", tr.NetPnL.String(), grossMinusFees.String())
	}
	// Result aggregates match the single trade.
	if res.NetPnL.Cmp(wantNet) != 0 || res.FeesPaid.Cmp(wantFees) != 0 {
		t.Fatalf("aggregate mismatch: net=%s fees=%s", res.NetPnL.String(), res.FeesPaid.String())
	}
	// EndingCash = 100000 + 937.854.
	if res.EndingCash.Cmp(d(t, "100937.854")) != 0 {
		t.Fatalf("ending cash: got %s want 100937.854", res.EndingCash.String())
	}

	// Float64 DRIFT PROOF: the same arithmetic done in float64 does NOT land on the
	// exact decimal, which is why the money ledger must be Decimal.
	fEntry := 100.005 * (1 + 0.003)
	fExit := 110.015 * (1 - 0.003)
	fGross := (fExit - fEntry) * 100
	fNet := fGross - 0.14
	exactNet, _ := wantNet.Float64()
	if fNet == 937.854 {
		t.Fatalf("expected float64 to DRIFT off 937.854, but it landed exactly (%v) — "+
			"pick values that actually drift", fNet)
	}
	t.Logf("float64 net=%.17g (drifts) vs Decimal net=%s exact; |diff|=%.3g",
		fNet, wantNet.String(), math.Abs(fNet-exactNet))
}

// ---------------------------------------------------------------------------
// SHORT round trip: prove short P&L sign and slippage direction are correct.
// ---------------------------------------------------------------------------

func TestBacktest_ShortRoundTripExact(t *testing.T) {
	// Short: enter at 100 (sell, slips DOWN), exit at 90 (buy, slips UP). Profit on
	// a falling price. Slippage 10 bps = 0.001. Qty 50. Commission 0.
	// Entry fill = 100 * (1-0.001) = 99.9 ; Exit fill = 90 * (1+0.001) = 90.09
	// Gross (short) = (entry - exit) * qty = (99.9 - 90.09) * 50 = 9.81 * 50 = 490.5
	cfg := Config{
		StartingCash:       d(t, "100000"),
		Qty:                d(t, "50"),
		CommissionPerTrade: orders.ZeroDecimal,
		SlippageBps:        d(t, "10"),
		EnterAtNextOpen:    false,
	}
	eng := NewEngine(cfg)
	bars := []data.Bar{
		bar(t, "AAPL", 0, 100, 100, 100, 100),
		bar(t, "AAPL", 1, 90, 90, 90, 90),
	}
	script := &scriptedStrategy{
		name: "short",
		actions: map[int]strategy.Signal{
			0: {Action: strategy.EnterShort, Stop: d(t, "102"), Reason: "short-enter"},
			1: {Action: strategy.Exit, Reason: "short-exit"},
		},
	}
	res, err := eng.Run(script, bars)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.NumTrades != 1 {
		t.Fatalf("expected 1 trade, got %d", res.NumTrades)
	}
	tr := res.Trades[0]
	if tr.Side != orders.SideSell {
		t.Fatalf("expected short (SideSell), got %s", tr.Side)
	}
	if tr.EntryPx.Cmp(d(t, "99.9")) != 0 {
		t.Fatalf("short entry fill got %s want 99.9", tr.EntryPx.String())
	}
	if tr.ExitPx.Cmp(d(t, "90.09")) != 0 {
		t.Fatalf("short exit fill got %s want 90.09", tr.ExitPx.String())
	}
	if tr.GrossPnL.Cmp(d(t, "490.5")) != 0 {
		t.Fatalf("short gross got %s want 490.5", tr.GrossPnL.String())
	}
}

// ---------------------------------------------------------------------------
// (b) EXPECTANCY CLEARS MODELED COST by >= ~2x on a favorable series.
// ---------------------------------------------------------------------------

func TestBacktest_ExpectancyClearsCostGate(t *testing.T) {
	cfg := Config{
		StartingCash:       d(t, "100000"),
		Qty:                d(t, "100"),
		CommissionPerTrade: d(t, "1"),
		SlippageBps:        d(t, "5"), // 5 bps
		EnterAtNextOpen:    false,
	}
	eng := NewEngine(cfg)

	// A repeated, favorable, REALISTIC pattern: enter near a low, exit near a high
	// for a clean ~ +$10/share move, several times. Costs are small relative to the
	// move, so expectancy clears modeled cost by well over 2x.
	var bars []data.Bar
	actions := map[int]strategy.Signal{}
	idx := 0
	for trade := 0; trade < 8; trade++ {
		// entry bar at 100
		bars = append(bars, bar(t, "AAPL", idx, 100, 100, 100, 100))
		actions[idx] = strategy.Signal{Action: strategy.EnterLong, Stop: d(t, "97"), Reason: "fav-enter"}
		idx++
		// a hold bar
		bars = append(bars, bar(t, "AAPL", idx, 105, 106, 104, 105))
		idx++
		// exit bar at 110 (+$10/share gross before costs)
		bars = append(bars, bar(t, "AAPL", idx, 110, 110, 110, 110))
		actions[idx] = strategy.Signal{Action: strategy.Exit, Reason: "fav-exit"}
		idx++
	}
	script := &scriptedStrategy{name: "favorable", actions: actions}

	res, err := eng.Run(script, bars)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.NumTrades < 5 {
		t.Fatalf("need several trades for expectancy, got %d", res.NumTrades)
	}

	// Modeled per-trade cost at the ~entry price 100: 2 commissions + 2 slippage legs.
	perTradeCost, err := eng.PerTradeCost(d(t, "100"), d(t, "100"))
	if err != nil {
		t.Fatalf("per-trade cost: %v", err)
	}
	// Assert Expectancy > 2 * per-trade-cost (exact Decimal comparison).
	twoCost, err := perTradeCost.Add(perTradeCost)
	if err != nil {
		t.Fatal(err)
	}
	if res.Expectancy.Cmp(twoCost) <= 0 {
		t.Fatalf("expectancy %s must exceed 2x modeled cost %s (cost=%s) to clear the gate",
			res.Expectancy.String(), twoCost.String(), perTradeCost.String())
	}
	// And expectancy must be POSITIVE net of costs (it already is net of fees+slip).
	if res.Expectancy.Sign() <= 0 {
		t.Fatalf("expectancy must be positive after costs, got %s", res.Expectancy.String())
	}
	t.Logf("expectancy=%s, per-trade-cost=%s, 2x=%s (cleared)",
		res.Expectancy.String(), perTradeCost.String(), twoCost.String())
}

// ---------------------------------------------------------------------------
// (c) OVERFITTING WARNINGS fire on a rigged ALWAYS-WIN series (PF/win-rate flags).
// ---------------------------------------------------------------------------

func TestBacktest_OverfittingWarningsFire(t *testing.T) {
	cfg := baseConfig(t) // no fees/slippage so every trade is a clean win
	eng := NewEngine(cfg)

	// Rig 10 always-win round trips: enter at 100, exit at 110. Win rate = 100%,
	// no losses → profit factor = +Inf. BOTH red flags must surface.
	var bars []data.Bar
	actions := map[int]strategy.Signal{}
	idx := 0
	for trade := 0; trade < 10; trade++ {
		bars = append(bars, bar(t, "AAPL", idx, 100, 100, 100, 100))
		actions[idx] = strategy.Signal{Action: strategy.EnterLong, Stop: d(t, "95"), Reason: "rig-enter"}
		idx++
		bars = append(bars, bar(t, "AAPL", idx, 110, 110, 110, 110))
		actions[idx] = strategy.Signal{Action: strategy.Exit, Reason: "rig-exit"}
		idx++
	}
	script := &scriptedStrategy{name: "rigged", actions: actions}

	res, err := eng.Run(script, bars)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.NumTrades != 10 {
		t.Fatalf("expected 10 trades, got %d", res.NumTrades)
	}
	if res.WinRate != 1.0 {
		t.Fatalf("rigged series should be 100%% wins, got %.4f", res.WinRate)
	}
	if !math.IsInf(res.ProfitFactor, 1) {
		t.Fatalf("no losses → profit factor should be +Inf, got %v", res.ProfitFactor)
	}
	if len(res.Warnings) < 2 {
		t.Fatalf("expected BOTH overfit warnings (PF>4 and WR>80%%), got %d: %v",
			len(res.Warnings), res.Warnings)
	}
	var sawPF, sawWR bool
	for _, w := range res.Warnings {
		if containsAll(w, "profit factor", "OVERFIT") {
			sawPF = true
		}
		if containsAll(w, "win rate", "OVERFIT") {
			sawWR = true
		}
	}
	if !sawPF {
		t.Fatalf("missing profit-factor overfit warning in %v", res.Warnings)
	}
	if !sawWR {
		t.Fatalf("missing win-rate overfit warning in %v", res.Warnings)
	}
	t.Logf("overfit flags fired: %v", res.Warnings)
}

// TestBacktest_OverfitProfitFactorUsesGrossNotNet is the gross-vs-net bite test. It
// runs with NON-ZERO commission AND slippage and a series whose GROSS profit factor
// exceeds the 4.0 overfit threshold even though fees drag the NET-based ratio under
// it. The series deliberately includes a trade that is a GROSS WINNER but a NET
// LOSER (fees exceed its small gross gain). Assertions:
//
//	(a) the overfit ProfitFactor>4 warning FIRES — and it fires on the GROSS edge,
//	(b) that gross-winner/net-loser trade is counted as a NET LOSS (WinRate is net).
//
// THE BITE: under the OLD net-based accumulation the profit factor is ~3.75 (< 4) so
// the warning would NOT fire — fees would mask a genuinely overfit raw edge. Under
// the correct GROSS accumulation it is ~4.32 (> 4) and the warning fires. Numbers
// (all exact Decimal): qty 100, commission 8/fill (round trip 16), slippage 10 bps.
//   - 4 big winners: enter 100 (buy slips to 100.1), exit 110 (sell slips to 109.89)
//     → gross (109.89-100.1)*100 = 979 each.
//   - 1 small GROSS WINNER / NET LOSER: enter 100 (→100.1), exit 100.30 (→100.1997)
//     → gross (100.1997-100.1)*100 = 9.97 ; net 9.97-16 = -6.03 (a NET LOSS).
//   - 7 gross losers: enter 100 (→100.1), exit 98.90 (→98.8011)
//     → gross (98.8011-100.1)*100 = -129.89 each.
//     gross PF = (4*979 + 9.97) / (7*129.89) = 3925.97 / 909.23 ≈ 4.318  (FIRES)
//     net   PF (old, wrong) = 3852.00 / 1027.26 ≈ 3.750               (would NOT fire)
func TestBacktest_OverfitProfitFactorUsesGrossNotNet(t *testing.T) {
	cfg := Config{
		StartingCash:       d(t, "100000"),
		Qty:                d(t, "100"),
		CommissionPerTrade: d(t, "8"),  // 8 per fill → 16 round trip
		SlippageBps:        d(t, "10"), // 0.001 fraction, applied adversely to both legs
		EnterAtNextOpen:    false,
	}
	eng := NewEngine(cfg)

	var bars []data.Bar
	actions := map[int]strategy.Signal{}
	idx := 0
	addTrade := func(entry, exit float64) {
		bars = append(bars, bar(t, "AAPL", idx, entry, entry, entry, entry))
		actions[idx] = strategy.Signal{Action: strategy.EnterLong, Stop: d(t, "90"), Reason: "gn-enter"}
		idx++
		bars = append(bars, bar(t, "AAPL", idx, exit, exit, exit, exit))
		actions[idx] = strategy.Signal{Action: strategy.Exit, Reason: "gn-exit"}
		idx++
	}
	// 4 big winners.
	for i := 0; i < 4; i++ {
		addTrade(100, 110)
	}
	// 1 small gross-winner / net-loser (this is the trade that bites).
	smallIdx := len(bars) / 2 // its trade ordinal (5th trade, index 4)
	addTrade(100, 100.30)
	// 7 gross losers.
	for i := 0; i < 7; i++ {
		addTrade(100, 98.90)
	}
	script := &scriptedStrategy{name: "gross-vs-net", actions: actions}

	res, err := eng.Run(script, bars)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.NumTrades != 12 {
		t.Fatalf("expected 12 trades, got %d", res.NumTrades)
	}

	// (b) The small trade must be a GROSS WINNER but a NET LOSER (the crux).
	small := res.Trades[smallIdx]
	if small.GrossPnL.Sign() <= 0 {
		t.Fatalf("small trade must be a GROSS winner, got gross=%s", small.GrossPnL.String())
	}
	if small.NetPnL.Sign() >= 0 {
		t.Fatalf("small trade must be a NET loser (fees exceed gross), got net=%s", small.NetPnL.String())
	}
	// Exact values: gross 9.97, net -6.03.
	if small.GrossPnL.Cmp(d(t, "9.97")) != 0 {
		t.Fatalf("small gross got %s want 9.97", small.GrossPnL.String())
	}
	if small.NetPnL.Cmp(d(t, "-6.03")) != 0 {
		t.Fatalf("small net got %s want -6.03", small.NetPnL.String())
	}

	// (b cont.) WinRate counts that trade as a NET loss: 4 net wins / 12 = 0.3333…,
	// NOT 5/12. (Under a buggy gross-based win count it would be 5/12.)
	if res.NumWins != 4 {
		t.Fatalf("net wins must be 4 (small trade is a net loss), got %d", res.NumWins)
	}
	if res.NumLosses != 8 {
		t.Fatalf("net losses must be 8 (7 gross losers + the small net loser), got %d", res.NumLosses)
	}

	// (a) ProfitFactor is the GROSS edge and exceeds 4.0 → overfit warning fires.
	// gross PF ≈ 4.318. If it had been computed on NET (the old bug) it would be
	// ≈ 3.750 and this assertion (and the warning) would FAIL.
	if res.ProfitFactor <= overfitProfitFactor {
		t.Fatalf("gross profit factor must exceed %.1f (overfit signal); got %v — "+
			"if this is ~3.75 the accumulator is using NET P&L (the bug)",
			overfitProfitFactor, res.ProfitFactor)
	}
	if res.ProfitFactor < 4.2 || res.ProfitFactor > 4.45 {
		t.Fatalf("gross profit factor should be ≈4.32, got %v (sanity band 4.2–4.45)", res.ProfitFactor)
	}
	// Prove the bite quantitatively: recompute what the OLD net-based accumulation
	// WOULD yield from the trade log, and confirm it is BELOW 4 (would not warn).
	var netWinSum, netLossSum orders.Decimal = orders.ZeroDecimal, orders.ZeroDecimal
	for _, tr := range res.Trades {
		if tr.NetPnL.Sign() > 0 {
			netWinSum, _ = netWinSum.Add(tr.NetPnL)
		} else if tr.NetPnL.Sign() < 0 {
			netLossSum, _ = netLossSum.Add(tr.NetPnL.Abs())
		}
	}
	oldNetPF := profitFactor(netWinSum, netLossSum)
	if oldNetPF >= overfitProfitFactor {
		t.Fatalf("the bite is gone: net-based PF %v is not below %.1f — pick a sharper series",
			oldNetPF, overfitProfitFactor)
	}

	// The overfit warning must actually be present in the Result.
	var sawPF bool
	for _, w := range res.Warnings {
		if containsAll(w, "profit factor", "OVERFIT") {
			sawPF = true
		}
	}
	if !sawPF {
		t.Fatalf("gross PF %v exceeds %.1f but no overfit warning fired: %v",
			res.ProfitFactor, overfitProfitFactor, res.Warnings)
	}
	t.Logf("gross PF=%v (FIRES) vs net-based PF=%v (would NOT fire); small trade gross=%s net=%s; winRate=%.4f",
		res.ProfitFactor, oldNetPF, small.GrossPnL.String(), small.NetPnL.String(), res.WinRate)
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestBacktest_NoWarningsOnHonestResult confirms warnings DON'T fire on a normal
// mixed-result series (no false positive) — half wins, half losses, PF < 4.
func TestBacktest_NoWarningsOnHonestResult(t *testing.T) {
	cfg := baseConfig(t)
	eng := NewEngine(cfg)
	var bars []data.Bar
	actions := map[int]strategy.Signal{}
	idx := 0
	// 5 wins (+5) and 5 losses (-4) → win rate 50%, PF = 25/20 = 1.25 < 4.
	for trade := 0; trade < 10; trade++ {
		entry := 100.0
		exit := 105.0
		if trade%2 == 1 {
			exit = 96.0 // a losing trade
		}
		bars = append(bars, bar(t, "AAPL", idx, entry, entry, entry, entry))
		actions[idx] = strategy.Signal{Action: strategy.EnterLong, Stop: d(t, "90"), Reason: "e"}
		idx++
		bars = append(bars, bar(t, "AAPL", idx, exit, exit, exit, exit))
		actions[idx] = strategy.Signal{Action: strategy.Exit, Reason: "x"}
		idx++
	}
	script := &scriptedStrategy{name: "honest", actions: actions}
	res, err := eng.Run(script, bars)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.WinRate != 0.5 {
		t.Fatalf("expected 50%% win rate, got %.4f", res.WinRate)
	}
	if res.ProfitFactor >= overfitProfitFactor {
		t.Fatalf("expected PF below %v, got %v", overfitProfitFactor, res.ProfitFactor)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("honest result must have NO overfit warnings, got %v", res.Warnings)
	}
}

// TestBacktest_MaxDrawdownTracked verifies the equity-curve max-drawdown is exact
// Decimal: a win then a bigger loss produces a drawdown equal to the loss.
func TestBacktest_MaxDrawdownTracked(t *testing.T) {
	cfg := baseConfig(t)
	eng := NewEngine(cfg)
	// Trade 1: +500 (100->105, qty 100). Trade 2: -800 (100->92, qty 100).
	bars := []data.Bar{
		bar(t, "AAPL", 0, 100, 100, 100, 100),
		bar(t, "AAPL", 1, 105, 105, 105, 105),
		bar(t, "AAPL", 2, 100, 100, 100, 100),
		bar(t, "AAPL", 3, 92, 92, 92, 92),
	}
	script := &scriptedStrategy{
		name: "dd",
		actions: map[int]strategy.Signal{
			0: {Action: strategy.EnterLong, Stop: d(t, "95"), Reason: "e1"},
			1: {Action: strategy.Exit, Reason: "x1"},
			2: {Action: strategy.EnterLong, Stop: d(t, "90"), Reason: "e2"},
			3: {Action: strategy.Exit, Reason: "x2"},
		},
	}
	res, err := eng.Run(script, bars)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Peak equity = 100000 + 500 = 100500; trough = 100500 - 800 = 99700.
	// Max drawdown = 800.
	if res.MaxDrawdown.Cmp(d(t, "800")) != 0 {
		t.Fatalf("max drawdown got %s want 800", res.MaxDrawdown.String())
	}
}
