package playbook

import (
	"strings"
	"testing"

	"bucks/internal/orders"

	"gopkg.in/yaml.v3"
)

// fullAnswers returns a complete, valid, non-contradictory answer set for the
// default intake. Tests mutate a copy to exercise individual failure modes.
func fullAnswers() map[string]string {
	return map[string]string{
		KeyRiskTolerance: "moderate",
		KeyCapital:       "25000",
		KeyStyle:         "swing",
		KeySectors:       "tech, energy , crypto",
		KeyMaxDrawdown:   "0.20",
		KeyGoals:         "steady growth, beat the S&P",
		KeyMaxRiskTrade:  "0.01",
		KeyMaxDailyLoss:  "0.03",
		KeyMaxLeverage:   "2",
		KeyMaxOpenPos:    "8",
	}
}

func dec(t *testing.T, s string) orders.Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return d
}

// TestBuildPlaybook_FullAnswersValid proves a complete answer set produces a
// valid, correctly-typed Playbook with the owner's explicit knobs preserved.
func TestBuildPlaybook_FullAnswersValid(t *testing.T) {
	pb, err := BuildPlaybook(fullAnswers())
	if err != nil {
		t.Fatalf("BuildPlaybook full answers: unexpected error: %v", err)
	}
	if pb.RiskTolerance != Moderate {
		t.Errorf("RiskTolerance = %q, want moderate", pb.RiskTolerance)
	}
	if pb.Style != Swing {
		t.Errorf("Style = %q, want swing", pb.Style)
	}
	if pb.Capital.Cmp(dec(t, "25000")) != 0 {
		t.Errorf("Capital = %s, want 25000", pb.Capital.String())
	}
	if pb.MaxDrawdownPct.Cmp(dec(t, "0.20")) != 0 {
		t.Errorf("MaxDrawdownPct = %s, want 0.20", pb.MaxDrawdownPct.String())
	}
	if pb.MaxRiskPerTradePct.Cmp(dec(t, "0.01")) != 0 {
		t.Errorf("MaxRiskPerTradePct = %s, want 0.01", pb.MaxRiskPerTradePct.String())
	}
	if pb.MaxOpenPositions != 8 {
		t.Errorf("MaxOpenPositions = %d, want 8", pb.MaxOpenPositions)
	}
	// Sectors trimmed, blanks dropped, order preserved.
	wantSectors := []string{"tech", "energy", "crypto"}
	if len(pb.Sectors) != len(wantSectors) {
		t.Fatalf("Sectors = %v, want %v", pb.Sectors, wantSectors)
	}
	for i, s := range wantSectors {
		if pb.Sectors[i] != s {
			t.Errorf("Sectors[%d] = %q, want %q", i, pb.Sectors[i], s)
		}
	}
	if err := pb.Validate(); err != nil {
		t.Errorf("built playbook fails its own Validate(): %v", err)
	}
}

// TestBuildPlaybook_DefaultsFromTolerance proves blank optional knobs are filled
// from the risk-tolerance register (not left zero, which would fail validation).
func TestBuildPlaybook_DefaultsFromTolerance(t *testing.T) {
	a := fullAnswers()
	a[KeyRiskTolerance] = "conservative"
	delete(a, KeyMaxRiskTrade)
	delete(a, KeyMaxDailyLoss)
	delete(a, KeyMaxLeverage)
	delete(a, KeyMaxOpenPos)

	pb, err := BuildPlaybook(a)
	if err != nil {
		t.Fatalf("BuildPlaybook conservative defaults: %v", err)
	}
	if pb.MaxRiskPerTradePct.Cmp(dec(t, "0.005")) != 0 {
		t.Errorf("conservative MaxRiskPerTradePct = %s, want 0.005", pb.MaxRiskPerTradePct.String())
	}
	if pb.MaxDailyLossPct.Cmp(dec(t, "0.02")) != 0 {
		t.Errorf("conservative MaxDailyLossPct = %s, want 0.02", pb.MaxDailyLossPct.String())
	}
	if pb.MaxGrossLeverage.Cmp(dec(t, "1")) != 0 {
		t.Errorf("conservative MaxGrossLeverage = %s, want 1", pb.MaxGrossLeverage.String())
	}
	if pb.MaxOpenPositions != 5 {
		t.Errorf("conservative MaxOpenPositions = %d, want 5", pb.MaxOpenPositions)
	}
}

// TestBuildPlaybook_RejectsMissingRequired proves a missing required answer is
// rejected with a message that names the field.
func TestBuildPlaybook_RejectsMissingRequired(t *testing.T) {
	a := fullAnswers()
	delete(a, KeyCapital)
	_, err := BuildPlaybook(a)
	if err == nil {
		t.Fatal("expected error for missing capital, got nil")
	}
	if !strings.Contains(err.Error(), KeyCapital) {
		t.Errorf("error %q does not name the missing field %q", err.Error(), KeyCapital)
	}
}

// TestBuildPlaybook_RejectsBadEnum proves an out-of-set enum answer is rejected.
func TestBuildPlaybook_RejectsBadEnum(t *testing.T) {
	a := fullAnswers()
	a[KeyStyle] = "daytrade-yolo"
	_, err := BuildPlaybook(a)
	if err == nil {
		t.Fatal("expected error for invalid style enum, got nil")
	}
	if !strings.Contains(err.Error(), KeyStyle) {
		t.Errorf("error %q does not name the style field", err.Error())
	}
}

// TestBuildPlaybook_RejectsNonPositiveCapital proves capital must be > 0.
func TestBuildPlaybook_RejectsNonPositiveCapital(t *testing.T) {
	a := fullAnswers()
	a[KeyCapital] = "0"
	_, err := BuildPlaybook(a)
	if err == nil {
		t.Fatal("expected error for zero capital, got nil")
	}
	if !strings.Contains(err.Error(), "capital") {
		t.Errorf("error %q does not mention capital", err.Error())
	}
}

// TestBuildPlaybook_RejectsDrawdownOutOfRange proves drawdown must be in (0,1].
func TestBuildPlaybook_RejectsDrawdownOutOfRange(t *testing.T) {
	for _, bad := range []string{"0", "1.5", "-0.1"} {
		a := fullAnswers()
		a[KeyMaxDrawdown] = bad
		_, err := BuildPlaybook(a)
		if err == nil {
			t.Fatalf("expected error for drawdown %q, got nil", bad)
		}
		if !strings.Contains(err.Error(), "max_drawdown_pct") {
			t.Errorf("drawdown %q: error %q does not name the field", bad, err.Error())
		}
	}
}

// TestBuildPlaybook_RejectsContradictoryHodl proves the HODL + tiny-drawdown
// contradiction is flagged with a specific reason (not silently accepted).
func TestBuildPlaybook_RejectsContradictoryHodl(t *testing.T) {
	a := fullAnswers()
	a[KeyStyle] = "hodl"
	a[KeyMaxDrawdown] = "0.01" // 1% drawdown with a buy-and-hold style — contradictory
	_, err := BuildPlaybook(a)
	if err == nil {
		t.Fatal("expected contradiction error for hodl+1% drawdown, got nil")
	}
	// The owner-facing message uses the plain-English "hold" (the legacy "hodl" input is
	// still accepted as an alias, but the explanation never shows the misspelling).
	if !strings.Contains(err.Error(), "hold") || !strings.Contains(err.Error(), "contradictory") {
		t.Errorf("error %q does not explain the hold/drawdown contradiction", err.Error())
	}
}

// TestBuildPlaybook_RejectsDailyLossBelowPerTrade proves the daily-loss <
// per-trade-risk contradiction is caught.
func TestBuildPlaybook_RejectsDailyLossBelowPerTrade(t *testing.T) {
	a := fullAnswers()
	a[KeyMaxRiskTrade] = "0.05"
	a[KeyMaxDailyLoss] = "0.02" // smaller than a single trade's risk — contradictory
	_, err := BuildPlaybook(a)
	if err == nil {
		t.Fatal("expected error for daily-loss < per-trade-risk, got nil")
	}
	if !strings.Contains(err.Error(), "max_daily_loss_pct") {
		t.Errorf("error %q does not name the daily-loss field", err.Error())
	}
}

// TestPlaybook_YAMLRoundTrip proves marshal→unmarshal yields an equal Playbook,
// with all Decimal money/fraction fields exact across the round trip.
func TestPlaybook_YAMLRoundTrip(t *testing.T) {
	orig, err := BuildPlaybook(fullAnswers())
	if err != nil {
		t.Fatalf("BuildPlaybook: %v", err)
	}
	blob, err := yaml.Marshal(orig)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var got Playbook
	if err := yaml.Unmarshal(blob, &got); err != nil {
		t.Fatalf("yaml.Unmarshal: %v\nyaml was:\n%s", err, blob)
	}
	if got.RiskTolerance != orig.RiskTolerance {
		t.Errorf("RiskTolerance round trip: got %q want %q", got.RiskTolerance, orig.RiskTolerance)
	}
	if got.Style != orig.Style {
		t.Errorf("Style round trip: got %q want %q", got.Style, orig.Style)
	}
	if got.Capital.Cmp(orig.Capital) != 0 {
		t.Errorf("Capital round trip: got %s want %s", got.Capital.String(), orig.Capital.String())
	}
	if got.MaxDrawdownPct.Cmp(orig.MaxDrawdownPct) != 0 {
		t.Errorf("MaxDrawdownPct round trip: got %s want %s", got.MaxDrawdownPct.String(), orig.MaxDrawdownPct.String())
	}
	if got.MaxRiskPerTradePct.Cmp(orig.MaxRiskPerTradePct) != 0 {
		t.Errorf("MaxRiskPerTradePct round trip: got %s want %s", got.MaxRiskPerTradePct.String(), orig.MaxRiskPerTradePct.String())
	}
	if got.MaxDailyLossPct.Cmp(orig.MaxDailyLossPct) != 0 {
		t.Errorf("MaxDailyLossPct round trip: got %s want %s", got.MaxDailyLossPct.String(), orig.MaxDailyLossPct.String())
	}
	if got.MaxGrossLeverage.Cmp(orig.MaxGrossLeverage) != 0 {
		t.Errorf("MaxGrossLeverage round trip: got %s want %s", got.MaxGrossLeverage.String(), orig.MaxGrossLeverage.String())
	}
	if got.MaxOpenPositions != orig.MaxOpenPositions {
		t.Errorf("MaxOpenPositions round trip: got %d want %d", got.MaxOpenPositions, orig.MaxOpenPositions)
	}
	if strings.Join(got.Sectors, ",") != strings.Join(orig.Sectors, ",") {
		t.Errorf("Sectors round trip: got %v want %v", got.Sectors, orig.Sectors)
	}
	// The re-read playbook must still pass validation.
	if err := got.Validate(); err != nil {
		t.Errorf("round-tripped playbook fails Validate(): %v", err)
	}
}

// TestPlaybook_ToRiskConfig proves the playbook maps onto risk.Config correctly:
// the knobs flow through, RequireStop is forced on, and concentration follows the
// tolerance register. The risk engine is then proven to enforce the mapped pct.
func TestPlaybook_ToRiskConfig(t *testing.T) {
	pb, err := BuildPlaybook(fullAnswers()) // moderate
	if err != nil {
		t.Fatalf("BuildPlaybook: %v", err)
	}
	cfg := pb.ToRiskConfig()
	if cfg.MaxRiskPerTradePct.Cmp(pb.MaxRiskPerTradePct) != 0 {
		t.Errorf("MaxRiskPerTradePct not mapped: got %s want %s", cfg.MaxRiskPerTradePct.String(), pb.MaxRiskPerTradePct.String())
	}
	if cfg.MaxDailyLossPct.Cmp(pb.MaxDailyLossPct) != 0 {
		t.Errorf("MaxDailyLossPct not mapped: got %s want %s", cfg.MaxDailyLossPct.String(), pb.MaxDailyLossPct.String())
	}
	if cfg.MaxGrossLeverage.Cmp(pb.MaxGrossLeverage) != 0 {
		t.Errorf("MaxGrossLeverage not mapped: got %s want %s", cfg.MaxGrossLeverage.String(), pb.MaxGrossLeverage.String())
	}
	if cfg.MaxOpenPositions != pb.MaxOpenPositions {
		t.Errorf("MaxOpenPositions not mapped: got %d want %d", cfg.MaxOpenPositions, pb.MaxOpenPositions)
	}
	// BUG-1: the owner's drawdown tolerance is a real, mapped limit (not discarded).
	if cfg.MaxDrawdownPct.Cmp(pb.MaxDrawdownPct) != 0 {
		t.Errorf("MaxDrawdownPct not mapped: got %s want %s", cfg.MaxDrawdownPct.String(), pb.MaxDrawdownPct.String())
	}
	if !cfg.RequireStop {
		t.Error("ToRiskConfig must force RequireStop on (no naked positions)")
	}
	// Moderate -> 0.25 concentration.
	if cfg.MaxConcentrationPct.Cmp(dec(t, "0.25")) != 0 {
		t.Errorf("moderate MaxConcentrationPct = %s, want 0.25", cfg.MaxConcentrationPct.String())
	}

	// Conservative -> tighter concentration (0.15).
	cpb := pb
	cpb.RiskTolerance = Conservative
	if c := cpb.ToRiskConfig().MaxConcentrationPct; c.Cmp(dec(t, "0.15")) != 0 {
		t.Errorf("conservative MaxConcentrationPct = %s, want 0.15", c.String())
	}
	// Aggressive -> looser concentration (0.35).
	apb := pb
	apb.RiskTolerance = Aggressive
	if c := apb.ToRiskConfig().MaxConcentrationPct; c.Cmp(dec(t, "0.35")) != 0 {
		t.Errorf("aggressive MaxConcentrationPct = %s, want 0.35", c.String())
	}
}

// TestQuestion_ValidateInt proves the int-typed question rejects non-integers.
func TestQuestion_ValidateInt(t *testing.T) {
	q := Question{Id: KeyMaxOpenPos, Prompt: "n?", Type: TypeInt, Required: false}
	if err := q.Validate("12"); err != nil {
		t.Errorf("Validate(12) int: unexpected error %v", err)
	}
	if err := q.Validate("3.5"); err == nil {
		t.Error("Validate(3.5) int: expected error, got nil")
	}
	if err := q.Validate(""); err != nil {
		t.Errorf("Validate(empty) optional int: unexpected error %v", err)
	}
}

// TestParseInt covers the BUG-4 fix and the normal paths: a bare sign with no
// digits is an error (not silently 0), while signed and unsigned integers parse.
func TestParseInt(t *testing.T) {
	ok := []struct {
		in   string
		want int
	}{
		{"0", 0}, {"12", 12}, {"+7", 7}, {"-3", -3}, {"  42  ", 42},
	}
	for _, tc := range ok {
		got, err := parseInt(tc.in)
		if err != nil {
			t.Errorf("parseInt(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseInt(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"+", "-", "", "1.5", "abc", "1a", "- 3"} {
		if _, err := parseInt(bad); err == nil {
			t.Errorf("parseInt(%q): expected error, got nil", bad)
		}
	}
}
