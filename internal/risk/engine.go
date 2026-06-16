// Package risk is BUCKS's pre-trade gate and circuit-breaker layer. It answers
// one question before any order reaches a broker: "is placing THIS order, given
// the CURRENT portfolio, still inside every limit the operator set?" and it owns
// the durable kill switch that halts trading on a breach.
//
// Three pieces live here, all deterministic and float64-free:
//
//   - Engine    — a pure pre-trade risk check. Every money value is orders.Decimal;
//     every limit (per-trade risk, daily loss, leverage, exposure, concentration,
//     open-position count, mandatory stop) is enforced with exact decimal math.
//   - KillSwitch — a two-layer, durable, fail-safe HALTED flag (app-side here; a
//     BrokerKillSwitch hook is the broker-native second layer). It NEVER auto-
//     resumes into a breach after a restart and only Clear() (an explicit operator
//     action) lifts a halt.
//   - TokenBucket + Backoff — per-broker rate limiting and full-jitter retry delay,
//     both clock-injected so tests are deterministic.
//
// Nothing here reads the wall clock or a global RNG directly: the Engine is a pure
// function of its inputs, and the kill switch / token bucket / backoff take an
// injected now func and an injected jitter seed. That is what makes the whole
// package reproducible under test.
package risk

import (
	"fmt"

	"bucks/internal/orders"
)

// Decimal is re-exported from the orders package so risk code (and its callers)
// use one money type end-to-end. Never float64 in any money/risk math.
type Decimal = orders.Decimal

// Limit identifies which risk control rejected an order. It is the machine-
// readable companion to Decision.Reason so callers (and tests) can branch on the
// exact control that tripped rather than parsing prose.
type Limit int

const (
	// LimitNone is the zero value: nothing tripped (the order is approved).
	LimitNone Limit = iota
	// LimitMissingStop fires when RequireStop is set and the proposal has no
	// valid protective stop (mandatory ATR stop — no naked positions).
	LimitMissingStop
	// LimitPerTradeRisk fires when qty*|entryPx-stopPx| exceeds
	// MaxRiskPerTradePct * equity.
	LimitPerTradeRisk
	// LimitDailyLoss fires when realized loss today has already reached
	// MaxDailyLossPct * equity — trading halts for the day.
	LimitDailyLoss
	// LimitGrossLeverage fires when adding this order would push gross exposure /
	// equity above MaxGrossLeverage.
	LimitGrossLeverage
	// LimitTotalExposure fires when adding this order would push gross exposure
	// above MaxTotalExposure (an absolute money cap).
	LimitTotalExposure
	// LimitConcentration fires when this symbol's post-order exposure would exceed
	// MaxConcentrationPct * equity.
	LimitConcentration
	// LimitMaxOpenPositions fires when opening a NEW symbol would exceed
	// MaxOpenPositions.
	LimitMaxOpenPositions
	// LimitInvalidInput fires on a malformed proposal (non-positive qty/price,
	// non-positive equity) — rejected rather than silently approved.
	LimitInvalidInput
)

// String renders the tripped limit for logs, reports, and tests.
func (l Limit) String() string {
	switch l {
	case LimitNone:
		return "None"
	case LimitMissingStop:
		return "MissingStop"
	case LimitPerTradeRisk:
		return "PerTradeRisk"
	case LimitDailyLoss:
		return "DailyLoss"
	case LimitGrossLeverage:
		return "GrossLeverage"
	case LimitTotalExposure:
		return "TotalExposure"
	case LimitConcentration:
		return "Concentration"
	case LimitMaxOpenPositions:
		return "MaxOpenPositions"
	case LimitInvalidInput:
		return "InvalidInput"
	default:
		return fmt.Sprintf("Limit(%d)", int(l))
	}
}

// hardCapRiskPerTradePct is the absolute ceiling on MaxRiskPerTradePct. The spec
// sets ≤1% risk/trade with a hard cap of 2%: even if a config asks for more, the
// Engine clamps the effective per-trade risk budget to this value. Stored as a
// string literal parsed once at construction so there is no float64 anywhere.
const hardCapRiskPerTradePct = "0.02"

// defaultRiskPerTradePct is the default per-trade risk budget (1%).
const defaultRiskPerTradePct = "0.01"

// Config holds every risk limit. Percentage fields are fractions (0.01 == 1%);
// money fields are absolute amounts. All are orders.Decimal — never float64.
//
// A zero-value field means "limit disabled" for the optional caps
// (MaxGrossLeverage, MaxTotalExposure, MaxConcentrationPct, MaxOpenPositions,
// MaxDailyLossPct); the per-trade risk budget and RequireStop always apply.
type Config struct {
	// MaxRiskPerTradePct is the fraction of equity a single trade may risk
	// (qty * |entry-stop|). Defaults to 0.01; clamped to the 0.02 hard cap.
	MaxRiskPerTradePct Decimal
	// MaxDailyLossPct halts trading for the day once realized loss reaches this
	// fraction of equity. Zero disables the daily-loss halt.
	MaxDailyLossPct Decimal
	// MaxGrossLeverage caps gross exposure / equity (e.g. 2 or 3). Zero disables.
	MaxGrossLeverage Decimal
	// MaxTotalExposure caps absolute gross exposure (money). Zero disables.
	MaxTotalExposure Decimal
	// MaxConcentrationPct caps a single symbol's exposure as a fraction of equity.
	// Zero disables.
	MaxConcentrationPct Decimal
	// MaxOpenPositions caps the number of distinct open-position symbols. Zero
	// disables (no cap).
	MaxOpenPositions int
	// MaxDrawdownPct is the worst peak-to-trough equity loss the operator will
	// tolerate, as a fraction of peak equity (0.20 == 20%). It is carried here from
	// the owner's Playbook and enforced via DrawdownBreached, which the trade loop
	// calls each tick to halt via the kill switch (the per-tick wiring lands in
	// slice 8). Zero (or negative) disables the drawdown halt.
	MaxDrawdownPct Decimal
	// RequireStop, when true (the default), rejects any proposal without a valid
	// protective stop. No naked positions.
	RequireStop bool
}

// Engine is the pure pre-trade risk gate. It is stateless and safe for concurrent
// use: CheckOrder reads only its config and the arguments passed in. The portfolio
// state is supplied by the caller (who owns it) rather than held here, which keeps
// the Engine a deterministic function of (config, proposal, state).
type Engine struct {
	cfg        Config
	maxRiskPct Decimal // effective, post-default, post-clamp per-trade risk pct
}

// NewEngine builds an Engine, applying the per-trade-risk default (1%) and the
// 2% hard cap, and defaulting RequireStop to true unless the caller explicitly
// built a Config with it set. Because Go zero-values a bool to false, callers
// MUST set RequireStop: true to require stops; NewEngine does NOT force it on, so
// that an operator who deliberately disables it can — but the default Config the
// rest of BUCKS constructs (via DefaultConfig) turns it on.
func NewEngine(cfg Config) *Engine {
	hardCap := orders.MustParseDecimal(hardCapRiskPerTradePct)
	maxRiskPct := cfg.MaxRiskPerTradePct
	if maxRiskPct.Sign() <= 0 {
		maxRiskPct = orders.MustParseDecimal(defaultRiskPerTradePct)
	}
	// Clamp to the hard cap: never let a config risk more than 2% per trade.
	if maxRiskPct.Cmp(hardCap) > 0 {
		maxRiskPct = hardCap
	}
	return &Engine{
		cfg:        cfg,
		maxRiskPct: maxRiskPct,
	}
}

// DefaultConfig returns the operator-default risk config: 1% per-trade risk, 3%
// daily-loss halt, 2x gross leverage, mandatory stop on. Money caps that need a
// concrete account size (MaxTotalExposure) are left zero (disabled) for the
// caller to set; MaxConcentrationPct defaults to 25% and MaxOpenPositions to 10.
func DefaultConfig() Config {
	return Config{
		MaxRiskPerTradePct:  orders.MustParseDecimal("0.01"),
		MaxDailyLossPct:     orders.MustParseDecimal("0.03"),
		MaxGrossLeverage:    orders.MustParseDecimal("2"),
		MaxConcentrationPct: orders.MustParseDecimal("0.25"),
		MaxOpenPositions:    10,
		RequireStop:         true,
	}
}

// EffectiveMaxRiskPerTradePct exposes the post-default, post-clamp per-trade risk
// fraction the Engine actually enforces (for reports/telemetry and tests).
func (e *Engine) EffectiveMaxRiskPerTradePct() Decimal { return e.maxRiskPct }

// DrawdownBreached reports whether the account's peak-to-current equity loss has
// reached or exceeded the configured MaxDrawdownPct — the signal the trade loop
// (slice 8) checks each tick to halt trading via the kill switch.
//
// It returns true when (peakEquity-currentEquity)/peakEquity >= MaxDrawdownPct,
// computed by cross-multiplication ((peak-current) >= MaxDrawdownPct*peak) so the
// comparison is decimal-exact with no divide. Guards:
//   - MaxDrawdownPct <= 0 means the drawdown halt is DISABLED -> always false.
//   - peakEquity <= 0 is a meaningless baseline (no real peak to measure against)
//     -> false; the caller supplies the live running peak (a positive number).
//
// A current equity at or above the peak (no drawdown) is never a breach. On any
// internal decimal error it fails SAFE for a halt signal by returning false only
// when the math could not be completed — but with valid positive inputs the math
// cannot error, so this is a defensive guard, not an expected path.
func (e *Engine) DrawdownBreached(peakEquity, currentEquity Decimal) bool {
	if e.cfg.MaxDrawdownPct.Sign() <= 0 {
		return false // drawdown halt disabled
	}
	if peakEquity.Sign() <= 0 {
		return false // no real peak baseline to measure against
	}
	drop, err := peakEquity.Sub(currentEquity)
	if err != nil {
		return false
	}
	if drop.Sign() <= 0 {
		return false // current at/above peak -> no drawdown
	}
	threshold, err := e.cfg.MaxDrawdownPct.Mul(peakEquity)
	if err != nil {
		return false
	}
	// Breach when the drop reaches or exceeds MaxDrawdownPct of the peak.
	return drop.Cmp(threshold) >= 0
}

// OrderProposal is one intent to evaluate. All money is orders.Decimal.
//
// Qty is always POSITIVE (the side carries direction). EntryPx is the expected
// entry price. StopPx is the protective (ATR) stop price; for RequireStop it must
// be a valid price on the protective side. AccountEquity is the account equity
// the per-trade risk budget is measured against (the caller passes the live value
// so the Engine stays stateless).
type OrderProposal struct {
	Symbol        string
	Side          orders.Side
	Qty           Decimal
	EntryPx       Decimal
	StopPx        Decimal
	AccountEquity Decimal
}

// HeldPosition is one open position in the portfolio snapshot: signed quantity
// (+ long / - short) and the price its exposure is marked at (typically the last
// or entry price).
type HeldPosition struct {
	Qty    Decimal // signed: + long / - short
	MarkPx Decimal
}

// PortfolioState is the live portfolio snapshot the Engine checks the proposal
// against. The caller owns and supplies it (the Engine never mutates it).
//
//   - Equity / Cash are the account totals.
//   - Positions maps symbol -> held position (signed qty + mark price).
//   - RealizedPnLToday is today's realized P&L (NEGATIVE for a loss). The daily-
//     loss check compares the loss magnitude against MaxDailyLossPct * Equity.
//   - GrossExposure is an OPTIONAL fast-path for the current absolute gross
//     exposure (sum of |qty*px|): nil means "not supplied" (the Engine derives it
//     from Positions), a non-nil pointer means "use this exact value" (even if it
//     points at zero — a legitimately flat book). This explicit nil sentinel
//     avoids conflating "not supplied" with "exposure happens to be zero".
//   - OpenPositionCount is an OPTIONAL fast-path for the number of distinct open
//     symbols: a NEGATIVE value (use -1) means "not supplied" (the Engine counts
//     Positions); a value >= 0 is used as-is (0 meaning a legitimately empty
//     book). This explicit -1 sentinel avoids conflating "not supplied" with a
//     real count of zero.
//
// IMPORTANT — fast-path contract: even when GrossExposure / OpenPositionCount are
// supplied, Positions MUST still faithfully contain the open positions. The
// per-symbol concentration check and the add-to-existing (alreadyHeld) check read
// Positions directly and are NOT derivable from the aggregate fast-paths; an
// inconsistent Positions map with a supplied fast-path will mis-classify those
// checks. The fast-paths only short-circuit the aggregate gross/count derivation.
type PortfolioState struct {
	Equity           Decimal
	Cash             Decimal
	Positions        map[string]HeldPosition
	RealizedPnLToday Decimal
	// GrossExposure: nil = not supplied (derive from Positions); non-nil = use it.
	GrossExposure *Decimal
	// OpenPositionCount: <0 (use -1) = not supplied (count Positions); >=0 = use it.
	OpenPositionCount int
}

// Decision is the Engine's verdict. Approved is the gate; Limit is the exact
// control that tripped (LimitNone when approved); Reason is plain-English detail.
type Decision struct {
	Approved bool
	Limit    Limit
	Reason   string
}

func approve() Decision {
	return Decision{Approved: true, Limit: LimitNone, Reason: "within all limits"}
}

func reject(l Limit, format string, args ...any) Decision {
	return Decision{Approved: false, Limit: l, Reason: fmt.Sprintf(format, args...)}
}

// CheckOrder evaluates a proposal against the current portfolio and config,
// enforcing EVERY limit with exact decimal math. The first limit that trips wins;
// the order is rejected with that Limit. If all pass, the order is approved.
//
// The check order is deliberate: input validity → mandatory stop → daily-loss
// halt (a portfolio-wide breaker that should reject before per-order sizing) →
// per-trade risk → leverage / total exposure / concentration / open-position
// count. Each comparison is decimal-exact; nothing rounds through float64.
func (e *Engine) CheckOrder(p OrderProposal, ps PortfolioState) Decision {
	// 0. Input validity — never silently approve a malformed proposal.
	if p.Qty.Sign() <= 0 {
		return reject(LimitInvalidInput, "qty must be positive, got %s", p.Qty.String())
	}
	if p.EntryPx.Sign() <= 0 {
		return reject(LimitInvalidInput, "entry price must be positive, got %s", p.EntryPx.String())
	}
	if ps.Equity.Sign() <= 0 {
		return reject(LimitInvalidInput, "account equity must be positive, got %s", ps.Equity.String())
	}

	// 1. Mandatory protective stop (no naked positions). A stop is valid if it is
	//    a positive price strictly on the protective side: BELOW entry for a long,
	//    ABOVE entry for a short. A zero/invalid/wrong-side stop is a hard reject.
	if e.cfg.RequireStop {
		if p.StopPx.Sign() <= 0 {
			return reject(LimitMissingStop, "RequireStop: missing/invalid stop (got %s)", p.StopPx.String())
		}
		if !stopOnProtectiveSide(p.Side, p.EntryPx, p.StopPx) {
			return reject(LimitMissingStop,
				"RequireStop: stop %s is not on the protective side of entry %s for a %s",
				p.StopPx.String(), p.EntryPx.String(), p.Side)
		}
	}

	// 2. Daily-loss halt: if today's realized LOSS already meets/exceeds the
	//    budget, halt — reject before doing anything else portfolio-changing.
	if e.cfg.MaxDailyLossPct.Sign() > 0 && ps.RealizedPnLToday.Sign() < 0 {
		lossBudget, err := e.cfg.MaxDailyLossPct.Mul(ps.Equity)
		if err != nil {
			return reject(LimitInvalidInput, "daily-loss budget compute: %v", err)
		}
		lossSoFar := ps.RealizedPnLToday.Abs()
		if lossSoFar.Cmp(lossBudget) >= 0 {
			return reject(LimitDailyLoss,
				"daily loss %s reached budget %s (%s of equity %s) — halt for the day",
				lossSoFar.String(), lossBudget.String(),
				e.cfg.MaxDailyLossPct.String(), ps.Equity.String())
		}
	}

	// 3. Per-trade risk: qty * |entry - stop| must be <= MaxRiskPerTradePct*equity.
	//    Only meaningful when a stop is present; if RequireStop is off and there is
	//    no stop, there is no defined per-trade risk to bound, so this check is
	//    skipped (the stop check above already rejects naked positions when on).
	if p.StopPx.Sign() > 0 {
		perTradeRisk, err := perTradeRiskAmount(p.Qty, p.EntryPx, p.StopPx)
		if err != nil {
			return reject(LimitInvalidInput, "per-trade risk compute: %v", err)
		}
		riskBudget, err := e.maxRiskPct.Mul(ps.Equity)
		if err != nil {
			return reject(LimitInvalidInput, "risk budget compute: %v", err)
		}
		if perTradeRisk.Cmp(riskBudget) > 0 {
			return reject(LimitPerTradeRisk,
				"per-trade risk %s exceeds budget %s (%s of equity %s)",
				perTradeRisk.String(), riskBudget.String(),
				e.maxRiskPct.String(), ps.Equity.String())
		}
	}

	// Compute the notional this order adds to gross exposure (qty * entry). Used
	// by leverage / total-exposure / concentration.
	orderNotional, err := p.Qty.Mul(p.EntryPx)
	if err != nil {
		return reject(LimitInvalidInput, "order notional compute: %v", err)
	}

	// Current gross exposure: use the caller-supplied fast-path when non-nil
	// (even if it is a legitimate zero), else derive it from Positions. The nil
	// sentinel keeps "not supplied" distinct from "exposure is exactly zero".
	// Derivation is decimal-exact.
	var curGross Decimal
	if ps.GrossExposure != nil {
		curGross = *ps.GrossExposure
	} else {
		curGross, err = grossExposureOf(ps.Positions)
		if err != nil {
			return reject(LimitInvalidInput, "gross exposure compute: %v", err)
		}
	}
	newGross, err := curGross.Add(orderNotional)
	if err != nil {
		return reject(LimitInvalidInput, "new gross exposure compute: %v", err)
	}

	// 4. Gross leverage: newGross / equity must be <= MaxGrossLeverage. Compare
	//    via cross-multiplication (newGross <= leverage*equity) to avoid a divide.
	if e.cfg.MaxGrossLeverage.Sign() > 0 {
		levBudget, err := e.cfg.MaxGrossLeverage.Mul(ps.Equity)
		if err != nil {
			return reject(LimitInvalidInput, "leverage budget compute: %v", err)
		}
		if newGross.Cmp(levBudget) > 0 {
			return reject(LimitGrossLeverage,
				"post-order gross %s exceeds %sx leverage budget %s on equity %s",
				newGross.String(), e.cfg.MaxGrossLeverage.String(),
				levBudget.String(), ps.Equity.String())
		}
	}

	// 5. Total exposure (absolute money cap).
	if e.cfg.MaxTotalExposure.Sign() > 0 && newGross.Cmp(e.cfg.MaxTotalExposure) > 0 {
		return reject(LimitTotalExposure,
			"post-order gross %s exceeds max total exposure %s",
			newGross.String(), e.cfg.MaxTotalExposure.String())
	}

	// 6. Per-symbol concentration: this symbol's post-order exposure must be
	//    <= MaxConcentrationPct * equity.
	if e.cfg.MaxConcentrationPct.Sign() > 0 {
		symGross, err := symbolExposureOf(ps.Positions, p.Symbol)
		if err != nil {
			return reject(LimitInvalidInput, "symbol exposure compute: %v", err)
		}
		newSymGross, err := symGross.Add(orderNotional)
		if err != nil {
			return reject(LimitInvalidInput, "new symbol exposure compute: %v", err)
		}
		concBudget, err := e.cfg.MaxConcentrationPct.Mul(ps.Equity)
		if err != nil {
			return reject(LimitInvalidInput, "concentration budget compute: %v", err)
		}
		if newSymGross.Cmp(concBudget) > 0 {
			return reject(LimitConcentration,
				"post-order %s exposure %s exceeds concentration budget %s (%s of equity %s)",
				p.Symbol, newSymGross.String(), concBudget.String(),
				e.cfg.MaxConcentrationPct.String(), ps.Equity.String())
		}
	}

	// 7. Max open positions: only NEW symbols (not already held) increase the
	//    count. An add to an existing position does not open a new slot.
	if e.cfg.MaxOpenPositions > 0 {
		// Use the caller-supplied count when it is >= 0 (0 meaning a legitimately
		// empty book), else derive it from Positions. The negative sentinel keeps
		// "not supplied" distinct from "count is exactly zero".
		var openCount int
		if ps.OpenPositionCount >= 0 {
			openCount = ps.OpenPositionCount
		} else {
			openCount = countOpen(ps.Positions)
		}
		_, alreadyHeld := ps.Positions[p.Symbol]
		if !alreadyHeld {
			if openCount+1 > e.cfg.MaxOpenPositions {
				return reject(LimitMaxOpenPositions,
					"opening %s would make %d open positions, exceeding max %d",
					p.Symbol, openCount+1, e.cfg.MaxOpenPositions)
			}
		}
	}

	return approve()
}

// stopOnProtectiveSide reports whether stop is a valid protective stop for side
// at the given entry: strictly below entry for a long (buy), strictly above for a
// short (sell). Decimal-exact comparison.
func stopOnProtectiveSide(side orders.Side, entry, stop Decimal) bool {
	switch side {
	case orders.SideBuy:
		return stop.Cmp(entry) < 0
	case orders.SideSell:
		return stop.Cmp(entry) > 0
	default:
		return false
	}
}

// perTradeRiskAmount computes qty * |entry - stop| in exact decimal.
func perTradeRiskAmount(qty, entry, stop Decimal) (Decimal, error) {
	diff, err := entry.Sub(stop)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	dist := diff.Abs()
	return qty.Mul(dist)
}

// grossExposureOf sums |qty * markPx| across all positions, exact decimal.
func grossExposureOf(positions map[string]HeldPosition) (Decimal, error) {
	total := orders.ZeroDecimal
	for _, pos := range positions {
		notional, err := pos.Qty.Mul(pos.MarkPx)
		if err != nil {
			return orders.ZeroDecimal, err
		}
		total, err = total.Add(notional.Abs())
		if err != nil {
			return orders.ZeroDecimal, err
		}
	}
	return total, nil
}

// symbolExposureOf returns |qty * markPx| for one symbol (zero if not held).
func symbolExposureOf(positions map[string]HeldPosition, symbol string) (Decimal, error) {
	pos, ok := positions[symbol]
	if !ok {
		return orders.ZeroDecimal, nil
	}
	notional, err := pos.Qty.Mul(pos.MarkPx)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	return notional.Abs(), nil
}

// countOpen counts positions with a non-zero quantity.
func countOpen(positions map[string]HeldPosition) int {
	n := 0
	for _, pos := range positions {
		if !pos.Qty.IsZero() {
			n++
		}
	}
	return n
}
