package playbook

import (
	"fmt"
	"strings"

	"bucks/internal/orders"
)

// QuestionType classifies how an answer is parsed and validated.
type QuestionType int

const (
	// TypeEnum — the answer must be one of Question.Options (case-insensitive).
	TypeEnum QuestionType = iota
	// TypeDecimal — the answer parses to an orders.Decimal (money or fraction).
	TypeDecimal
	// TypeInt — the answer parses to an integer.
	TypeInt
	// TypeText — free text (optional unless Required).
	TypeText
	// TypeList — a comma-separated list of free-form tags (sectors).
	TypeList
)

// Question is one leading prompt in the intake. Id is the answer key, Prompt is
// the plain-English question shown to the owner, Type drives parsing/validation,
// Options enumerates the allowed values for an enum, and Required marks whether a
// blank answer is acceptable. The Question set is the fixed, auditable script the
// onboarding wizard (TUI) walks — there is no hidden question.
type Question struct {
	Id       string
	Prompt   string
	Type     QuestionType
	Options  []string // for TypeEnum
	Required bool
}

// Validate checks a single raw answer against this question's type/options,
// returning a clear error if the answer is malformed or (when required) missing.
// It does NOT cross-check against other answers — that is Playbook.Validate's job.
func (q Question) Validate(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if q.Required {
			return fmt.Errorf("playbook: answer for %q (%s) is required", q.Id, q.Prompt)
		}
		return nil
	}
	switch q.Type {
	case TypeEnum:
		for _, opt := range q.Options {
			if strings.EqualFold(raw, opt) {
				return nil
			}
		}
		return fmt.Errorf("playbook: answer %q for %q is not one of %v", raw, q.Id, q.Options)
	case TypeDecimal:
		if _, err := orders.ParseDecimal(raw); err != nil {
			return fmt.Errorf("playbook: answer %q for %q is not a valid number: %w", raw, q.Id, err)
		}
	case TypeInt:
		if _, err := parseInt(raw); err != nil {
			return fmt.Errorf("playbook: answer %q for %q is not a valid integer: %w", raw, q.Id, err)
		}
	case TypeText, TypeList:
		// any non-empty text is structurally fine
	}
	return nil
}

// Answer-key constants — the stable ids the intake and BuildPlaybook agree on.
const (
	KeyRiskTolerance = "risk_tolerance"
	KeyCapital       = "capital"
	KeyStyle         = "style"
	KeySectors       = "sectors"
	KeyMaxDrawdown   = "max_drawdown_pct"
	KeyGoals         = "goals"
	KeyMaxRiskTrade  = "max_risk_per_trade_pct"
	KeyMaxDailyLoss  = "max_daily_loss_pct"
	KeyMaxLeverage   = "max_gross_leverage"
	KeyMaxOpenPos    = "max_open_positions"
)

// Intake is the fixed set of leading questions that build a Playbook. It is a
// value type (no hidden state) so the same script is reproducible everywhere.
type Intake struct {
	Questions []Question
}

// DefaultIntake returns BUCKS's standard onboarding script — the leading
// questions from spec §4.9 step 5 (risk tolerance, capital, style, sectors, max
// drawdown, goals) plus the explicit risk knobs that bind to the safety layer.
func DefaultIntake() Intake {
	return Intake{Questions: []Question{
		{Id: KeyRiskTolerance, Prompt: "How much risk can you stomach?", Type: TypeEnum,
			Options: []string{string(Conservative), string(Moderate), string(Aggressive)}, Required: true},
		{Id: KeyCapital, Prompt: "How much capital are you funding BUCKS with (in dollars)?", Type: TypeDecimal, Required: true},
		{Id: KeyStyle, Prompt: "What's your trading style?", Type: TypeEnum,
			Options: []string{string(Scalp), string(Swing), string(Hodl)}, Required: true},
		{Id: KeySectors, Prompt: "Which sectors/markets should BUCKS focus on (comma-separated)?", Type: TypeList, Required: false},
		{Id: KeyMaxDrawdown, Prompt: "Worst peak-to-trough loss you'll tolerate, as a fraction (e.g. 0.20 for 20%)?", Type: TypeDecimal, Required: true},
		{Id: KeyGoals, Prompt: "What's your goal in your own words?", Type: TypeText, Required: false},
		{Id: KeyMaxRiskTrade, Prompt: "Max fraction of capital to risk on one trade (e.g. 0.01 for 1%)?", Type: TypeDecimal, Required: false},
		{Id: KeyMaxDailyLoss, Prompt: "Max fraction of capital to lose in a day before halting (e.g. 0.03)?", Type: TypeDecimal, Required: false},
		{Id: KeyMaxLeverage, Prompt: "Max gross leverage (1 = no leverage, 2, 3...)?", Type: TypeDecimal, Required: false},
		{Id: KeyMaxOpenPos, Prompt: "Max number of positions open at once?", Type: TypeInt, Required: false},
	}}
}

// toleranceDefaults are the per-register defaults for the risk knobs the owner
// may leave blank. Conservative trades small with a low daily-loss budget; an
// aggressive register risks more per trade and tolerates a wider daily loss. The
// risk engine still clamps per-trade risk to its 2% hard cap regardless.
type toleranceDefaults struct {
	riskPerTrade string
	dailyLoss    string
	leverage     string
	openPos      int
}

func defaultsFor(t RiskTolerance) toleranceDefaults {
	switch t {
	case Conservative:
		return toleranceDefaults{riskPerTrade: "0.005", dailyLoss: "0.02", leverage: "1", openPos: 5}
	case Aggressive:
		return toleranceDefaults{riskPerTrade: "0.02", dailyLoss: "0.05", leverage: "3", openPos: 15}
	default: // Moderate
		return toleranceDefaults{riskPerTrade: "0.01", dailyLoss: "0.03", leverage: "2", openPos: 10}
	}
}

// BuildPlaybook assembles a Playbook from a map of answer-id -> raw answer string,
// applying per-tolerance defaults for any optional risk knob the owner left blank,
// then validating the result. It rejects incomplete or contradictory input with
// the specific reason (no silent default-to-pass): a missing required answer, an
// unparseable number, an unknown enum value, or a contradiction such as a HODL
// style with a sub-10% drawdown tolerance all return a clear error.
func BuildPlaybook(answers map[string]string) (Playbook, error) {
	intake := DefaultIntake()

	// 1. Per-question structural validation (required present, parseable, in enum).
	for _, q := range intake.Questions {
		if err := q.Validate(answers[q.Id]); err != nil {
			return Playbook{}, err
		}
	}

	// 2. Risk tolerance first — it seeds the defaults for blank knobs.
	tol := RiskTolerance(strings.ToLower(strings.TrimSpace(answers[KeyRiskTolerance])))
	if !tol.valid() {
		// Defensive: Validate above already enforces this, but never assume.
		return Playbook{}, fmt.Errorf("playbook: risk_tolerance %q is invalid", answers[KeyRiskTolerance])
	}
	defs := defaultsFor(tol)

	pb := Playbook{
		RiskTolerance: tol,
		Style:         Style(strings.ToLower(strings.TrimSpace(answers[KeyStyle]))),
		Sectors:       trimmedSectors(strings.Split(answers[KeySectors], ",")),
		Goals:         strings.TrimSpace(answers[KeyGoals]),
	}

	// 3. Required decimals — present-and-parseable was checked; parse exactly.
	var err error
	if pb.Capital, err = orders.ParseDecimal(strings.TrimSpace(answers[KeyCapital])); err != nil {
		return Playbook{}, fmt.Errorf("playbook: capital parse: %w", err)
	}
	if pb.MaxDrawdownPct, err = orders.ParseDecimal(strings.TrimSpace(answers[KeyMaxDrawdown])); err != nil {
		return Playbook{}, fmt.Errorf("playbook: max_drawdown_pct parse: %w", err)
	}

	// 4. Optional risk knobs — owner's value if present, else the register default.
	if pb.MaxRiskPerTradePct, err = decimalOrDefault(answers[KeyMaxRiskTrade], defs.riskPerTrade); err != nil {
		return Playbook{}, fmt.Errorf("playbook: max_risk_per_trade_pct parse: %w", err)
	}
	if pb.MaxDailyLossPct, err = decimalOrDefault(answers[KeyMaxDailyLoss], defs.dailyLoss); err != nil {
		return Playbook{}, fmt.Errorf("playbook: max_daily_loss_pct parse: %w", err)
	}
	if pb.MaxGrossLeverage, err = decimalOrDefault(answers[KeyMaxLeverage], defs.leverage); err != nil {
		return Playbook{}, fmt.Errorf("playbook: max_gross_leverage parse: %w", err)
	}
	if raw := strings.TrimSpace(answers[KeyMaxOpenPos]); raw == "" {
		pb.MaxOpenPositions = defs.openPos
	} else if pb.MaxOpenPositions, err = parseInt(raw); err != nil {
		return Playbook{}, fmt.Errorf("playbook: max_open_positions parse: %w", err)
	}

	// 5. Whole-playbook validation: completeness + no contradictions.
	if err := pb.Validate(); err != nil {
		return Playbook{}, err
	}
	return pb, nil
}

// decimalOrDefault parses raw if non-blank, else parses the (constant, known-good)
// default string. The default is parsed via MustParseDecimal because it is a
// compile-time literal, never owner input.
func decimalOrDefault(raw, def string) (orders.Decimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return orders.MustParseDecimal(def), nil
	}
	return orders.ParseDecimal(raw)
}

// parseInt parses a base-10 integer from trimmed input. Kept local so the intake
// has one integer-parsing rule (no float64, no silent truncation).
func parseInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	var n int
	var sign = 1
	var sawDigit bool
	if raw == "" {
		return 0, fmt.Errorf("empty integer")
	}
	for i, r := range raw {
		if i == 0 && (r == '+' || r == '-') {
			if r == '-' {
				sign = -1
			}
			continue
		}
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid integer %q", raw)
		}
		sawDigit = true
		n = n*10 + int(r-'0')
	}
	// A bare sign ("+"/"-") with no digits is not an integer.
	if !sawDigit {
		return 0, fmt.Errorf("invalid integer %q: sign with no digits", raw)
	}
	return sign * n, nil
}
