// Package playbook captures the OWNER's trading profile — the brain's mandate —
// and binds it to the safety layer. A Playbook is the typed, YAML-serializable
// record of how its owner wants BUCKS to trade (risk tolerance, capital, style,
// sectors, max drawdown, goals) plus the concrete risk knobs that map onto
// risk.Config. An Intake drives a fixed set of leading questions and, from the
// owner's answers, builds and validates a Playbook — rejecting anything
// incomplete or self-contradictory with a clear, plain-English reason.
//
// Money is always orders.Decimal (never float64): Capital and the drawdown /
// risk fractions cross into risk math, so they stay exact end to end.
package playbook

import (
	"fmt"
	"strings"

	"bucks/internal/orders"
	"bucks/internal/risk"
)

// RiskTolerance is how much volatility / drawdown the owner is willing to stomach.
// It is the qualitative knob; the quantitative caps live on the Playbook itself.
type RiskTolerance string

const (
	// Conservative — capital preservation first; small per-trade risk, tight caps.
	Conservative RiskTolerance = "conservative"
	// Moderate — balanced growth and protection (the default register).
	Moderate RiskTolerance = "moderate"
	// Aggressive — growth-seeking, accepts larger swings (still inside hard caps).
	Aggressive RiskTolerance = "aggressive"
)

// validTolerance reports whether t is one of the three known registers.
func (t RiskTolerance) valid() bool {
	switch t {
	case Conservative, Moderate, Aggressive:
		return true
	default:
		return false
	}
}

// Style is the holding-horizon the owner trades on.
type Style string

const (
	// Scalp — very short holds, many small trades.
	Scalp Style = "scalp"
	// Swing — multi-day to multi-week holds.
	Swing Style = "swing"
	// Hodl — long-term buy-and-hold; wide drawdown tolerance by nature.
	Hodl Style = "hodl"
)

// valid reports whether s is one of the three known styles.
func (s Style) valid() bool {
	switch s {
	case Scalp, Swing, Hodl:
		return true
	default:
		return false
	}
}

// normalizeStyle maps an owner's raw style answer to the canonical Style. "hold" is the
// plain-English label the owner sees and types; it canonicalizes to Hodl (long-term
// buy-and-hold). The legacy "hodl" spelling is still accepted as a hidden alias so older
// saved configs and prior answers keep working, but the owner is only ever shown "hold".
// Any other value passes through lowercased so Validate can reject it with a clear message.
func normalizeStyle(raw string) Style {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hold", "hodl":
		return Hodl
	case string(Scalp):
		return Scalp
	case string(Swing):
		return Swing
	default:
		return Style(strings.ToLower(strings.TrimSpace(raw)))
	}
}

// Playbook is the owner's trading profile. It is YAML-serializable (gopkg.in/
// yaml.v3) so it can be written to and re-read from the config on disk across the
// zip→ship→unpack round trip. Every money / fraction field is orders.Decimal and
// marshals as its decimal string (govalues/decimal implements TextMarshaler), so
// the YAML round trip is exact — no float64 ever touches a price or a percentage.
type Playbook struct {
	// RiskTolerance is the qualitative register (conservative|moderate|aggressive).
	RiskTolerance RiskTolerance `yaml:"risk_tolerance"`
	// Capital is the account capital the owner is funding BUCKS with (> 0).
	Capital orders.Decimal `yaml:"capital"`
	// Style is the holding horizon (scalp|swing|hodl).
	Style Style `yaml:"style"`
	// Sectors are the markets/sectors the owner wants traded (free-form tags).
	Sectors []string `yaml:"sectors,omitempty"`
	// MaxDrawdownPct is the worst peak-to-trough loss the owner will tolerate,
	// as a fraction in (0,1] (0.20 == 20%).
	MaxDrawdownPct orders.Decimal `yaml:"max_drawdown_pct"`
	// Goals is the owner's plain-English objective (free text).
	Goals string `yaml:"goals,omitempty"`

	// --- risk knobs that map onto risk.Config (the brain→safety binding) ---

	// MaxRiskPerTradePct is the fraction of capital a single trade may risk.
	// Defaults from RiskTolerance when the owner leaves it unset; clamped by the
	// risk engine's 2% hard cap regardless.
	MaxRiskPerTradePct orders.Decimal `yaml:"max_risk_per_trade_pct"`
	// MaxDailyLossPct halts trading for the day at this fraction of capital.
	MaxDailyLossPct orders.Decimal `yaml:"max_daily_loss_pct"`
	// MaxGrossLeverage caps gross exposure / equity (e.g. 1, 2, 3).
	MaxGrossLeverage orders.Decimal `yaml:"max_gross_leverage"`
	// MaxOpenPositions caps the number of distinct open-position symbols.
	MaxOpenPositions int `yaml:"max_open_positions"`
}

// one is the Decimal constant 1, used as the inclusive upper bound on fractions.
var one = orders.MustParseDecimal("1")

// ToRiskConfig maps the playbook onto the risk engine's Config — the single point
// where the owner's mandate becomes enforceable safety limits. RequireStop is
// always true (no naked positions, per spec §4.5); per-symbol concentration is
// derived from tolerance. The owner's MaxDrawdownPct is carried into risk.Config
// where the engine enforces it via DrawdownBreached (the trade loop wires the
// per-tick equity halt in slice 8) — so the drawdown tolerance is a real, mapped,
// enforceable limit, not a value collected and discarded. The risk engine still
// clamps the per-trade risk to its own 2% hard cap, so a playbook can never widen
// risk past the invariant.
func (p Playbook) ToRiskConfig() risk.Config {
	cfg := risk.Config{
		MaxRiskPerTradePct: p.MaxRiskPerTradePct,
		MaxDailyLossPct:    p.MaxDailyLossPct,
		MaxGrossLeverage:   p.MaxGrossLeverage,
		MaxOpenPositions:   p.MaxOpenPositions,
		MaxDrawdownPct:     p.MaxDrawdownPct,
		RequireStop:        true,
	}
	// Concentration cap follows the register: a conservative owner spreads thin,
	// an aggressive one concentrates more. These are fractions of equity.
	switch p.RiskTolerance {
	case Conservative:
		cfg.MaxConcentrationPct = orders.MustParseDecimal("0.15")
	case Aggressive:
		cfg.MaxConcentrationPct = orders.MustParseDecimal("0.35")
	default: // Moderate (and any already-validated value)
		cfg.MaxConcentrationPct = orders.MustParseDecimal("0.25")
	}
	return cfg
}

// Validate checks a Playbook for completeness and internal consistency, returning
// a clear, specific error on the FIRST problem found (never a silent pass). It is
// the single source of truth for "is this a usable mandate?" and is called by
// BuildPlaybook after assembling answers.
//
// Rules enforced:
//   - RiskTolerance is one of the three registers.
//   - Style is one of the three horizons.
//   - Capital > 0.
//   - MaxDrawdownPct in (0, 1].
//   - MaxRiskPerTradePct in (0, 1] (the risk engine clamps the upper cap further).
//   - MaxDailyLossPct in (0, 1] and >= MaxRiskPerTradePct (a daily-loss budget
//     smaller than a single trade's risk is contradictory).
//   - MaxGrossLeverage >= 1.
//   - MaxOpenPositions >= 1.
//   - Contradiction guard: a HODL style with a tiny MaxDrawdownPct (< 10%) is
//     self-contradictory — long-term holding inherently rides out larger
//     drawdowns — and is flagged rather than silently accepted.
func (p Playbook) Validate() error {
	if !p.RiskTolerance.valid() {
		return fmt.Errorf("playbook: risk_tolerance %q is invalid (want conservative|moderate|aggressive)", p.RiskTolerance)
	}
	if !p.Style.valid() {
		return fmt.Errorf("playbook: style %q is invalid (want scalp|swing|hold)", p.Style)
	}
	if p.Capital.Sign() <= 0 {
		return fmt.Errorf("playbook: capital must be > 0, got %s", p.Capital.String())
	}
	if err := fractionInUnitInterval("max_drawdown_pct", p.MaxDrawdownPct); err != nil {
		return err
	}
	if err := fractionInUnitInterval("max_risk_per_trade_pct", p.MaxRiskPerTradePct); err != nil {
		return err
	}
	if err := fractionInUnitInterval("max_daily_loss_pct", p.MaxDailyLossPct); err != nil {
		return err
	}
	// A daily-loss budget below the per-trade risk is contradictory: a single
	// allowed trade could breach the day's budget on entry.
	if p.MaxDailyLossPct.Cmp(p.MaxRiskPerTradePct) < 0 {
		return fmt.Errorf(
			"playbook: max_daily_loss_pct %s is smaller than max_risk_per_trade_pct %s — a single trade could breach the daily budget",
			p.MaxDailyLossPct.String(), p.MaxRiskPerTradePct.String())
	}
	if p.MaxGrossLeverage.Cmp(one) < 0 {
		return fmt.Errorf("playbook: max_gross_leverage must be >= 1, got %s", p.MaxGrossLeverage.String())
	}
	if p.MaxOpenPositions < 1 {
		return fmt.Errorf("playbook: max_open_positions must be >= 1, got %d", p.MaxOpenPositions)
	}
	// Contradiction guard: HODL with a very tight drawdown tolerance.
	if p.Style == Hodl {
		tightDrawdown := orders.MustParseDecimal("0.10")
		if p.MaxDrawdownPct.Cmp(tightDrawdown) < 0 {
			return fmt.Errorf(
				"playbook: a 'hold' (long-term) style with max_drawdown_pct %s (< 0.10) is contradictory — long-term holding rides out larger drawdowns; raise the drawdown tolerance or pick swing/scalp",
				p.MaxDrawdownPct.String())
		}
	}
	return nil
}

// fractionInUnitInterval validates that d is a fraction in (0, 1], returning a
// named, specific error otherwise. Used for all percentage knobs.
func fractionInUnitInterval(name string, d orders.Decimal) error {
	if d.Sign() <= 0 {
		return fmt.Errorf("playbook: %s must be > 0, got %s", name, d.String())
	}
	if d.Cmp(one) > 0 {
		return fmt.Errorf("playbook: %s must be <= 1 (a fraction, e.g. 0.20 for 20%%), got %s", name, d.String())
	}
	return nil
}

// trimmedSectors returns the owner's sectors with surrounding whitespace removed
// and empty entries dropped, preserving order. Free-form tags stay free-form, but
// blank/whitespace-only entries are never stored.
func trimmedSectors(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
