package risk

import (
	"testing"

	"bucks/internal/orders"
)

// d is a test helper: parse a decimal literal or fail.
func d(t *testing.T, s string) Decimal {
	t.Helper()
	v, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return v
}

// pd is a test helper: parse a decimal literal and return its ADDRESS, for the
// PortfolioState.GrossExposure fast-path (a *Decimal where nil == "not supplied").
func pd(t *testing.T, s string) *Decimal {
	t.Helper()
	v := d(t, s)
	return &v
}

// baseConfig is a fully-populated config used as the starting point for each
// single-limit test; individual tests build a proposal/state that trips exactly
// one limit while all others pass.
func baseConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		MaxRiskPerTradePct:  d(t, "0.01"), // 1% per trade
		MaxDailyLossPct:     d(t, "0.03"), // 3% daily loss halt
		MaxGrossLeverage:    d(t, "2"),    // 2x
		MaxTotalExposure:    d(t, "100000"),
		MaxConcentrationPct: d(t, "0.25"), // 25% per symbol
		MaxOpenPositions:    3,
		RequireStop:         true,
	}
}

func TestCheckOrder_Approved(t *testing.T) {
	e := NewEngine(baseConfig(t))
	// Equity 100000, 1% risk budget = 1000. qty 100, entry 50, stop 49 -> risk
	// = 100*1 = 100 <= 1000. Notional 5000; gross after = 5000 <= 2x*100000 and
	// <= 100000 total and <= 25%*100000=25000 concentration; 0 open positions.
	p := OrderProposal{
		Symbol:        "AAPL",
		Side:          orders.SideBuy,
		Qty:           d(t, "100"),
		EntryPx:       d(t, "50"),
		StopPx:        d(t, "49"),
		AccountEquity: d(t, "100000"),
	}
	ps := PortfolioState{
		Equity:            d(t, "100000"),
		Cash:              d(t, "100000"),
		OpenPositionCount: -1, // not supplied -> derive from Positions
		Positions:         map[string]HeldPosition{},
	}
	dec := e.CheckOrder(p, ps)
	if !dec.Approved {
		t.Fatalf("expected approved, got rejected: limit=%s reason=%s", dec.Limit, dec.Reason)
	}
	if dec.Limit != LimitNone {
		t.Fatalf("approved decision should carry LimitNone, got %s", dec.Limit)
	}
}

// TestCheckOrder_LimitsIndividually trips each limit ONE at a time and asserts
// both Approved==false AND the exact tripped-limit enum.
func TestCheckOrder_LimitsIndividually(t *testing.T) {
	tests := []struct {
		name      string
		mutateCfg func(*Config)
		proposal  OrderProposal
		state     PortfolioState
		wantLimit Limit
	}{
		{
			name: "per-trade risk too big",
			// risk = qty*|entry-stop| = 100*(50-40)=1000 > 1% of 50000 = 500.
			proposal: OrderProposal{
				Symbol: "AAPL", Side: orders.SideBuy,
				Qty: d(t, "100"), EntryPx: d(t, "50"), StopPx: d(t, "40"),
				AccountEquity: d(t, "50000"),
			},
			state: PortfolioState{
				Equity: d(t, "50000"), Cash: d(t, "50000"),
				OpenPositionCount: -1,
				Positions:         map[string]HeldPosition{},
			},
			wantLimit: LimitPerTradeRisk,
		},
		{
			name: "missing stop with RequireStop",
			proposal: OrderProposal{
				Symbol: "AAPL", Side: orders.SideBuy,
				Qty: d(t, "10"), EntryPx: d(t, "50"), StopPx: orders.ZeroDecimal,
				AccountEquity: d(t, "100000"),
			},
			state: PortfolioState{
				Equity: d(t, "100000"), Cash: d(t, "100000"),
				OpenPositionCount: -1,
				Positions:         map[string]HeldPosition{},
			},
			wantLimit: LimitMissingStop,
		},
		{
			name: "stop on wrong side with RequireStop",
			// long with a stop ABOVE entry is not protective.
			proposal: OrderProposal{
				Symbol: "AAPL", Side: orders.SideBuy,
				Qty: d(t, "10"), EntryPx: d(t, "50"), StopPx: d(t, "55"),
				AccountEquity: d(t, "100000"),
			},
			state: PortfolioState{
				Equity: d(t, "100000"), Cash: d(t, "100000"),
				OpenPositionCount: -1,
				Positions:         map[string]HeldPosition{},
			},
			wantLimit: LimitMissingStop,
		},
		{
			name: "daily loss breach -> halt",
			// realized loss today -3000 on equity 100000 = 3% = budget reached.
			proposal: OrderProposal{
				Symbol: "AAPL", Side: orders.SideBuy,
				Qty: d(t, "1"), EntryPx: d(t, "50"), StopPx: d(t, "49"),
				AccountEquity: d(t, "100000"),
			},
			state: PortfolioState{
				Equity: d(t, "100000"), Cash: d(t, "100000"),
				RealizedPnLToday:  d(t, "-3000"),
				OpenPositionCount: -1,
				Positions:         map[string]HeldPosition{},
			},
			wantLimit: LimitDailyLoss,
		},
		{
			name: "leverage exceeded",
			// equity 10000, 2x budget = 20000 gross. Existing gross 18000, new
			// order notional 50*100=5000 -> 23000 > 20000. Keep per-trade risk
			// small (50*(100-99)=50 <= 1% of 10000 = 100) and concentration ok.
			mutateCfg: func(c *Config) {
				c.MaxTotalExposure = d(t, "1000000") // disable total-exposure trip
				c.MaxConcentrationPct = orders.ZeroDecimal
			},
			proposal: OrderProposal{
				Symbol: "TSLA", Side: orders.SideBuy,
				Qty: d(t, "50"), EntryPx: d(t, "100"), StopPx: d(t, "99"),
				AccountEquity: d(t, "10000"),
			},
			state: PortfolioState{
				Equity: d(t, "10000"), Cash: d(t, "0"),
				GrossExposure:     pd(t, "18000"),
				OpenPositionCount: -1,
				Positions: map[string]HeldPosition{
					"MSFT": {Qty: d(t, "60"), MarkPx: d(t, "300")}, // 18000
				},
			},
			wantLimit: LimitGrossLeverage,
		},
		{
			name: "total exposure exceeded",
			// leverage disabled, total-exposure cap 100000. Existing gross 98000,
			// order notional 50*50=2500 -> 100500 > 100000.
			mutateCfg: func(c *Config) {
				c.MaxGrossLeverage = orders.ZeroDecimal
				c.MaxTotalExposure = d(t, "100000")
				c.MaxConcentrationPct = orders.ZeroDecimal
			},
			proposal: OrderProposal{
				Symbol: "NVDA", Side: orders.SideBuy,
				Qty: d(t, "50"), EntryPx: d(t, "50"), StopPx: d(t, "49"),
				AccountEquity: d(t, "1000000"),
			},
			state: PortfolioState{
				Equity: d(t, "1000000"), Cash: d(t, "0"),
				GrossExposure:     pd(t, "98000"),
				OpenPositionCount: -1,
				Positions:         map[string]HeldPosition{},
			},
			wantLimit: LimitTotalExposure,
		},
		{
			name: "concentration exceeded",
			// 25% of equity 40000 = 10000 per-symbol budget. Existing AAPL
			// exposure 8000, order notional 100*30=3000 -> 11000 > 10000.
			mutateCfg: func(c *Config) {
				c.MaxGrossLeverage = orders.ZeroDecimal
				c.MaxTotalExposure = orders.ZeroDecimal
			},
			proposal: OrderProposal{
				Symbol: "AAPL", Side: orders.SideBuy,
				Qty: d(t, "100"), EntryPx: d(t, "30"), StopPx: d(t, "29.9"),
				AccountEquity: d(t, "40000"),
			},
			state: PortfolioState{
				Equity: d(t, "40000"), Cash: d(t, "0"),
				OpenPositionCount: -1,
				Positions: map[string]HeldPosition{
					"AAPL": {Qty: d(t, "200"), MarkPx: d(t, "40")}, // 8000
				},
			},
			wantLimit: LimitConcentration,
		},
		{
			name: "max open positions exceeded",
			// MaxOpenPositions 3, already 3 distinct held, opening a NEW symbol.
			mutateCfg: func(c *Config) {
				c.MaxGrossLeverage = orders.ZeroDecimal
				c.MaxTotalExposure = orders.ZeroDecimal
				c.MaxConcentrationPct = orders.ZeroDecimal
			},
			proposal: OrderProposal{
				Symbol: "NEW", Side: orders.SideBuy,
				Qty: d(t, "1"), EntryPx: d(t, "50"), StopPx: d(t, "49"),
				AccountEquity: d(t, "100000"),
			},
			state: PortfolioState{
				Equity: d(t, "100000"), Cash: d(t, "0"),
				OpenPositionCount: -1,
				Positions: map[string]HeldPosition{
					"AAA": {Qty: d(t, "1"), MarkPx: d(t, "10")},
					"BBB": {Qty: d(t, "1"), MarkPx: d(t, "10")},
					"CCC": {Qty: d(t, "1"), MarkPx: d(t, "10")},
				},
			},
			wantLimit: LimitMaxOpenPositions,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t)
			if tc.mutateCfg != nil {
				tc.mutateCfg(&cfg)
			}
			e := NewEngine(cfg)
			dec := e.CheckOrder(tc.proposal, tc.state)
			if dec.Approved {
				t.Fatalf("expected rejection on %q, got approved", tc.name)
			}
			if dec.Limit != tc.wantLimit {
				t.Fatalf("wrong tripped limit for %q: got %s (%s), want %s",
					tc.name, dec.Limit, dec.Reason, tc.wantLimit)
			}
		})
	}
}

// TestCheckOrder_ExactDecimalMath uses values that DRIFT under float64 to prove
// the per-trade-risk comparison is exact. 0.1 + 0.2 != 0.3 in IEEE-754; here the
// per-trade risk lands EXACTLY on the budget, which must APPROVE (<=) and would
// spuriously REJECT if computed with float64 (0.30000000000000004 > 0.3).
func TestCheckOrder_ExactDecimalMath(t *testing.T) {
	// qty=1, entry-stop = 0.1+0.2 worth of distance expressed as entry=10.3,
	// stop=10.0 -> distance 0.3 exactly; risk = 1*0.3 = 0.3.
	// Equity chosen so 1% budget == 0.3 EXACTLY: 0.01 * 30 = 0.3.
	e := NewEngine(Config{
		MaxRiskPerTradePct: d(t, "0.01"),
		RequireStop:        true,
	})
	p := OrderProposal{
		Symbol: "AAPL", Side: orders.SideBuy,
		Qty: d(t, "1"), EntryPx: d(t, "10.3"), StopPx: d(t, "10.0"),
		AccountEquity: d(t, "30"),
	}
	ps := PortfolioState{Equity: d(t, "30"), OpenPositionCount: -1, Positions: map[string]HeldPosition{}}

	dec := e.CheckOrder(p, ps)
	if !dec.Approved {
		t.Fatalf("exact-boundary risk 0.3 vs budget 0.3 must APPROVE (<=); "+
			"float64 would mis-reject. got limit=%s reason=%s", dec.Limit, dec.Reason)
	}

	// Now nudge equity down by the smallest amount so budget = 0.299999... < 0.3:
	// budget = 0.01 * 29.99 = 0.2999 < 0.3 -> must REJECT on per-trade risk.
	ps2 := PortfolioState{Equity: d(t, "29.99"), OpenPositionCount: -1, Positions: map[string]HeldPosition{}}
	p2 := p
	p2.AccountEquity = d(t, "29.99")
	dec2 := e.CheckOrder(p2, ps2)
	if dec2.Approved {
		t.Fatal("risk 0.3 vs budget 0.2999 must REJECT")
	}
	if dec2.Limit != LimitPerTradeRisk {
		t.Fatalf("want LimitPerTradeRisk, got %s", dec2.Limit)
	}
}

// TestNewEngine_HardCapClamp proves a config asking for >2% per-trade risk is
// clamped to the 2% hard cap.
func TestNewEngine_HardCapClamp(t *testing.T) {
	e := NewEngine(Config{MaxRiskPerTradePct: d(t, "0.10")}) // asks for 10%
	got := e.EffectiveMaxRiskPerTradePct()
	want := d(t, "0.02")
	if got.Cmp(want) != 0 {
		t.Fatalf("per-trade risk not clamped to hard cap: got %s want %s", got.String(), want.String())
	}
}

// TestNewEngine_Default proves a zero per-trade-risk config defaults to 1%.
func TestNewEngine_Default(t *testing.T) {
	e := NewEngine(Config{}) // zero -> default
	got := e.EffectiveMaxRiskPerTradePct()
	want := d(t, "0.01")
	if got.Cmp(want) != 0 {
		t.Fatalf("default per-trade risk wrong: got %s want %s", got.String(), want.String())
	}
}

// TestCheckOrder_InvalidInput proves malformed proposals are rejected, never
// silently approved.
func TestCheckOrder_InvalidInput(t *testing.T) {
	e := NewEngine(DefaultConfig())
	cases := []struct {
		name string
		p    OrderProposal
		ps   PortfolioState
	}{
		{
			name: "zero qty",
			p:    OrderProposal{Symbol: "X", Side: orders.SideBuy, Qty: orders.ZeroDecimal, EntryPx: d(t, "10"), StopPx: d(t, "9"), AccountEquity: d(t, "1000")},
			ps:   PortfolioState{Equity: d(t, "1000"), OpenPositionCount: -1, Positions: map[string]HeldPosition{}},
		},
		{
			name: "zero entry",
			p:    OrderProposal{Symbol: "X", Side: orders.SideBuy, Qty: d(t, "1"), EntryPx: orders.ZeroDecimal, StopPx: d(t, "9"), AccountEquity: d(t, "1000")},
			ps:   PortfolioState{Equity: d(t, "1000"), OpenPositionCount: -1, Positions: map[string]HeldPosition{}},
		},
		{
			name: "zero equity",
			p:    OrderProposal{Symbol: "X", Side: orders.SideBuy, Qty: d(t, "1"), EntryPx: d(t, "10"), StopPx: d(t, "9"), AccountEquity: orders.ZeroDecimal},
			ps:   PortfolioState{Equity: orders.ZeroDecimal, OpenPositionCount: -1, Positions: map[string]HeldPosition{}},
		},
		{
			name: "invalid side",
			p:    OrderProposal{Symbol: "X", Side: orders.Side(99), Qty: d(t, "1"), EntryPx: d(t, "10"), StopPx: d(t, "9"), AccountEquity: d(t, "1000")},
			ps:   PortfolioState{Equity: d(t, "1000"), OpenPositionCount: -1, Positions: map[string]HeldPosition{}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec := e.CheckOrder(c.p, c.ps)
			if dec.Approved {
				t.Fatalf("expected rejection for %q", c.name)
			}
			if dec.Limit != LimitInvalidInput {
				t.Fatalf("want LimitInvalidInput, got %s", dec.Limit)
			}
		})
	}
}

func TestCheckOrder_InvalidSideCannotMasqueradeAsReducingExposure(t *testing.T) {
	e := NewEngine(Config{
		MaxGrossLeverage: d(t, "1"),
		RequireStop:      false,
	})
	ps := PortfolioState{
		Equity:            d(t, "100000"),
		Cash:              d(t, "0"),
		OpenPositionCount: -1,
		Positions: map[string]HeldPosition{
			"AAPL": {Qty: d(t, "1200"), MarkPx: d(t, "100")},
		},
	}
	p := OrderProposal{
		Symbol:        "AAPL",
		Side:          orders.Side(99),
		Qty:           d(t, "300"),
		EntryPx:       d(t, "100"),
		AccountEquity: d(t, "100000"),
	}

	dec := e.CheckOrder(p, ps)
	if dec.Approved {
		t.Fatal("invalid side must not be approved as a risk-reducing exit")
	}
	if dec.Limit != LimitInvalidInput {
		t.Fatalf("want LimitInvalidInput, got %s (%s)", dec.Limit, dec.Reason)
	}
}

// TestCheckOrder_AddToExistingDoesNotOpenSlot proves adding to a held symbol does
// not consume a new open-position slot.
func TestCheckOrder_AddToExistingDoesNotOpenSlot(t *testing.T) {
	cfg := baseConfig(t)
	cfg.MaxOpenPositions = 3
	cfg.MaxGrossLeverage = orders.ZeroDecimal
	cfg.MaxTotalExposure = orders.ZeroDecimal
	cfg.MaxConcentrationPct = orders.ZeroDecimal
	e := NewEngine(cfg)
	ps := PortfolioState{
		Equity: d(t, "100000"), Cash: d(t, "0"),
		OpenPositionCount: -1,
		Positions: map[string]HeldPosition{
			"AAA": {Qty: d(t, "1"), MarkPx: d(t, "10")},
			"BBB": {Qty: d(t, "1"), MarkPx: d(t, "10")},
			"CCC": {Qty: d(t, "1"), MarkPx: d(t, "10")},
		},
	}
	// Adding to AAA (already held) at 3/3 should still be allowed.
	p := OrderProposal{Symbol: "AAA", Side: orders.SideBuy, Qty: d(t, "1"), EntryPx: d(t, "10"), StopPx: d(t, "9"), AccountEquity: d(t, "100000")}
	dec := e.CheckOrder(p, ps)
	if !dec.Approved {
		t.Fatalf("adding to existing symbol at max-open should be approved, got %s: %s", dec.Limit, dec.Reason)
	}
}

func TestCheckOrder_RiskReducingOrdersReduceExposure(t *testing.T) {
	cfg := Config{
		MaxGrossLeverage:    d(t, "1"),
		MaxConcentrationPct: d(t, "1"),
		RequireStop:         false,
	}
	e := NewEngine(cfg)
	cases := []struct {
		name      string
		heldQty   string
		orderSide orders.Side
	}{
		{
			name:      "sell reduces long",
			heldQty:   "1200",
			orderSide: orders.SideSell,
		},
		{
			name:      "buy reduces short",
			heldQty:   "-1200",
			orderSide: orders.SideBuy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := PortfolioState{
				Equity:            d(t, "100000"),
				Cash:              d(t, "0"),
				OpenPositionCount: -1,
				Positions: map[string]HeldPosition{
					"AAPL": {Qty: d(t, tc.heldQty), MarkPx: d(t, "100")},
				},
			}
			p := OrderProposal{
				Symbol:        "AAPL",
				Side:          tc.orderSide,
				Qty:           d(t, "300"),
				EntryPx:       d(t, "100"),
				AccountEquity: d(t, "100000"),
			}

			dec := e.CheckOrder(p, ps)
			if !dec.Approved {
				t.Fatalf("risk-reducing order should be approved, got %s: %s", dec.Limit, dec.Reason)
			}
		})
	}
}

func TestCheckOrder_ShortOpeningSellStillIncreasesExposure(t *testing.T) {
	e := NewEngine(Config{
		MaxGrossLeverage: d(t, "1"),
		RequireStop:      false,
	})
	ps := PortfolioState{
		Equity:            d(t, "100000"),
		Cash:              d(t, "0"),
		OpenPositionCount: -1,
		Positions:         map[string]HeldPosition{},
	}
	p := OrderProposal{
		Symbol:        "AAPL",
		Side:          orders.SideSell,
		Qty:           d(t, "1100"),
		EntryPx:       d(t, "100"),
		AccountEquity: d(t, "100000"),
	}

	dec := e.CheckOrder(p, ps)
	if dec.Approved {
		t.Fatal("short-opening sell over gross budget must still be rejected")
	}
	if dec.Limit != LimitGrossLeverage {
		t.Fatalf("want LimitGrossLeverage, got %s (%s)", dec.Limit, dec.Reason)
	}
}

func TestCheckOrder_FlipAcrossFlatCountsOnlyExcessExposure(t *testing.T) {
	e := NewEngine(Config{
		MaxGrossLeverage: d(t, "1"),
		RequireStop:      false,
	})
	ps := PortfolioState{
		Equity:            d(t, "100000"),
		Cash:              d(t, "0"),
		OpenPositionCount: -1,
		Positions: map[string]HeldPosition{
			"AAPL": {Qty: d(t, "100"), MarkPx: d(t, "100")},
		},
	}
	p := OrderProposal{
		Symbol:        "AAPL",
		Side:          orders.SideSell,
		Qty:           d(t, "150"),
		EntryPx:       d(t, "100"),
		AccountEquity: d(t, "100000"),
	}

	dec := e.CheckOrder(p, ps)
	if !dec.Approved {
		t.Fatalf("flip should count only excess short exposure, got %s: %s", dec.Limit, dec.Reason)
	}
}

func TestCheckOrder_ReducingOrderUsesSuppliedGrossFastPath(t *testing.T) {
	e := NewEngine(Config{
		MaxGrossLeverage: d(t, "1"),
		RequireStop:      false,
	})
	ps := PortfolioState{
		Equity:            d(t, "100000"),
		Cash:              d(t, "0"),
		GrossExposure:     pd(t, "120000"),
		OpenPositionCount: -1,
		Positions: map[string]HeldPosition{
			"AAPL": {Qty: d(t, "1200"), MarkPx: d(t, "100")},
		},
	}
	p := OrderProposal{
		Symbol:        "AAPL",
		Side:          orders.SideSell,
		Qty:           d(t, "300"),
		EntryPx:       d(t, "100"),
		AccountEquity: d(t, "100000"),
	}

	dec := e.CheckOrder(p, ps)
	if !dec.Approved {
		t.Fatalf("supplied gross fast path should still reduce held symbol exposure, got %s: %s", dec.Limit, dec.Reason)
	}
}

func TestCheckOrder_SameDirectionShortAddStillIncreasesExposure(t *testing.T) {
	e := NewEngine(Config{
		MaxGrossLeverage: d(t, "1"),
		RequireStop:      false,
	})
	ps := PortfolioState{
		Equity:            d(t, "100000"),
		Cash:              d(t, "0"),
		OpenPositionCount: -1,
		Positions: map[string]HeldPosition{
			"AAPL": {Qty: d(t, "-900"), MarkPx: d(t, "100")},
		},
	}
	p := OrderProposal{
		Symbol:        "AAPL",
		Side:          orders.SideSell,
		Qty:           d(t, "200"),
		EntryPx:       d(t, "100"),
		AccountEquity: d(t, "100000"),
	}

	dec := e.CheckOrder(p, ps)
	if dec.Approved {
		t.Fatal("same-direction short add over gross budget must still be rejected")
	}
	if dec.Limit != LimitGrossLeverage {
		t.Fatalf("want LimitGrossLeverage, got %s (%s)", dec.Limit, dec.Reason)
	}
}

// TestCheckOrder_SellStopSide exercises the SHORT branch of stopOnProtectiveSide:
// for a SideSell the protective stop is ABOVE entry (price rising is the loss),
// so a stop BELOW entry is wrong-side and must reject with LimitMissingStop,
// while a stop ABOVE entry is correct and must pass the stop check.
func TestCheckOrder_SellStopSide(t *testing.T) {
	// Only the stop check matters here; keep all other limits comfortably clear.
	cfg := Config{
		MaxRiskPerTradePct: d(t, "0.01"),
		RequireStop:        true,
	}
	e := NewEngine(cfg)
	ps := PortfolioState{Equity: d(t, "100000"), OpenPositionCount: -1, Positions: map[string]HeldPosition{}}

	t.Run("sell with stop BELOW entry is wrong-side -> rejected", func(t *testing.T) {
		// Short at 50 with a stop at 49 (below). For a short, price FALLING is
		// profit, so a stop below entry is not protective -> LimitMissingStop.
		p := OrderProposal{
			Symbol: "AAPL", Side: orders.SideSell,
			Qty: d(t, "10"), EntryPx: d(t, "50"), StopPx: d(t, "49"),
			AccountEquity: d(t, "100000"),
		}
		dec := e.CheckOrder(p, ps)
		if dec.Approved {
			t.Fatal("sell with stop below entry (wrong side) must be rejected")
		}
		if dec.Limit != LimitMissingStop {
			t.Fatalf("want LimitMissingStop for wrong-side short stop, got %s (%s)", dec.Limit, dec.Reason)
		}
	})

	t.Run("sell with stop ABOVE entry is protective -> approved", func(t *testing.T) {
		// Short at 50 with a stop at 51 (above). Correct protective side; risk =
		// 10*|50-51| = 10 <= 1% of 100000 = 1000, and no other limit set -> approve.
		p := OrderProposal{
			Symbol: "AAPL", Side: orders.SideSell,
			Qty: d(t, "10"), EntryPx: d(t, "50"), StopPx: d(t, "51"),
			AccountEquity: d(t, "100000"),
		}
		dec := e.CheckOrder(p, ps)
		if !dec.Approved {
			t.Fatalf("sell with stop above entry (protective) must pass the stop check, got %s (%s)", dec.Limit, dec.Reason)
		}
		if dec.Limit != LimitNone {
			t.Fatalf("approved short should carry LimitNone, got %s", dec.Limit)
		}
	})
}

// TestDrawdownBreached covers the BUG-1 enforcement primitive: the drawdown limit
// is carried into risk.Config and checked exactly. The trade loop (slice 8) calls
// this each tick to halt via the kill switch; here we prove the mechanism is real,
// decimal-exact, and disabled when configured off.
func TestDrawdownBreached(t *testing.T) {
	// 20% max drawdown. Peak 10000 -> breach at/under 8000.
	e := NewEngine(Config{MaxDrawdownPct: d(t, "0.20")})

	// Under the limit: 10% drawdown (9000) is NOT a breach.
	if e.DrawdownBreached(d(t, "10000"), d(t, "9000")) {
		t.Error("10% drawdown is under the 20% limit -> must NOT breach")
	}
	// Exactly at the limit: 20% drawdown (8000) IS a breach (>=).
	if !e.DrawdownBreached(d(t, "10000"), d(t, "8000")) {
		t.Error("exactly 20% drawdown must breach (>= boundary)")
	}
	// Over the limit: 30% drawdown (7000) IS a breach.
	if !e.DrawdownBreached(d(t, "10000"), d(t, "7000")) {
		t.Error("30% drawdown is over the 20% limit -> must breach")
	}
	// No drawdown (current == peak) is never a breach.
	if e.DrawdownBreached(d(t, "10000"), d(t, "10000")) {
		t.Error("no drawdown (current == peak) must not breach")
	}
	// Current above peak (a new high) is never a breach.
	if e.DrawdownBreached(d(t, "10000"), d(t, "11000")) {
		t.Error("current above peak must not breach")
	}
}

// TestDrawdownBreached_Disabled proves MaxDrawdownPct <= 0 disables the halt:
// even a total wipeout never reports a breach when the limit is off.
func TestDrawdownBreached_Disabled(t *testing.T) {
	e := NewEngine(Config{MaxDrawdownPct: orders.ZeroDecimal}) // 0 == disabled
	if e.DrawdownBreached(d(t, "10000"), d(t, "1")) {
		t.Error("MaxDrawdownPct=0 must disable the drawdown halt (never breach)")
	}
	// A negative pct is also treated as disabled.
	en := NewEngine(Config{MaxDrawdownPct: d(t, "-0.5")})
	if en.DrawdownBreached(d(t, "10000"), d(t, "1")) {
		t.Error("negative MaxDrawdownPct must disable the drawdown halt")
	}
}

// TestDrawdownBreached_PeakGuard proves a non-positive peak equity is a meaningless
// baseline and never reports a breach (no divide-by-zero, no false halt).
func TestDrawdownBreached_PeakGuard(t *testing.T) {
	e := NewEngine(Config{MaxDrawdownPct: d(t, "0.20")})
	if e.DrawdownBreached(orders.ZeroDecimal, d(t, "1")) {
		t.Error("zero peak equity must not breach (no baseline)")
	}
	if e.DrawdownBreached(d(t, "-100"), d(t, "-200")) {
		t.Error("negative peak equity must not breach (no baseline)")
	}
}

// TestDrawdownBreached_ExactDecimalBoundary proves the boundary is decimal-exact:
// a peak/current pair that lands one cent ABOVE the threshold does not breach, and
// one cent AT the threshold does. With peak 12345.67 and a 33% limit the threshold
// drop is exactly 4074.0711; current = peak - 4074.0711 = 8271.5989 breaches, one
// cent richer does not.
func TestDrawdownBreached_ExactDecimalBoundary(t *testing.T) {
	e := NewEngine(Config{MaxDrawdownPct: d(t, "0.33")})
	peak := d(t, "12345.67")
	// drop threshold = 0.33 * 12345.67 = 4074.0711  -> current at exactly threshold
	atThreshold := d(t, "8271.5989") // 12345.67 - 4074.0711
	if !e.DrawdownBreached(peak, atThreshold) {
		t.Error("current exactly at the decimal threshold must breach (>=)")
	}
	justAbove := d(t, "8271.6089") // one cent less drawdown -> just under the limit
	if e.DrawdownBreached(peak, justAbove) {
		t.Error("one cent under the threshold drawdown must NOT breach")
	}
}
